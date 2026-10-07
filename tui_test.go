package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kimonus/multi-ping/internal/ping"
)

var testIfaces = []ping.Interface{
	{Name: "eth0", Index: 1, IP: net.IPv4(10, 0, 0, 10).To4(), Gateway: net.IPv4(10, 0, 0, 1).To4()},
	{Name: "wlan0", Index: 2, IP: net.IPv4(192, 168, 1, 20).To4(), IP6: net.ParseIP("2001:db8::20")},
	{Name: "usb0", Index: 3, IP: net.IPv4(172, 16, 0, 2).To4()},
}

func testModel() *model {
	return newModel(slices3(), []int{0, 1}, options{timeoutMS: 1000, size: 56, intervalMS: 1000, gateway: true})
}

func slices3() []ping.Interface { return append([]ping.Interface(nil), testIfaces...) }

func key(m *model, keys ...string) {
	for _, k := range keys {
		msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace}
		}
		m.Update(msg)
	}
}

func TestSeriesStats(t *testing.T) {
	s := &series{}
	for seq := 1; seq <= 4; seq++ {
		s.send(seq, time.Now())
	}
	s.result(1, 10*time.Millisecond, nil)
	s.result(2, 0, ping.ErrTimeout)
	s.result(3, 30*time.Millisecond, nil)
	s.result(4, 0, errors.New("network unreachable"))

	if s.sent != 4 || s.recv != 2 || s.lost != 2 {
		t.Fatalf("sent/recv/lost = %d/%d/%d, want 4/2/2", s.sent, s.recv, s.lost)
	}
	if s.min != 10*time.Millisecond || s.max != 30*time.Millisecond || s.sum != 40*time.Millisecond {
		t.Fatalf("min/max/sum = %v/%v/%v", s.min, s.max, s.sum)
	}
	if s.jitterSum != 20*time.Millisecond {
		t.Fatalf("jitterSum = %v, want 20ms", s.jitterSum)
	}
	if s.rows[1].state != rowTimeout || s.rows[3].err != "network unreachable" {
		t.Fatalf("rows not updated: %+v", s.rows)
	}
}

func TestKeepLimit(t *testing.T) {
	defer func(old int) { maxRows = old }(maxRows)
	now := time.Now()
	fill := func() *series {
		s := &series{}
		for seq := 1; seq <= 10; seq++ {
			s.send(seq, now)
			s.result(seq, time.Millisecond, nil)
		}
		return s
	}
	maxRows = 4
	if s := fill(); len(s.rows) != 4 || s.rows[0].seq != 7 || s.recv != 10 {
		t.Fatalf("kept %d rows from seq %d, recv %d; want the last 4 and all 10 counted", len(s.rows), s.rows[0].seq, s.recv)
	}
	maxRows = 0
	if s := fill(); len(s.rows) != 10 {
		t.Fatalf("kept %d rows with no limit, want 10", len(s.rows))
	}
}

func TestWindow(t *testing.T) {
	now := time.Now()
	s := &series{}
	// 100 old probes at 5 ms, then 20 recent ones at 1..18 ms with two lost.
	for seq := 1; seq <= 120; seq++ {
		switch {
		case seq <= 100:
			s.send(seq, now.Add(-10*time.Minute))
			s.result(seq, 5*time.Millisecond, nil)
		case seq > 118:
			s.send(seq, now)
			s.result(seq, 0, ping.ErrTimeout)
		default:
			s.send(seq, now)
			s.result(seq, time.Duration(seq-100)*time.Millisecond, nil)
		}
	}
	w := s.window(now.Add(time.Second), recentWindow)
	if w.recv != 18 || w.lost != 2 {
		t.Fatalf("recv/lost = %d/%d, want 18/2", w.recv, w.lost)
	}
	if w.avg != 9500*time.Microsecond || w.p95 != 18*time.Millisecond {
		t.Fatalf("avg/p95 = %v/%v, want 9.5ms/18ms", w.avg, w.p95)
	}
	if w := (&series{}).window(now, recentWindow); w != (windowStats{}) {
		t.Fatalf("empty window = %+v", w)
	}
}

