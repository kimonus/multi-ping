package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"math"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kimonus/multi-ping/internal/ping"
)

// record is one finished probe.
type record struct {
	at      time.Time // when it was sent
	panel   int       // 1-based panel number, 0 if unknown
	ifName  string
	target  net.IP
	gateway bool // probe to the interface's gateway, not the destination
	seq     int
	rtt     time.Duration
	err     error
}

func (r record) kind() string {
	if r.gateway {
		return "gateway"
	}
	return "destination"
}

func (r record) status() string {
	switch {
	case r.err == nil:
		return "ok"
	case errors.Is(r.err, ping.ErrTimeout):
		return "timeout"
	}
	return "error"
}

// The JSON Lines format: one JSON object per line, told apart by "type".
// A dump holds one session line, a summary line per panel and target, then
// the probes in time order. The streaming log holds probe lines only.
type (
	sessionLine struct {
		Type       string `json:"type"` // "session"
		Format     int    `json:"format"`
		Time       string `json:"time"`
		TimeoutMS  int64  `json:"timeout_ms"`
		SizeBytes  int    `json:"size_bytes"`
		IntervalMS int64  `json:"interval_ms"`
	}
	summaryLine struct {
		Type      string   `json:"type"` // "summary"
		Panel     int      `json:"panel"`
		Interface string   `json:"interface"`
		Source    string   `json:"source,omitempty"`
		Kind      string   `json:"kind"`
		Target    string   `json:"target"`
		Sent      int      `json:"sent"`
		Recv      int      `json:"recv"`
		Lost      int      `json:"lost"`
		LossPct   float64  `json:"loss_pct"`
		MinMS     *float64 `json:"min_ms,omitempty"`
		AvgMS     *float64 `json:"avg_ms,omitempty"`
		MaxMS     *float64 `json:"max_ms,omitempty"`
		JitterMS  *float64 `json:"jitter_ms,omitempty"`
	}
	probeLine struct {
		Type      string   `json:"type"` // "probe"
		Time      string   `json:"time"`
		Panel     int      `json:"panel,omitempty"`
		Interface string   `json:"interface"`
		Kind      string   `json:"kind"`
		Target    string   `json:"target"`
		Seq       int      `json:"seq"`
		Status    string   `json:"status"` // ok, timeout, error
		RTTMS     *float64 `json:"rtt_ms,omitempty"`
		Error     string   `json:"error,omitempty"`
	}
)

const jsonlFormat = 1

// ms converts to milliseconds with microsecond precision.
func ms(d time.Duration) *float64 {
	v := math.Round(float64(d)/float64(time.Microsecond)) / 1000
	return &v
}

func (r record) line() probeLine {
	l := probeLine{Type: "probe", Time: r.at.Format(time.RFC3339Nano), Panel: r.panel,
		Interface: r.ifName, Kind: r.kind(), Target: r.target.String(), Seq: r.seq, Status: r.status()}
	switch l.Status {
	case "ok":
		l.RTTMS = ms(r.rtt)
	case "error":
		l.Error = r.err.Error()
	}
	return l
}

// logger appends finished probes to a file as they complete: JSON Lines when
// the name ends in .jsonl or .ndjson, CSV otherwise.
type logger struct {
	path string
	f    *os.File
	csv  *csv.Writer // nil for JSON Lines
}

func isJSONL(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".jsonl" || ext == ".ndjson"
}

func openLog(path string) (*logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &logger{path: path, f: f}
	if !isJSONL(path) {
		l.csv = csv.NewWriter(f)
		if st, err := f.Stat(); err == nil && st.Size() == 0 {
			l.csv.Write([]string{"time", "interface", "kind", "target", "seq", "status", "rtt_ms", "error"})
		}
	}
	return l, nil
}

func (l *logger) write(r record) error {
	if l.csv == nil {
		b, err := json.Marshal(r.line())
		if err != nil {
			return err
		}
		_, err = l.f.Write(append(b, '\n'))
		return err
	}
	rtt, msg := "", ""
	switch r.status() {
	case "ok":
		rtt = strconv.FormatFloat(*ms(r.rtt), 'f', 3, 64)
	case "error":
		msg = r.err.Error()
	}
	l.csv.Write([]string{r.at.Format(time.RFC3339Nano), r.ifName, r.kind(), r.target.String(), strconv.Itoa(r.seq), r.status(), rtt, msg})
	l.csv.Flush()
	return l.csv.Error()
}

func (l *logger) close() {
	if l.csv != nil {
		l.csv.Flush()
	}
	l.f.Close()
}

// dump writes what the panels currently hold to path as JSON Lines and
// returns the number of probes written. Probes still waiting for a reply are
// left out; each panel holds at most maxRows probes per target.
func (m *model) dump(path string, now time.Time) (int, error) {
	lines := []any{sessionLine{Type: "session", Format: jsonlFormat, Time: now.Format(time.RFC3339Nano),
		TimeoutMS: m.timeout.Milliseconds(), SizeBytes: m.size, IntervalMS: m.interval.Milliseconds()}}
	var probes []record
	for i, p := range m.panels {
		for _, t := range []struct {
			s       *series
			target  net.IP
			gateway bool
		}{{&p.series, m.target(p), false}, {&p.gw, p.ifc.Gateway, true}} {
			if t.s.sent == 0 || t.target == nil {
				continue
			}
			base := record{panel: i + 1, ifName: p.ifc.Name, target: t.target, gateway: t.gateway}
			sum := summaryLine{Type: "summary", Panel: base.panel, Interface: base.ifName, Kind: base.kind(),
				Target: t.target.String(), Sent: t.s.sent, Recv: t.s.recv, Lost: t.s.lost,
				LossPct: math.Round(t.s.lossPct()*100) / 100}
			if src := p.ifc.Addr(t.target.To4() == nil); src != nil {
				sum.Source = src.String()
			}
			if t.s.recv > 0 {
				sum.MinMS, sum.AvgMS, sum.MaxMS = ms(t.s.min), ms(t.s.avg()), ms(t.s.max)
			}
			if t.s.recv > 1 {
				sum.JitterMS = ms(t.s.jitterSum / time.Duration(t.s.recv-1))
			}
			lines = append(lines, sum)
			for _, r := range t.s.rows {
				rec := base
				rec.at, rec.seq, rec.rtt = r.at, r.seq, r.rtt
				switch r.state {
				case rowPending:
					continue
				case rowTimeout:
					rec.err = ping.ErrTimeout
				case rowError:
					rec.err = errors.New(r.err)
				}
				probes = append(probes, rec)
			}
		}
	}
	slices.SortStableFunc(probes, func(a, b record) int { return a.at.Compare(b.at) })
	for _, r := range probes {
		lines = append(lines, r.line())
	}

	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	return len(probes), f.Close()
}
