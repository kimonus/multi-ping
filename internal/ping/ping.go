// Package ping sends single ICMP echo requests through a chosen network
// interface. Each OS has its own backend; none of them needs elevated
// privileges on a default install.
package ping

import (
	"errors"
	"net"
	"strings"
	"time"
)

// ErrTimeout is returned by Ping when no reply arrived in time.
var ErrTimeout = errors.New("timeout")

// Interface is a network interface that can be used as a ping source.
type Interface struct {
	Name    string
	Index   int
	IP      net.IP // first IPv4 address, nil if none
	IP6     net.IP // a global IPv6 address if there is one, else link-local, else nil
	Gateway net.IP // IPv4 default gateway reached through this interface, nil if none
}

// Addr returns the interface's source address for the given family.
func (i Interface) Addr(v6 bool) net.IP {
	if v6 {
		return i.IP6
	}
	return i.IP
}

// Interfaces lists interfaces that are up, not loopback, and have an IPv4 or
// a global IPv6 address. Link-local IPv6 alone does not qualify, which keeps
// virtual and tunnel devices out of the list.
func Interfaces() ([]Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	gws := gateways()
	var out []Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		ifc := Interface{Name: ifi.Name, Index: ifi.Index, Gateway: gws[ifi.Name]}
		global6 := false
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			switch ip4 := ip.To4(); {
			case ip4 != nil:
				if ifc.IP == nil {
					ifc.IP = ip4
				}
			case ip.IsLinkLocalUnicast():
				if ifc.IP6 == nil {
					ifc.IP6 = ip
				}
			case !global6:
				ifc.IP6, global6 = ip, true
			}
		}
		// Tunnels list their own address as the gateway; there is no router to ping.
		if ifc.Gateway.Equal(ifc.IP) {
			ifc.Gateway = nil
		}
		if ifc.IP != nil || global6 {
			out = append(out, ifc)
		}
	}
	return out, nil
}

// Resolve turns a host name or literal into an address. When the name has
// both families, prefer6 picks which one wins.
func Resolve(host string, prefer6 bool) (net.IP, error) {
	// The zone of a link-local literal is implied by the panel's interface.
	host, _, _ = strings.Cut(host, "%")
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	var pick net.IP
	for _, ip := range ips {
		is6 := ip.To4() == nil
		if pick == nil || (is6 == prefer6 && (pick.To4() == nil) != prefer6) {
			pick = ip
		}
	}
	if pick == nil {
		return nil, errors.New("no address for " + host)
	}
	if ip4 := pick.To4(); ip4 != nil {
		return ip4, nil
	}
	return pick, nil
}

// Ping sends one echo request with size payload bytes to dst through ifc and
// waits up to timeout for the reply. Concurrent calls are independent.
func Ping(ifc Interface, dst net.IP, seq, size int, timeout time.Duration) (time.Duration, error) {
	if dst4 := dst.To4(); dst4 != nil {
		if ifc.IP == nil {
			return 0, errors.New("no IPv4 address on " + ifc.Name)
		}
		return ping4(ifc, dst4, seq, size, timeout)
	}
	if ifc.IP6 == nil {
		return 0, errors.New("no IPv6 address on " + ifc.Name)
	}
	return ping6(ifc, dst.To16(), seq, size, timeout)
}