func TestStaleResultIgnored(t *testing.T) {
	m := testModel()
	p := m.panels[0]
	p.send(1, time.Now())
	old := p.epoch
	p.reset()
	m.Update(resultMsg{p: p, epoch: old, rec: record{seq: 1, rtt: time.Millisecond}})
	if p.recv != 0 {
		t.Fatal("result from before the reset was counted")
	}
}

func TestGatewayResultsKeptApart(t *testing.T) {
	m := testModel()
	p := m.panels[0]
	p.send(1, time.Now())
	p.gw.send(1, time.Now())
	m.Update(resultMsg{p: p, epoch: p.epoch, rec: record{seq: 1, rtt: 40 * time.Millisecond}})
	m.Update(resultMsg{p: p, epoch: p.epoch, rec: record{seq: 1, rtt: time.Millisecond, gateway: true}})
	if p.avg() != 40*time.Millisecond || p.gw.avg() != time.Millisecond {
		t.Fatalf("dest avg %v, gateway avg %v", p.avg(), p.gw.avg())
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if view := m.View(); !strings.Contains(view, "gateway 10.0.0.1  avg 1.00  max 1.00 ms  lost 0") {
		t.Errorf("gateway line missing:\n%s", view)
	}
}

func TestInterfaceRefresh(t *testing.T) {
	m := testModel()
	p := m.panels[1] // wlan0

	// wlan0 disappears: the panel keeps its choice but is marked down.
	// (Both panels go down here, so round() below never opens a socket.)
	m.Update(ifacesMsg(testIfaces[2:]))
	if !p.down || p.ifc.Name != "wlan0" {
		t.Fatalf("down=%v ifc=%s, want down wlan0", p.down, p.ifc.Name)
	}
	m.dst, m.gen = net.IPv4(8, 8, 8, 8).To4(), 1
	for _, cmd := range batch(m.round()) {
		if r, ok := cmd().(resultMsg); ok && r.p == p {
			m.Update(r)
		}
	}
	if p.lost != 1 || p.rows[0].err != errIfaceDown.Error() {
		t.Fatalf("probe on a down interface: lost=%d rows=%+v", p.lost, p.rows)
	}

	// It comes back with a new address.
	back := slices3()
	back[1].IP = net.IPv4(192, 168, 1, 99).To4()
	m.Update(ifacesMsg(back))
	if p.down || !p.ifc.IP.Equal(back[1].IP) {
		t.Fatalf("down=%v ip=%s after the interface returned", p.down, p.ifc.IP)
	}

	// A failed refresh keeps the old list.
	m.Update(ifacesMsg(nil))
	if len(m.ifaces) != 3 {
		t.Fatalf("list dropped after a failed refresh")
	}

	// ←/→ on a panel whose interface is gone picks a live one.
	m.Update(ifacesMsg(testIfaces[2:]))
	m.focus = numFields + 1
	key(m, "right")
	if p.down || p.ifc.Name != "usb0" {
		t.Fatalf("down=%v ifc=%s, want usb0", p.down, p.ifc.Name)
	}
}

// batch unpacks the commands of a tea.Batch, skipping the timer.
func batch(cmd tea.Cmd) []tea.Cmd {
	var out []tea.Cmd
	for _, c := range cmd().(tea.BatchMsg)[1:] {
		out = append(out, c)
	}
	return out
}

func TestDropDown(t *testing.T) {
	m := testModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.focus = numFields // first panel, on eth0
	key(m, " ")
	if m.menu != 0 || m.menuSel != 0 {
		t.Fatalf("menu=%d sel=%d after Space", m.menu, m.menuSel)
	}
	if view := m.View(); !strings.Contains(view, "▸ eth0 (10.0.0.10)") || !strings.Contains(view, "  usb0 (172.16.0.2)") {
		t.Errorf("open list not drawn:\n%s", view)
	}
	key(m, "down", "down", "enter")
	if m.menu != -1 || m.panels[0].ifc.Name != "usb0" {
		t.Fatalf("menu=%d ifc=%s, want closed on usb0", m.menu, m.panels[0].ifc.Name)
	}
	if m.running {
		t.Fatal("Enter in the list started pinging")
	}
	key(m, " ", "down", "esc")
	if m.panels[0].ifc.Name != "usb0" {
		t.Fatal("Esc changed the interface")
	}
	key(m, "right")
	if m.panels[0].ifc.Name != "eth0" {
		t.Fatalf("→ went to %s, want wrap to eth0", m.panels[0].ifc.Name)
	}
}

func TestFamilyLabel(t *testing.T) {
	m := testModel()
	m.focus = numFields
	key(m, "v")
	if !m.prefer6 {
		t.Fatal("v did not toggle the family")
	}
	if got := ifaceLabel(testIfaces[0], m.v6(m.panels[0])); got != "eth0 (no IPv6)" {
		t.Errorf("label = %q", got)
	}
	if got := ifaceLabel(testIfaces[1], m.v6(m.panels[1])); got != "wlan0 (2001:db8::20)" {
		t.Errorf("label = %q", got)
	}
}

func TestPanelDestination(t *testing.T) {
	m := testModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.fields[fDest] = "8.8.8.8"
	p0, p1 := m.panels[0], m.panels[1]
	m.focus = numFields + 1

	// Typing q and r in the edit box must not quit or reset.
	key(m, "d", "q", "r")
	if m.editing != 1 || m.editBuf != "qr" {
		t.Fatalf("editing=%d buf=%q", m.editing, m.editBuf)
	}
	key(m, "esc")
	if p1.dest != "" {
		t.Fatal("Esc kept the typed destination")
	}
	key(m, "d", "10.0.0.1", "enter")
	if p1.dest != "10.0.0.1" || m.editing != -1 {
		t.Fatalf("dest=%q editing=%d", p1.dest, m.editing)
	}

	// Start resolves the shared destination and the panel's own one.
	msg := m.start()().(resolvedMsg)
	m.Update(msg)
	if !m.running || m.target(p0).String() != "8.8.8.8" || m.target(p1).String() != "10.0.0.1" {
		t.Fatalf("running=%v targets %v / %v", m.running, m.target(p0), m.target(p1))
	}
	view := m.View()
	for _, want := range []string{"→ 8.8.8.8", "→ 10.0.0.1", "Δ vs eth0 │ wlan0→10.0.0.1:"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}

	// Changing it while running resolves just that panel and resets its stats.
	p1.sent = 5
	epoch := p1.epoch
	m.editing, m.editBuf = 1, "10.0.0.2"
	cmd := m.onEditKey(tea.KeyMsg{Type: tea.KeyEnter})
	if p1.sent != 0 || p1.epoch == epoch || m.target(p1) != nil {
		t.Fatalf("panel not reset: sent=%d target=%v", p1.sent, m.target(p1))
	}
	m.Update(cmd())
	if m.target(p1).String() != "10.0.0.2" || !m.running {
		t.Fatalf("target=%v running=%v", m.target(p1), m.running)
	}

	// Emptying it returns the panel to the shared destination.
	m.editing, m.editBuf = 1, ""
	m.onEditKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.target(p1).String() != "8.8.8.8" {
		t.Fatalf("target=%v, want shared", m.target(p1))
	}

	// A start with no shared destination works only if no panel needs it.
	m.fields[fDest] = ""
	if m.start() != nil || m.status == "" {
		t.Fatal("started without a destination")
	}
	p0.dest, p1.dest = "10.0.0.1", "10.0.0.2"
	if m.start() == nil {
		t.Fatal("did not start although every panel has its own destination")
	}
}

func TestAddRemovePanels(t *testing.T) {
	m := testModel()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.dst, m.running = net.IPv4(8, 8, 8, 8).To4(), true
	m.focus = fTimeout
	key(m, "+")
	if len(m.panels) != 3 || m.panels[2].ifc.Name != "usb0" || m.focus != numFields+2 {
		t.Fatalf("panels=%d ifc=%s focus=%d", len(m.panels), m.panels[2].ifc.Name, m.focus)
	}
	if m.target(m.panels[2]) == nil {
		t.Fatal("new panel has no target while running")
	}
	if got := strings.Count(m.View(), "╭"); got != 3 {
		t.Fatalf("%d panels drawn, want 3", got)
	}
	for range 10 {
		key(m, "+")
	}
	if len(m.panels) != maxPanels {
		t.Fatalf("panels=%d, want cap %d", len(m.panels), maxPanels)
	}

	// "-" removes the focused panel; a late reply for it is harmless.
	m.focus = numFields
	gone := m.panels[0]
	key(m, "-")
	if len(m.panels) != maxPanels-1 || m.panels[0] == gone {
		t.Fatal("focused panel not removed")
	}
	m.Update(resultMsg{p: gone, epoch: gone.epoch, rec: record{seq: 1}})
	for range 10 {
		key(m, "-")
	}
	if len(m.panels) != 1 || m.focus != numFields {
		t.Fatalf("panels=%d focus=%d, want 1 panel in focus", len(m.panels), m.focus)
	}
	m.View()
}

func TestLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.csv")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	gw := net.IPv4(10, 0, 0, 1).To4()
	for range 2 { // reopening appends without a second header
		l, err := openLog(path)
		if err != nil {
			t.Fatal(err)
		}
		l.write(record{at: now, ifName: "eth0", target: gw, gateway: true, seq: 1, rtt: 1500 * time.Microsecond})
		l.write(record{at: now, ifName: "eth0", target: gw, seq: 2, err: ping.ErrTimeout})
		l.close()
	}
	b, _ := os.ReadFile(path)
	want := "time,interface,kind,target,seq,status,rtt_ms,error\n" + strings.Repeat(
		"2026-10-06T12:00:00Z,eth0,gateway,10.0.0.1,1,ok,1.500,\n"+
			"2026-10-06T12:00:00Z,eth0,destination,10.0.0.1,2,timeout,,\n", 2)
	if string(b) != want {
		t.Errorf("log =\n%s\nwant\n%s", b, want)
	}
}

func TestLogJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	dst := net.IPv4(8, 8, 8, 8).To4()
	l.write(record{at: now, panel: 2, ifName: "eth0", target: dst, seq: 1, rtt: 1234567 * time.Nanosecond})
	l.write(record{at: now, ifName: "eth0", target: dst, seq: 2, err: errors.New("network unreachable")})
	l.close()
	b, _ := os.ReadFile(path)
	want := `{"type":"probe","time":"2026-10-06T12:00:00Z","panel":2,"interface":"eth0","kind":"destination","target":"8.8.8.8","seq":1,"status":"ok","rtt_ms":1.235}
{"type":"probe","time":"2026-10-06T12:00:00Z","interface":"eth0","kind":"destination","target":"8.8.8.8","seq":2,"status":"error","error":"network unreachable"}
`
	if string(b) != want {
		t.Errorf("log =\n%s\nwant\n%s", b, want)
	}
}

func TestDump(t *testing.T) {
	m := testModel()
	m.dst = net.IPv4(8, 8, 8, 8).To4()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	p0, p1 := m.panels[0], m.panels[1]
	p1.dest, p1.own = "example", net.ParseIP("2001:db8::1")

	p0.send(1, now)
	p0.result(1, 10*time.Millisecond, nil)
	p0.gw.send(1, now)
	p0.gw.result(1, time.Millisecond, nil)
	p1.send(1, now.Add(time.Millisecond))
	p1.result(1, 0, ping.ErrTimeout)
	p0.send(2, now.Add(time.Second))
	p0.result(2, 30*time.Millisecond, nil)
	p0.send(3, now.Add(2*time.Second)) // still pending: counted as sent, not dumped

	path := filepath.Join(t.TempDir(), "dump.jsonl")
	n, err := m.dump(path, now.Add(3*time.Second))
	if err != nil || n != 4 {
		t.Fatalf("dump = %d, %v; want 4 probes", n, err)
	}
	b, _ := os.ReadFile(path)
	want := `{"type":"session","format":1,"time":"2026-10-06T12:00:03Z","timeout_ms":1000,"size_bytes":56,"interval_ms":1000}
{"type":"summary","panel":1,"interface":"eth0","source":"10.0.0.10","kind":"destination","target":"8.8.8.8","sent":3,"recv":2,"lost":0,"loss_pct":0,"min_ms":10,"avg_ms":20,"max_ms":30,"jitter_ms":20}
{"type":"summary","panel":1,"interface":"eth0","source":"10.0.0.10","kind":"gateway","target":"10.0.0.1","sent":1,"recv":1,"lost":0,"loss_pct":0,"min_ms":1,"avg_ms":1,"max_ms":1}
{"type":"summary","panel":2,"interface":"wlan0","source":"2001:db8::20","kind":"destination","target":"2001:db8::1","sent":1,"recv":0,"lost":1,"loss_pct":100}
{"type":"probe","time":"2026-10-06T12:00:00Z","panel":1,"interface":"eth0","kind":"destination","target":"8.8.8.8","seq":1,"status":"ok","rtt_ms":10}
{"type":"probe","time":"2026-10-06T12:00:00Z","panel":1,"interface":"eth0","kind":"gateway","target":"10.0.0.1","seq":1,"status":"ok","rtt_ms":1}
{"type":"probe","time":"2026-10-06T12:00:00.001Z","panel":2,"interface":"wlan0","kind":"destination","target":"2001:db8::1","seq":1,"status":"timeout"}
{"type":"probe","time":"2026-10-06T12:00:01Z","panel":1,"interface":"eth0","kind":"destination","target":"8.8.8.8","seq":2,"status":"ok","rtt_ms":30}
`
	if string(b) != want {
		t.Errorf("dump =\n%s\nwant\n%s", b, want)
	}
}

