// multi-ping pings one destination through several network interfaces at the
// same time, to compare their delay and packet loss side by side.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kimonus/multi-ping/internal/ping"
)

// options are the settings shared by the terminal UI and plain mode.
type options struct {
	dest                        string
	timeoutMS, size, intervalMS int
	prefer6                     bool     // pick IPv6 when a host name has both families
	gateway                     bool     // also ping each interface's gateway
	panelDests                  []string // per panel: its own destination, "" = dest
	log                         *logger
}

func main() {
	var o options
	var (
		list    = flag.Bool("list", false, "list usable interfaces and exit")
		plain   = flag.Bool("plain", false, "print plain lines instead of the terminal UI")
		count   = flag.Int("c", 0, "plain mode: stop after this many rounds (0 = forever)")
		names   = flag.String("i", "", "comma-separated interfaces, one panel each; name=dest gives a panel its own destination")
		panels  = flag.Int("p", 2, "number of panels when -i is not given")
		noGW    = flag.Bool("no-gw", false, "do not ping each interface's gateway")
		logPath = flag.String("o", "", "append every probe to this CSV file")
	)
	flag.IntVar(&o.timeoutMS, "t", 1000, "reply timeout in ms")
	flag.IntVar(&o.size, "s", 56, "payload size in bytes")
	flag.IntVar(&o.intervalMS, "n", 1000, "interval between rounds in ms")
	flag.IntVar(&maxRows, "keep", maxRows, "probes kept in memory per panel and target, for dumps (0 = no limit)")
	flag.BoolVar(&o.prefer6, "6", false, "prefer IPv6 when the destination name has both")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] [destination]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	o.dest = flag.Arg(0)
	if maxRows < 0 {
		fatal(fmt.Errorf("-keep must not be negative"))
	}
	o.gateway = !*noGW

	ifaces, err := ping.Interfaces()
	if err != nil {
		fatal(err)
	}
	if *list {
		for _, ifc := range ifaces {
			fmt.Printf("%-20s %-15s gw %-15s %s\n", ifc.Name, orDash(ifc.IP), orDash(ifc.Gateway), orDash(ifc.IP6))
		}
		return
	}
	if len(ifaces) == 0 {
		fatal(fmt.Errorf("no interface that is up and has an address"))
	}

	// Indexes into ifaces, one per panel.
	var selected []int
	if *names != "" {
		for entry := range strings.SplitSeq(*names, ",") {
			name, dest, _ := strings.Cut(strings.TrimSpace(entry), "=")
			idx := -1
			for i, ifc := range ifaces {
				if ifc.Name == name {
					idx = i
				}
			}
			if idx < 0 {
				fatal(fmt.Errorf("interface %q not found (try -list)", name))
			}
			selected = append(selected, idx)
			o.panelDests = append(o.panelDests, dest)
		}
		if len(selected) > maxPanels {
			fatal(fmt.Errorf("at most %d panels", maxPanels))
		}
	}

	if *logPath != "" {
		if o.log, err = openLog(*logPath); err != nil {
			fatal(err)
		}
		defer o.log.close()
	}

	if *plain {
		if selected == nil {
			for i := range ifaces {
				selected = append(selected, i)
			}
		}
		runPlain(ifaces, selected, o, *count)
		return
	}

	if selected == nil {
		for i := range min(max(*panels, 1), maxPanels) {
			selected = append(selected, i%len(ifaces))
		}
	}
	if _, err := tea.NewProgram(newModel(ifaces, selected, o), tea.WithAltScreen()).Run(); err != nil {
		fatal(err)
	}
}

func runPlain(ifaces []ping.Interface, selected []int, o options, count int) {
	// One target per panel: its own destination, else the shared one.
	dsts := make([]net.IP, len(selected))
	for i := range selected {
		dest := o.dest
		if i < len(o.panelDests) && o.panelDests[i] != "" {
			dest = o.panelDests[i]
		}
		if dest == "" {
			fatal(fmt.Errorf("plain mode needs a destination"))
		}
		var err error
		if dsts[i], err = ping.Resolve(dest, o.prefer6); err != nil {
			fatal(err)
		}
	}
	timeout := time.Duration(o.timeoutMS) * time.Millisecond
	interval := time.Duration(o.intervalMS) * time.Millisecond
	fmt.Printf("pinging with %d bytes\n", o.size)
	for seq := 1; count == 0 || seq <= count; seq++ {
		start := time.Now()
		recs := make([]record, len(selected))
		var wg sync.WaitGroup
		for i, idx := range selected {
			wg.Go(func() {
				ifc := ifaces[idx]
				rtt, err := ping.Ping(ifc, dsts[i], seq, o.size, timeout)
				recs[i] = record{at: start, ifName: ifc.Name, target: dsts[i], seq: seq, rtt: rtt, err: err}
			})
		}
		wg.Wait()
		res := make([]string, len(recs))
		for i, r := range recs {
			if r.err != nil {
				res[i] = fmt.Sprintf("%s→%s=%v", r.ifName, r.target, r.err)
			} else {
				res[i] = fmt.Sprintf("%s→%s=%s ms", r.ifName, r.target, fmtMS(r.rtt))
			}
			if o.log != nil {
				if err := o.log.write(r); err != nil {
					fatal(err)
				}
			}
		}
		fmt.Printf("seq=%d  %s\n", seq, strings.Join(res, "  "))
		if count == 0 || seq < count {
			time.Sleep(interval - time.Since(start))
		}
	}
}

func orDash(ip fmt.Stringer) string {
	if s := ip.String(); s != "<nil>" {
		return s
	}
	return "-"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "multi-ping:", err)
	os.Exit(1)
}
