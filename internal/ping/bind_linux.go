package ping

import "golang.org/x/sys/unix"

// Linux ping sockets deliver the bare ICMP message, rewrite the echo ID to a
// per-socket value and only hand a socket its own replies.
const (
	replyHasIPHeader = false
	replyCheckID     = false
)

func bindInterface(fd int, ifc Interface, v6 bool) error {
	return unix.BindToDevice(fd, ifc.Name)
}