func TestDumpKey(t *testing.T) {
	t.Chdir(t.TempDir())
	m := testModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.dst, m.running = net.IPv4(8, 8, 8, 8).To4(), true
	m.panels[0].send(1, time.Now())
	m.panels[0].result(1, time.Millisecond, nil)
	m.focus = numFields
	key(m, "w")
	files, _ := filepath.Glob("multi-ping-*.jsonl")
	if len(files) != 1 || !strings.Contains(m.View(), "saved 1 probes to "+files[0]) {
		t.Fatalf("files=%v notice=%q", files, m.notice)
	}
	key(m, "tab")
	if m.notice != "" {
		t.Fatal("notice not cleared by the next key")
	}
}

func TestViewFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {60, 20}, {40, 18}} {
		m := testModel()
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.dst, m.running = net.IPv4(8, 8, 8, 8), true
		for seq := 1; seq <= 50; seq++ {
			for i, p := range m.panels {
				p.send(seq, time.Now())
				switch {
				case i == 1 && seq%7 == 0:
					p.result(seq, 0, ping.ErrTimeout)
				case seq < 50:
					p.result(seq, time.Duration(seq*(i+1))*time.Millisecond, nil)
				}
			}
		}
		for _, open := range []bool{false, true} {
			if open {
				m.focus = numFields
				key(m, " ")
			}
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > size[1] {
				t.Errorf("%v open=%v: view is %d lines tall", size, open, len(lines))
			}
			for _, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%v open=%v: line is %d cells wide: %q", size, open, w, l)
				}
			}
			if size[0] != 100 || open {
				continue
			}
			t.Log("\n" + view)
			// Panel 1 is twice as slow, so only it may reach full height.
			graph, _, _ := m.viewGraph(size[0], 1)
			if strings.Contains(graph[0], "█") || !strings.Contains(graph[1], "█") {
				t.Errorf("shared scale not applied:\n%s", strings.Join(graph, "\n"))
			}
			graph, _, _ = m.viewGraph(size[0], maxGraphH)
			for _, l := range graph {
				if !strings.Contains(view, strings.TrimRight(l, " ")) {
					t.Errorf("view lacks the graph line %q", l)
				}
			}
			for _, want := range []string{"Δ vs eth0", "wlan0: +24.0 ms avg, +14.3% loss", "graph 0–96.0 ms", "last 60s  avg 49.0  p95 92.0 ms  lost 7 (14.3%)"} {
				if !strings.Contains(view, want) {
					t.Errorf("view lacks %q", want)
				}
			}
		}
	}
}

