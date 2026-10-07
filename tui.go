package main

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kimonus/multi-ping/internal/ping"
)

// Top-bar fields, in focus order. Panels follow them in the focus cycle.
const (
	fDest = iota
	fTimeout
	fSize
	fInterval
	numFields
)

var fieldLabels = [numFields]string{"Destination", "Timeout ms", "Packet size", "Interval ms"}

// maxRows is how many probes each panel keeps per target, for the log on
// screen and for dumps; 0 means no limit. Set by the -keep flag.
var maxRows = 100_000

const maxPanels = 6

// minBodyH is the least height of the panels: border, title and statistics.
const minBodyH = 7

// recentWindow is the span of the "last 60s" figures.
const recentWindow = 60 * time.Second

// ifaceRefresh is how often the interface list is re-read.
const ifaceRefresh = 2 * time.Second

var errIfaceDown = errors.New("interface down")

var (
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleFocus  = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleLabel  = lipgloss.NewStyle().Bold(true)
	borderPlain = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	borderFocus = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("14"))
)

// panelColors tell the panels apart: each one's title and its line in the
// graph share a colour. Red and grey are left out, they mean lost and idle.
var panelColors = [maxPanels]lipgloss.Color{"14", "11", "13", "10", "12", "15"}

type rowState int

const (
	rowPending rowState = iota
	rowOK
	rowTimeout
	rowError
)

type row struct {
	seq   int
	at    time.Time // when the probe was sent
	state rowState
	rtt   time.Duration
	err   string
}

// series is the log and running statistics of probes to one target.
type series struct {
	rows []row

	sent, recv, lost int
	min, max, sum    time.Duration
	prev             time.Duration
	jitterSum        time.Duration
}

// lossPct is the share of finished probes that got no reply.
func (s *series) lossPct() float64 {
	if s.recv+s.lost == 0 {
		return 0
	}
	return 100 * float64(s.lost) / float64(s.recv+s.lost)
}

func (s *series) avg() time.Duration {
	if s.recv == 0 {
		return 0
	}
	return s.sum / time.Duration(s.recv)
}

func (s *series) send(seq int, at time.Time) {
	s.sent++
	s.rows = append(s.rows, row{seq: seq, at: at})
	if maxRows > 0 && len(s.rows) > maxRows {
		s.rows = s.rows[len(s.rows)-maxRows:]
	}
}

func (s *series) result(seq int, rtt time.Duration, err error) {
	r := row{seq: seq}
	switch {
	case err == nil:
		r.state, r.rtt = rowOK, rtt
		if s.recv == 0 || rtt < s.min {
			s.min = rtt
		}
		if s.recv > 0 {
			s.jitterSum += (rtt - s.prev).Abs()
		}
		s.max = max(s.max, rtt)
		s.sum += rtt
		s.prev = rtt
		s.recv++
	case errors.Is(err, ping.ErrTimeout):
		r.state = rowTimeout
		s.lost++
	default:
		r.state, r.err = rowError, err.Error()
		s.lost++
	}
	for i := len(s.rows) - 1; i >= 0; i-- {
		if s.rows[i].seq == seq {
			r.at = s.rows[i].at
			s.rows[i] = r
			break
		}
	}
}

type windowStats struct {
	recv, lost int
	avg, p95   time.Duration
}

// window summarises the finished probes sent within d before now.
func (s *series) window(now time.Time, d time.Duration) windowStats {
	var w windowStats
	var rtts []time.Duration
	var sum time.Duration
	for i := len(s.rows) - 1; i >= 0 && now.Sub(s.rows[i].at) <= d; i-- {
		switch r := s.rows[i]; r.state {
		case rowPending:
		case rowOK:
			rtts = append(rtts, r.rtt)
			sum += r.rtt
		default:
			w.lost++
		}
	}
	if w.recv = len(rtts); w.recv > 0 {
		slices.Sort(rtts)
		w.avg = sum / time.Duration(w.recv)
		w.p95 = rtts[(w.recv*95+99)/100-1]
	}
	return w
}

// graphBars are the heights a delay can be drawn at, lowest first.
var graphBars = []rune("▁▂▃▄▅▆▇█")

