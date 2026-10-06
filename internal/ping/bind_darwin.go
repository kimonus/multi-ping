package ping

import "golang.org/x/sys/unix"

// macOS ICMP datagram sockets deliver the IPv4 header too and see replies
// meant for other sockets, so the echo ID has to be checked.
const (
	replyHasIPHeader = true
	replyCheckID     = true
)

func bindInterface(fd int, ifc Interface, v6 bool) error {
	if v6 {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifc.Index)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, ifc.Index)
}