func TestGraph(t *testing.T) {
	m := testModel()
	m.dst = net.IPv4(8, 8, 8, 8)
	eth, wlan := m.panels[0], m.panels[1]
	// wlan0 joins at round 3; round 4 is lost on it and round 6 still open.
	for seq := 1; seq <= 6; seq++ {
		eth.send(seq, time.Now())
		eth.result(seq, 10*time.Millisecond, nil)
		if seq < 3 {
			continue
		}
		wlan.send(seq, time.Now())
		switch seq {
		case 4:
			wlan.result(seq, 0, ping.ErrTimeout)
		case 6:
		default:
			wlan.result(seq, 80*time.Millisecond, nil)
		}
	}
	graph, scale, over := m.viewGraph(40, 1)
	if scale != 80*time.Millisecond || over {
		t.Errorf("scale = %v over = %v, want 80ms false", scale, over)
	}
	// Each round is in the same column on both lines.
	if want := []string{"eth0  ▂▂▂▂▂▂", "wlan0   █×█·"}; !slices.Equal(graph, want) {
		t.Errorf("graph = %q, want %q", graph, want)
	}

	// A narrow graph keeps the newest rounds, still aligned.
	if graph, _, _ = m.viewGraph(9, 1); !slices.Equal(graph, []string{"eth ▂▂▂▂▂", "wla  █×█·"}) {
		t.Errorf("narrow graph = %q", graph)
	}
	for _, l := range graph {
		if w := lipgloss.Width(l); w > 9 {
			t.Errorf("line is %d cells wide: %q", w, l)
		}
	}

	// A terminal too short for both keeps the panels and drops the graph.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	if view := m.View(); strings.Contains(view, "▂▂▂") || lipgloss.Height(view) > 12 {
		t.Errorf("short view:\n%s", view)
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if view := m.View(); !strings.Contains(view, "wlan0   █×█·") {
		t.Errorf("view lacks the graph:\n%s", view)
	}
}

