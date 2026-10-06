//go:build linux || darwin

package ping

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// ICMP message types.
const (
	echoRequest4 = 8
	echoReply4   = 0
	echoRequest6 = 128
	echoReply6   = 129
)

func ping4(ifc Interface, dst net.IP, seq, size int, timeout time.Duration) (time.Duration, error) {
	fd, err := openSocket(ifc, false)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	id := uint16(rand.Uint32())
	sa := &unix.SockaddrInet4{}
	copy(sa.Addr[:], dst)
	pkt := echoRequest(echoRequest4, id, uint16(seq), size)
	binary.BigEndian.PutUint16(pkt[2:], checksum(pkt))
	return exchange(fd, sa, pkt, timeout, func(b []byte) bool {
		if replyHasIPHeader && len(b) >= 20 && b[0]>>4 == 4 {
			ihl := int(b[0]&0x0f) * 4
			if ihl > len(b) {
				return false
			}
			b = b[ihl:]
		}
		return isReply(b, echoReply4, id, uint16(seq))
	})
}

func ping6(ifc Interface, dst net.IP, seq, size int, timeout time.Duration) (time.Duration, error) {
	fd, err := openSocket(ifc, true)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	id := uint16(rand.Uint32())
	sa := &unix.SockaddrInet6{}
	copy(sa.Addr[:], dst)
	if dst.IsLinkLocalUnicast() || dst.IsLinkLocalMulticast() {
		sa.ZoneId = uint32(ifc.Index)
	}
	// The kernel fills in the ICMPv6 checksum.
	pkt := echoRequest(echoRequest6, id, uint16(seq), size)
	return exchange(fd, sa, pkt, timeout, func(b []byte) bool {
		return isReply(b, echoReply6, id, uint16(seq))
	})
}

// openSocket returns an unprivileged ICMP socket bound to ifc. Every probe
// gets its own socket, so concurrent probes never see each other's replies.
func openSocket(ifc Interface, v6 bool) (int, error) {
	family, proto := unix.AF_INET, unix.IPPROTO_ICMP
	if v6 {
		family, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM, proto)
	if err != nil {
		return -1, fmt.Errorf("icmp socket: %w", err)
	}
	unix.CloseOnExec(fd)
	if err := bindInterface(fd, ifc, v6); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind %s: %w", ifc.Name, err)
	}
	return fd, nil
}

// exchange sends pkt and waits until match accepts a received message.
func exchange(fd int, to unix.Sockaddr, pkt []byte, timeout time.Duration, match func([]byte) bool) (time.Duration, error) {
	start := time.Now()
	if err := unix.Sendto(fd, pkt, 0, to); err != nil {
		return 0, err
	}
	deadline := start.Add(timeout)
	buf := make([]byte, 65535)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, ErrTimeout
		}
		// A zero timeval would mean "block forever".
		left = max(left, time.Millisecond)
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return 0, err
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		rtt := time.Since(start)
		switch {
		case err == unix.EINTR:
			continue
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			return 0, ErrTimeout
		case err != nil:
			return 0, err
		}
		if match(buf[:n]) {
			return rtt, nil
		}
	}
}

func echoRequest(typ byte, id, seq uint16, size int) []byte {
	b := make([]byte, 8+size)
	b[0] = typ
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	for i := 8; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func isReply(b []byte, typ byte, id, seq uint16) bool {
	if len(b) < 8 || b[0] != typ || b[1] != 0 {
		return false
	}
	if replyCheckID && binary.BigEndian.Uint16(b[4:]) != id {
		return false
	}
	return binary.BigEndian.Uint16(b[6:]) == seq
}
