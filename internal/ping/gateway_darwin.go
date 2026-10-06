package ping

import (
	"net"
	"os/exec"
	"strings"
)

// gateways maps interface names to their IPv4 default gateway, as listed by
// netstat. Best effort: an empty map on any failure.
func gateways() map[string]net.IP {
	out := map[string]net.IP{}
	b, err := exec.Command("netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return out
	}
	for line := range strings.Lines(string(b)) {
		// Destination Gateway Flags Netif [Expire]
		fld := strings.Fields(line)
		if len(fld) < 4 || fld[0] != "default" {
			continue
		}
		gw := net.ParseIP(fld[1]).To4()
		if _, dup := out[fld[3]]; gw != nil && !dup {
			out[fld[3]] = gw
		}
	}
	return out
}