func TestGraphOutlier(t *testing.T) {
	m := testModel()
	m.dst = net.IPv4(8, 8, 8, 8)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	eth, wlan := m.panels[0], m.panels[1]
	for seq := 1; seq <= 30; seq++ {
		eth.send(seq, time.Now())
		wlan.send(seq, time.Now())
		eth.result(seq, 10*time.Millisecond, nil)
		wlan.result(seq, 20*time.Millisecond, nil)
	}
	// Twice the usual delay still fits the scale.
	eth.send(31, time.Now())
	eth.result(31, 40*time.Millisecond, nil)
	graph, scale, over := m.viewGraph(100, 1)
	if scale != 40*time.Millisecond || over || !strings.HasSuffix(graph[0], "▃█") {
		t.Errorf("scale = %v over = %v\n%s", scale, over, strings.Join(graph, "\n"))
	}

	// One stray reply is marked instead of flattening the rest.
	wlan.send(31, time.Now())
	wlan.result(31, 900*time.Millisecond, nil)
	graph, scale, over = m.viewGraph(100, 1)
	if scale != 40*time.Millisecond || !over {
		t.Errorf("scale = %v over = %v, want 40ms true", scale, over)
	}
	if !strings.HasSuffix(graph[0], "▃█") || !strings.HasSuffix(graph[1], "▅▲") {
		t.Errorf("graph:\n%s", strings.Join(graph, "\n"))
	}
	if view := m.View(); !strings.Contains(view, "graph 0–40.0 ms, ▲ above") {
		t.Errorf("view lacks the legend:\n%s", view)
	}

	// A panel that is slow throughout fits the scale, however short its history.
	m.addPanel()
	usb := m.panels[2]
	for seq := 30; seq <= 31; seq++ {
		usb.send(seq, time.Now())
		usb.result(seq, 300*time.Millisecond, nil)
	}
	graph, scale, over = m.viewGraph(100, 1)
	if scale != 600*time.Millisecond || !over || !strings.HasSuffix(graph[2], "▅▅") || !strings.HasSuffix(graph[1], "▁▲") {
		t.Errorf("scale = %v over = %v\n%s", scale, over, strings.Join(graph, "\n"))
	}
}

