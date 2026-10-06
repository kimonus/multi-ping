package ping

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// gateways maps interface names to their IPv4 default gateway, as reported
// by the adapter list. Best effort: an empty map on any failure.
func gateways() map[string]net.IP {
	out := map[string]net.IP{}
	size := uint32(15000)
	var buf []byte
	for range 3 {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(windows.AF_INET, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0,
			(*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return out
		}
		buf = nil
	}
	if buf == nil {
		return out
	}
	for a := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); a != nil; a = a.Next {
		for g := a.FirstGatewayAddress; g != nil; g = g.Next {
			sa := g.Address.Sockaddr
			if sa == nil || sa.Addr.Family != windows.AF_INET {
				continue
			}
			ifi, err := net.InterfaceByIndex(int(a.IfIndex))
			if err != nil {
				break
			}
			in4 := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa))
			out[ifi.Name] = net.IPv4(in4.Addr[0], in4.Addr[1], in4.Addr[2], in4.Addr[3]).To4()
			break
		}
	}
	return out
}