// graphOver marks a reply too slow for the graph's scale.
const graphOver = '▲'

// maxGraphH is how many lines a panel's graph takes when there is room, and
// graphLogH how many lines of the panels' logs a taller graph must leave.
const (
	maxGraphH = 3
	graphLogH = 3
)

// graphHeadroom is how far above the 95th percentile of a panel's replies
// the graph's scale may reach.
const graphHeadroom = 2

// panel is one interface's column.
type panel struct {
	series        // probes to the destination
	gw     series // probes to the interface's gateway

	ifc   ping.Interface
	down  bool // the interface is gone from the system's list
	epoch int  // bumped on reset so in-flight replies are dropped

	// dest overrides the shared destination when not empty; own is its
	// resolved address and destErr why resolving failed.
	dest    string
	own     net.IP
	destErr string
}

func (p *panel) reset() {
	p.series, p.gw = series{}, series{}
	p.epoch++
}

type (
	startMsg  struct{}
	ifTickMsg struct{}
	ifacesMsg []ping.Interface
	tickMsg   struct{ gen int }
	// resolvedMsg carries looked-up addresses: the shared destination when
	// start is set, plus every listed panel's own destination.
	resolvedMsg struct {
		gen    int
		start  bool
		ip     net.IP
		err    error
		panels []panelTarget
	}
	panelTarget struct {
		p    *panel
		dest string
		ip   net.IP
		err  error
	}
	resultMsg struct {
		p     *panel
		epoch int
		rec   record
	}
)

type model struct {
	ifaces []ping.Interface
	fields [numFields]string
	focus  int
	panels []*panel

	// menu is the panel whose interface list is open, or -1.
	menu, menuSel int

	// editing is the panel whose own destination is being typed, or -1.
	editing int
	editBuf string

	// Settings in effect; the numeric fields are applied on Tab or start.
	timeout, interval time.Duration
	size              int
	prefer6, gateway  bool
	log               *logger

	running bool
	gen     int // bumped on every start so stale ticks are dropped
	seq     int
	dst     net.IP // the shared destination, resolved
	status  string
	notice  string // outcome of the last dump, shown until the next key
	auto    bool
	w, h    int
}

func newModel(ifaces []ping.Interface, selected []int, o options) *model {
	m := &model{ifaces: ifaces, menu: -1, editing: -1, prefer6: o.prefer6, gateway: o.gateway, log: o.log}
	m.fields = [numFields]string{o.dest, strconv.Itoa(o.timeoutMS), strconv.Itoa(o.size), strconv.Itoa(o.intervalMS)}
	for i, idx := range selected {
		p := &panel{ifc: ifaces[idx]}
		if i < len(o.panelDests) {
			p.dest = o.panelDests[i]
		}
		m.panels = append(m.panels, p)
	}
	// Start right away when the command line gave every panel a destination.
	m.auto = !m.needShared() || o.dest != ""
	m.apply()
	return m
}

// apply reads the numeric fields, clamps them and writes the result back.
func (m *model) apply() {
	num := func(f, def, lo, hi int) int {
		v, err := strconv.Atoi(m.fields[f])
		if err != nil {
			v = def
		}
		v = min(max(v, lo), hi)
		m.fields[f] = strconv.Itoa(v)
		return v
	}
	m.timeout = time.Duration(num(fTimeout, 1000, 1, 60000)) * time.Millisecond
	m.size = num(fSize, 56, 0, 65000)
	m.interval = time.Duration(num(fInterval, 1000, 100, 60000)) * time.Millisecond
}

// target is the address panel p pings: its own destination if it has one,
// else the shared one. Nil while unresolved.
func (m *model) target(p *panel) net.IP {
	if p.dest != "" {
		return p.own
	}
	return m.dst
}

// v6 reports which address family panel p is (or will be) pinging.
func (m *model) v6(p *panel) bool {
	if t := m.target(p); t != nil {
		return t.To4() == nil
	}
	return m.prefer6
}

// needShared reports whether any panel relies on the shared destination.
func (m *model) needShared() bool {
	return slices.ContainsFunc(m.panels, func(p *panel) bool { return p.dest == "" })
}