func TestGraphHeight(t *testing.T) {
	m := testModel()
	m.dst = net.IPv4(8, 8, 8, 8)
	eth, wlan := m.panels[0], m.panels[1]
	// eth0 climbs from an eighth of the scale to all of it; wlan0 stays at
	// half, loses one round and has one still open.
	for seq, ms := range []int{10, 20, 30, 40, 50, 60, 70, 80} {
		eth.send(seq+1, time.Now())
		eth.result(seq+1, time.Duration(ms)*time.Millisecond, nil)
		wlan.send(seq+1, time.Now())
		switch seq {
		case 2:
			wlan.result(seq+1, 0, ping.ErrTimeout)
		case 7:
		default:
			wlan.result(seq+1, 40*time.Millisecond, nil)
		}
	}
	trimmed := func(w, h int) []string {
		graph, _, _ := m.viewGraph(w, h)
		for i := range graph {
			graph[i] = strings.TrimRight(graph[i], " ")
		}
		return graph
	}
	want := []string{
		"           ▃▆█",
		"        ▂▅████",
		"eth0  ▄▇██████",
		"",
		"      ▅▅ ▅▅▅▅",
		"wlan0 ██×████·",
	}
	if graph := trimmed(40, 3); !slices.Equal(graph, want) {
		t.Errorf("graph:\n%s\nwant:\n%s", strings.Join(graph, "\n"), strings.Join(want, "\n"))
	}

	// A reply off the scale fills its column and is marked at the top.
	for seq := 9; seq <= 40; seq++ {
		wlan.send(seq, time.Now())
		wlan.result(seq, 40*time.Millisecond, nil)
	}
	wlan.send(41, time.Now())
	wlan.result(41, 900*time.Millisecond, nil)
	graph := trimmed(60, 3)
	for i, end := range []string{"▲", "█", "▇█"} {
		if !strings.HasSuffix(graph[3+i], end) {
			t.Errorf("line %d of wlan0 does not end in %q:\n%s", i, end, strings.Join(graph, "\n"))
		}
	}

	// The graph shrinks as the terminal gets shorter, then gives way.
	for h, want := range map[int]int{30: 6, 19: 4, 16: 2, 12: 0} {
		m.Update(tea.WindowSizeMsg{Width: 100, Height: h})
		view := m.View()
		lines := strings.Split(view, "\n")
		box := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "╰") })
		delta := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Δ") })
		if got := delta - box - 1; got != want || len(lines) > h {
			t.Errorf("height %d: %d graph lines, want %d; view is %d lines tall\n%s", h, got, want, len(lines), view)
		}
	}
}

func TestPanelRule(t *testing.T) {
	m := testModel()
	m.dst, m.running = net.IPv4(8, 8, 8, 8), true
	for seq := 1; seq <= 5; seq++ {
		m.panels[0].send(seq, time.Now())
		m.panels[0].result(seq, 10*time.Millisecond, nil)
	}
	rule := "├" + strings.Repeat("─", 38) + "┤"

	// The rule sits between the statistics and the log, joined to the frame.
	lines := strings.Split(m.viewPanel(0, 40, 12), "\n")
	if len(lines) != 12 || lines[6] != rule {
		t.Fatalf("panel:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[5], "gateway") || !strings.Contains(lines[7], "seq=2") || !strings.Contains(lines[10], "seq=5") {
		t.Errorf("rule not between statistics and log:\n%s", strings.Join(lines, "\n"))
	}

	// No rule without room for a log line under it, nor over the interface list.
	if box := m.viewPanel(0, 40, 8); strings.Contains(box, "├") || lipgloss.Height(box) != 8 {
		t.Errorf("short panel:\n%s", box)
	}
	m.focus = numFields
	key(m, " ")
	if box := m.viewPanel(0, 40, 12); strings.Contains(box, "├") {
		t.Errorf("rule over the open list:\n%s", box)
	}
}
