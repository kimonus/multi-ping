package ping

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

// gateways maps interface names to their IPv4 default gateway, read from the
// kernel routing table. Best effort: an empty map on any failure.
func gateways() map[string]net.IP {
	out := map[string]net.IP{}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return out
	}
	defer f.Close()
	const rtfGateway = 0x2
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Iface Destination Gateway Flags ...
		fld := strings.Fields(sc.Text())
		if len(fld) < 4 || fld[1] != "00000000" {
			continue
		}
		flags, _ := strconv.ParseUint(fld[3], 16, 32)
		gw, err := hex.DecodeString(fld[2])
		if flags&rtfGateway == 0 || err != nil || len(gw) != 4 {
			continue
		}
		if _, dup := out[fld[0]]; !dup {
			// The address is printed as a host-order (little-endian) integer.
			out[fld[0]] = net.IPv4(gw[3], gw[2], gw[1], gw[0]).To4()
		}
	}
	return out
}