// panelName labels a panel in the comparison line.
func (m *model) panelName(p *panel) string {
	if p.dest != "" {
		return p.ifc.Name + "→" + p.dest
	}
	return p.ifc.Name
}

func (m *model) addPanel() {
	if len(m.panels) >= maxPanels {
		return
	}
	// Prefer an interface no panel shows yet.
	p := &panel{ifc: m.panels[len(m.panels)-1].ifc}
	for _, ifc := range m.ifaces {
		if !slices.ContainsFunc(m.panels, func(q *panel) bool { return q.ifc.Name == ifc.Name }) {
			p.ifc = ifc
			break
		}
	}
	p.down = m.ifaceIndex(p.ifc.Name) < 0
	m.panels = append(m.panels, p)
	m.focus = numFields + len(m.panels) - 1
}

// removePanel drops the focused panel, or the last one when a field has focus.
func (m *model) removePanel() {
	if len(m.panels) == 1 {
		return
	}
	i := len(m.panels) - 1
	if m.focus >= numFields {
		i = m.focus - numFields
	}
	m.panels = slices.Delete(m.panels, i, i+1)
	m.focus = min(m.focus, numFields+len(m.panels)-1)
}

// setPanelDest gives p its own destination; empty returns it to the shared one.
func (m *model) setPanelDest(p *panel, dest string) tea.Cmd {
	if dest == p.dest {
		return nil
	}
	p.dest, p.own, p.destErr = dest, nil, ""
	p.reset()
	if !m.running || dest == "" {
		return nil
	}
	return m.resolve([]*panel{p}, false)
}

// resolve looks up the own destinations of ps and, for a start, the shared one.
func (m *model) resolve(ps []*panel, start bool) tea.Cmd {
	msg := resolvedMsg{gen: m.gen, start: start}
	for _, p := range ps {
		if p.dest != "" {
			msg.panels = append(msg.panels, panelTarget{p: p, dest: p.dest})
		}
	}
	shared, prefer6 := strings.TrimSpace(m.fields[fDest]), m.prefer6
	return func() tea.Msg {
		if start && shared != "" {
			msg.ip, msg.err = ping.Resolve(shared, prefer6)
		}
		for i := range msg.panels {
			t := &msg.panels[i]
			t.ip, t.err = ping.Resolve(t.dest, prefer6)
		}
		return msg
	}
}

func (m *model) ifaceIndex(name string) int {
	return slices.IndexFunc(m.ifaces, func(i ping.Interface) bool { return i.Name == name })
}

// setIfaces installs a fresh interface list. Panels keep their choice by
// name: they pick up address changes and are marked down while it is missing.
func (m *model) setIfaces(list []ping.Interface) {
	m.ifaces = list
	for _, p := range m.panels {
		i := m.ifaceIndex(p.ifc.Name)
		if p.down = i < 0; !p.down {
			p.ifc = list[i]
		}
	}
	if len(list) == 0 {
		m.menu = -1
	}
	m.menuSel = min(m.menuSel, max(len(list)-1, 0))
}

func (m *model) selectIface(p *panel, i int) {
	if len(m.ifaces) == 0 {
		return
	}
	i = (i + len(m.ifaces)) % len(m.ifaces)
	p.ifc, p.down = m.ifaces[i], false
	p.reset()
}

func ifTick() tea.Cmd {
	return tea.Tick(ifaceRefresh, func(time.Time) tea.Msg { return ifTickMsg{} })
}

func (m *model) Init() tea.Cmd {
	if m.auto {
		return tea.Batch(ifTick(), func() tea.Msg { return startMsg{} })
	}
	return ifTick()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyMsg:
		return m, m.onKey(msg)
	case startMsg:
		return m, m.start()
	case ifTickMsg:
		return m, func() tea.Msg {
			list, err := ping.Interfaces()
			if err != nil {
				return ifacesMsg(nil)
			}
			return ifacesMsg(list)
		}
	case ifacesMsg:
		// nil means the list could not be read; keep the old one.
		if msg != nil {
			m.setIfaces(msg)
		}
		return m, ifTick()
	case resolvedMsg:
		if msg.gen != m.gen {
			break
		}
		for _, t := range msg.panels {
			if t.p.dest != t.dest {
				continue // edited again meanwhile
			}
			t.p.own, t.p.destErr = t.ip, ""
			if t.err != nil {
				t.p.destErr = t.err.Error()
			}
		}
		if !msg.start {
			break
		}
		if msg.err != nil && m.needShared() {
			m.status = msg.err.Error()
			break
		}
		m.dst, m.status, m.running, m.seq = msg.ip, "", true, 0
		for _, p := range m.panels {
			p.reset()
		}
		if m.focus == fDest {
			m.focus = numFields
		}
		return m, m.round()
	case tickMsg:
		if m.running && msg.gen == m.gen {
			return m, m.round()
		}
	case resultMsg:
		rec := msg.rec
		if p := msg.p; msg.epoch == p.epoch {
			if rec.gateway {
				p.gw.result(rec.seq, rec.rtt, rec.err)
			} else {
				p.result(rec.seq, rec.rtt, rec.err)
			}
		}
		if m.log != nil {
			if err := m.log.write(rec); err != nil {
				m.status, m.log = "log stopped: "+err.Error(), nil
			}
		}
	}
	return m, nil
}

func (m *model) start() tea.Cmd {
	m.apply()
	m.gen++
	m.running = false
	if m.needShared() && strings.TrimSpace(m.fields[fDest]) == "" {
		m.status, m.focus = "enter a destination first", fDest
		return nil
	}
	m.status = "resolving…"
	return m.resolve(m.panels, true)
}

// round sends the next echo on every panel at once and schedules the next round.
func (m *model) round() tea.Cmd {
	m.seq++
	gen, seq, size, timeout := m.gen, m.seq, m.size, m.timeout
	cmds := []tea.Cmd{tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{gen} })}
	for i, p := range m.panels {
		dst := m.target(p)
		if dst == nil {
			continue // its own destination did not resolve (yet)
		}
		ifc, epoch, down := p.ifc, p.epoch, p.down
		probe := func(target net.IP, gateway bool) tea.Cmd {
			rec := record{at: time.Now(), panel: i + 1, ifName: ifc.Name, target: target, gateway: gateway, seq: seq}
			return func() tea.Msg {
				if down {
					rec.err = errIfaceDown
				} else {
					rec.rtt, rec.err = ping.Ping(ifc, target, seq, size, timeout)
				}
				return resultMsg{p, epoch, rec}
			}
		}
		now := time.Now()
		p.send(seq, now)
		cmds = append(cmds, probe(dst, false))
		if m.gateway && !down && ifc.Gateway != nil {
			p.gw.send(seq, now)
			cmds = append(cmds, probe(ifc.Gateway, true))
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	m.notice = ""
	if m.menu >= 0 {
		return m.onMenuKey(key)
	}
	if m.editing >= 0 {
		return m.onEditKey(k)
	}

	n := numFields + len(m.panels)
	switch key {
	case "tab":
		m.apply()
		m.focus = (m.focus + 1) % n
		return nil
	case "shift+tab":
		m.apply()
		m.focus = (m.focus + n - 1) % n
		return nil
	case "enter":
		if m.running {
			m.running = false
			return nil
		}
		return m.start()
	}

	if m.focus < numFields {
		f := &m.fields[m.focus]
		if k.Type == tea.KeyBackspace && *f != "" {
			r := []rune(*f)
			*f = string(r[:len(r)-1])
		}
		if k.Type != tea.KeyRunes {
			return nil
		}
		for _, r := range k.Runes {
			switch {
			case m.focus == fDest && !unicode.IsSpace(r), unicode.IsDigit(r):
				*f += string(r)
			default:
				// Letters mean nothing in a numeric field, so hotkeys still work there.
				return m.hotkey(string(r))
			}
		}
		return nil
	}

	pi := m.focus - numFields
	p := m.panels[pi]
	switch key {
	case "left", "h":
		m.selectIface(p, m.ifaceIndex(p.ifc.Name)-1)
	case "right", "l":
		m.selectIface(p, m.ifaceIndex(p.ifc.Name)+1)
	case "down", "j", " ":
		if len(m.ifaces) > 0 {
			m.menu, m.menuSel = pi, max(m.ifaceIndex(p.ifc.Name), 0)
		}
	case "d":
		m.editing, m.editBuf = pi, p.dest
	default:
		return m.hotkey(key)
	}
	return nil
}

// onMenuKey handles keys while a panel's interface list is open.
func (m *model) onMenuKey(key string) tea.Cmd {
	n := len(m.ifaces)
	switch key {
	case "up", "k":
		m.menuSel = (m.menuSel + n - 1) % n
	case "down", "j":
		m.menuSel = (m.menuSel + 1) % n
	case "enter", " ":
		if p := m.panels[m.menu]; p.down || m.ifaces[m.menuSel].Name != p.ifc.Name {
			m.selectIface(p, m.menuSel)
		}
		m.menu = -1
	case "esc", "tab", "shift+tab", "q":
		m.menu = -1
	}
	return nil
}

// onEditKey handles keys while a panel's own destination is being typed.
func (m *model) onEditKey(k tea.KeyMsg) tea.Cmd {
	switch k.Type {
	case tea.KeyEsc:
		m.editing = -1
	case tea.KeyEnter:
		p := m.panels[m.editing]
		m.editing = -1
		return m.setPanelDest(p, m.editBuf)
	case tea.KeyBackspace:
		if r := []rune(m.editBuf); len(r) > 0 {
			m.editBuf = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		for _, r := range k.Runes {
			if !unicode.IsSpace(r) {
				m.editBuf += string(r)
			}
		}
	}
	return nil
}

func (m *model) hotkey(key string) tea.Cmd {
	switch key {
	case "q":
		return tea.Quit
	case "w":
		path := "multi-ping-" + time.Now().Format("20060102-150405") + ".jsonl"
		if n, err := m.dump(path, time.Now()); err != nil {
			m.notice = "dump failed: " + err.Error()
		} else {
			m.notice = fmt.Sprintf("saved %d probes to %s", n, path)
		}
	case "+", "=":
		m.addPanel()
	case "-", "_":
		m.removePanel()
	case "r":
		for _, p := range m.panels {
			p.reset()
		}
	case "v":
		// Only matters for host names that have both families.
		m.prefer6 = !m.prefer6
		if m.running {
			return m.start()
		}
		m.dst = nil
		for _, p := range m.panels {
			p.own = nil
		}
	}
	return nil
}

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	top := m.viewTop()
	help := []string{"Tab next", "←/→ or Space interface", "d panel destination", "+/- panels",
		"Enter start/stop", "r reset", "w save dump", "v IPv4/6", "q quit"}
	switch {
	case m.menu >= 0:
		help = []string{"↑/↓ choose", "Enter select", "Esc cancel"}
	case m.editing >= 0:
		help = []string{"type this panel's destination", "Enter confirm (empty = shared)", "Esc cancel"}
	}
	bottom := styleDim.Render(wrap(help, " · ", m.w))
	bodyH := max(m.h-lipgloss.Height(top)-lipgloss.Height(bottom)-2, minBodyH)
	n := len(m.panels)
	widths := make([]int, n)
	for i := range widths {
		widths[i] = m.w / n
	}
	widths[n-1] = m.w - (m.w/n)*(n-1)

	// The graph is as tall as leaves the panels a few lines of their logs,
	// and gives way to them when the terminal is too short for both.
	var graph []string
	var scale time.Duration
	var over bool
	for gh := maxGraphH; gh > 0 && graph == nil; gh-- {
		room := minBodyH + graphLogH
		if gh == 1 {
			room = minBodyH
		}
		if bodyH-n*gh >= room {
			graph, scale, over = m.viewGraph(m.w, gh)
		}
	}
	bodyH -= len(graph)
	cols := make([]string, n)
	for i := range m.panels {
		cols[i] = m.viewPanel(i, widths[i], bodyH)
	}
	parts := append([]string{top, lipgloss.JoinHorizontal(lipgloss.Top, cols...)}, graph...)
	parts = append(parts, clip(m.viewCompare(scale, over), m.w), clip(m.viewStatus(), m.w), bottom)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// viewGraph draws the recent rounds of every panel across the full width, h
// lines per panel and one column per round. A round sits in the same column
// for every panel and all share one scale, so delay and loss on different
// interfaces can be compared directly. It also returns the delay that is drawn
// at full height, and whether any reply was slower than that.
func (m *model) viewGraph(w, h int) ([]string, time.Duration, bool) {
	labelW, last := 0, 0
	for _, p := range m.panels {
		labelW = max(labelW, lipgloss.Width(m.panelName(p)))
		if n := len(p.rows); n > 0 {
			last = max(last, p.rows[n-1].seq)
		}
	}
	labelW = min(labelW, w/3)
	cells := max(w-labelW-1, 1)
	// Fill from the left at first, then scroll with the newest round on the right.
	first := max(last-cells+1, 1)

	// The slowest reply sets the scale, unless it is far above the rest: a
	// few stray replies must not flatten every other bar. Each panel is
	// judged by its own replies, so one that is slow throughout still fits.
	visible := make([][]row, len(m.panels))
	var scale, limit time.Duration
	var rtts []time.Duration
	for i, p := range m.panels {
		at, _ := slices.BinarySearchFunc(p.rows, first, func(r row, seq int) int { return r.seq - seq })
		rows := p.rows[at:]
		visible[i] = rows
		rtts = rtts[:0]
		for _, r := range rows {
			if r.state == rowOK {
				rtts = append(rtts, r.rtt)
			}
		}
		if n := len(rtts); n > 0 {
			slices.Sort(rtts)
			scale = max(scale, rtts[n-1])
			limit = max(limit, rtts[(n*95+99)/100-1]*graphHeadroom)
		}
	}
	over := scale > limit
	scale = min(scale, limit)

	// A bar's height is counted in eighths of a line.
	full := len(graphBars)
	steps := time.Duration(h * full)
	var lines []string
	for i, p := range m.panels {
		color := lipgloss.NewStyle().Foreground(panelColors[i])
		label := color.Render(fmt.Sprintf("%-*s", labelW, clip(m.panelName(p), labelW)))
		// Bars stand on the bottom line, next to the label; line counts up from it.
		for line := h - 1; line >= 0; line-- {
			var b strings.Builder
			if line == 0 {
				b.WriteString(label)
			} else {
				b.WriteString(strings.Repeat(" ", labelW))
			}
			b.WriteByte(' ')
			// Consecutive cells of one style are rendered together.
			var run []rune
			var runStyle *lipgloss.Style
			flush := func() {
				if len(run) > 0 {
					b.WriteString(runStyle.Render(string(run)))
					run = run[:0]
				}
			}
			put := func(r rune, style *lipgloss.Style) {
				if style != runStyle {
					flush()
				}
				run, runStyle = append(run, r), style
			}
			col := first
			for _, r := range visible[i] {
				cell, style := ' ', &color
				switch {
				case r.state == rowOK && r.rtt > scale:
					if cell = graphBars[full-1]; line == h-1 {
						cell = graphOver
					}
				case r.state == rowOK:
					height := 1
					if scale > 0 {
						height += min(int(r.rtt*steps/scale), int(steps)-1)
					}
					if part := height - line*full; part > 0 {
						cell = graphBars[min(part, full)-1]
					}
				case line > 0:
				case r.state == rowPending:
					cell, style = '·', &styleDim
				default:
					cell, style = '×', &styleBad
				}
				if cell == ' ' {
					continue // drawn as a gap, like a round this panel sat out
				}
				if r.seq > col {
					flush()
					b.WriteString(strings.Repeat(" ", r.seq-col))
				}
				col = r.seq + 1
				put(cell, style)
			}
			flush()
			lines = append(lines, clip(b.String(), w))
		}
	}
	return lines, scale, over
}

// wrap joins items with sep, breaking into lines no wider than w.
func wrap(items []string, sep string, w int) string {
	var lines []string
	cur := ""
	for _, item := range items {
		switch {
		case cur == "":
			cur = item
		case lipgloss.Width(cur)+lipgloss.Width(sep)+lipgloss.Width(item) > w:
			lines = append(lines, cur)
			cur = item
		default:
			cur += sep + item
		}
	}
	lines = append(lines, cur)
	for i := range lines {
		lines[i] = clip(lines[i], w)
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewTop() string {
	widths := [numFields]int{15, 5, 5, 5}
	var items []string
	for f, label := range fieldLabels {
		val := fmt.Sprintf("%-*s", widths[f], m.fields[f])
		if m.focus == f {
			val = styleFocus.Render(val)
		}
		items = append(items, styleLabel.Render(label)+" ["+val+"]")
	}
	return wrap(items, "  ", m.w)
}

func (m *model) viewStatus() string {
	var extra string
	if m.prefer6 {
		extra += "  IPv6 preferred"
	}
	if m.log != nil {
		extra += "  log: " + m.log.path
	}
	extra = styleDim.Render(extra)
	if m.notice != "" {
		extra += "  " + styleLabel.Render(m.notice)
	}
	switch {
	case m.running:
		return styleGood.Render("● running") +
			fmt.Sprintf("  seq %d  %d bytes  every %s", m.seq, m.size, m.interval) + extra
	case m.status != "":
		return styleBad.Render("■ "+m.status) + extra
	}
	return styleDim.Render("■ stopped — Enter to start") + extra
}

// viewCompare shows how every other panel differs from the first one.
func (m *model) viewCompare(scale time.Duration, over bool) string {
	ref := m.panels[0]
	parts := []string{"Δ vs " + m.panelName(ref)}
	signed := func(v float64, text string) string {
		switch {
		case v > 0:
			return styleBad.Render("+" + text)
		case v < 0:
			return styleGood.Render("-" + text)
		}
		return "±" + text
	}
	for _, p := range m.panels[1:] {
		avg := "– avg"
		if p.recv > 0 && ref.recv > 0 {
			d := p.avg() - ref.avg()
			avg = signed(float64(d), fmtMS(d.Abs())+" ms avg")
		}
		d := p.lossPct() - ref.lossPct()
		loss := signed(d, fmt.Sprintf("%.1f%% loss", max(d, -d)))
		parts = append(parts, fmt.Sprintf("%s: %s, %s", m.panelName(p), avg, loss))
	}
	if scale > 0 {
		legend := "graph 0–" + fmtMS(scale) + " ms"
		if over {
			legend += ", " + string(graphOver) + " above"
		}
		parts = append(parts, styleDim.Render(legend))
	}
	return strings.Join(parts, styleDim.Render(" │ "))
}

// ifaceLabel is an interface's name with its address for the family in use.
func ifaceLabel(ifc ping.Interface, v6 bool) string {
	addr := "no IPv4"
	if v6 {
		addr = "no IPv6"
	}
	if ip := ifc.Addr(v6); ip != nil {
		addr = ip.String()
	}
	return fmt.Sprintf("%s (%s)", ifc.Name, addr)
}

func (m *model) viewPanel(i, w, h int) string {
	p := m.panels[i]
	innerW, innerH := max(w-2, 1), max(h-2, 1)
	focused := m.focus == numFields+i

	title := "◂ " + ifaceLabel(p.ifc, m.v6(p)) + " ▸"
	if focused {
		title = styleFocus.Foreground(panelColors[i]).Render(title)
	} else {
		title = styleLabel.Foreground(panelColors[i]).Render(title)
	}
	if p.down {
		title += styleBad.Render(" down")
	}
	switch t := m.target(p); {
	case m.editing == i:
		title = styleLabel.Render("destination") + " [" + styleFocus.Render(m.editBuf+" ") + "]"
	case p.destErr != "":
		title += styleBad.Render(" → " + p.dest + ": cannot resolve")
	case p.dest != "" && t == nil:
		title += " → " + p.dest
	case p.dest != "":
		title += " → " + t.String()
	case t != nil:
		title += styleDim.Render(" → " + t.String())
	case m.running:
		title += styleBad.Render(" → no destination (d to set)")
	}

	var lines []string
	if m.menu == i {
		lines = append([]string{title}, m.viewMenu(innerH-1, m.v6(p))...)
	} else {
		lines = append([]string{title}, p.viewStats(m)...)
		rows := p.rows
		if logH := max(innerH-len(lines), 0); len(rows) > logH {
			rows = rows[len(rows)-logH:]
		}
		for _, r := range rows {
			seq := fmt.Sprintf("seq=%-5d ", r.seq)
			switch r.state {
			case rowPending:
				lines = append(lines, styleDim.Render(seq+"…"))
			case rowOK:
				lines = append(lines, seq+fmtMS(r.rtt)+" ms")
			case rowTimeout:
				lines = append(lines, styleBad.Render(seq+"timeout"))
			case rowError:
				lines = append(lines, styleBad.Render(seq+r.err))
			}
		}
	}
	for j := range lines {
		lines[j] = clip(lines[j], innerW)
	}

	border := borderPlain
	if focused {
		border = borderFocus
	}
	return border.Width(innerW).Height(innerH).Render(strings.Join(lines, "\n"))
}

// viewStats renders the fixed lines between a panel's title and its log.
func (p *panel) viewStats(m *model) []string {
	lost := func(n int, pct float64) string {
		if n == 0 {
			return "lost 0"
		}
		return styleBad.Render(fmt.Sprintf("lost %d (%.1f%%)", n, pct))
	}

	counts := fmt.Sprintf("sent %d  recv %d  %s", p.sent, p.recv, lost(p.lost, p.lossPct()))
	times := styleDim.Render("min –  avg –  max –  jitter –")
	if p.recv > 0 {
		jitter := "–"
		if p.recv > 1 {
			jitter = fmtMS(p.jitterSum / time.Duration(p.recv-1))
		}
		times = fmt.Sprintf("min %s  avg %s  max %s  jitter %s ms",
			fmtMS(p.min), fmtMS(p.avg()), fmtMS(p.max), jitter)
	}

	recent := styleDim.Render("last 60s  –")
	if win := p.window(time.Now(), recentWindow); win.recv+win.lost > 0 {
		t := "avg –  p95 –"
		if win.recv > 0 {
			t = fmt.Sprintf("avg %s  p95 %s ms", fmtMS(win.avg), fmtMS(win.p95))
		}
		recent = "last 60s  " + t + "  " + lost(win.lost, 100*float64(win.lost)/float64(win.recv+win.lost))
	}

	gw := styleDim.Render("gateway –")
	switch g := &p.gw; {
	case !m.gateway:
		gw = styleDim.Render("gateway off")
	case p.ifc.Gateway == nil:
	case g.recv > 0:
		gw = fmt.Sprintf("gateway %s  avg %s  max %s ms  %s",
			p.ifc.Gateway, fmtMS(g.avg()), fmtMS(g.max), lost(g.lost, g.lossPct()))
	default:
		gw = fmt.Sprintf("gateway %s  %s", p.ifc.Gateway, lost(g.lost, g.lossPct()))
	}

	return []string{counts, times, recent, gw}
}

// viewMenu renders the open interface list, scrolled to keep the selection
// visible in h lines.
func (m *model) viewMenu(h int, v6 bool) []string {
	h = max(h, 1)
	first := min(max(m.menuSel-h/2, 0), max(len(m.ifaces)-h, 0))
	var lines []string
	for i := first; i < len(m.ifaces) && len(lines) < h; i++ {
		if i == m.menuSel {
			lines = append(lines, styleFocus.Render("▸ "+ifaceLabel(m.ifaces[i], v6)))
		} else {
			lines = append(lines, "  "+ifaceLabel(m.ifaces[i], v6))
		}
	}
	return lines
}

func clip(s string, w int) string {
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

func fmtMS(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	switch {
	case ms < 10:
		return fmt.Sprintf("%.2f", ms)
	case ms < 100:
		return fmt.Sprintf("%.1f", ms)
	}
	return fmt.Sprintf("%.0f", ms)
}
