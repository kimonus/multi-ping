package ping

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
	procIcmp6CreateFile = iphlpapi.NewProc("Icmp6CreateFile")
	procIcmp6SendEcho2  = iphlpapi.NewProc("Icmp6SendEcho2")
)

// IP_STATUS values from ipexport.h.
const (
	ipSuccess             = 0
	ipDestNetUnreachable  = 11002
	ipDestHostUnreachable = 11003
	ipReqTimedOut         = 11010
	ipTTLExpiredTransit   = 11013
	ipGeneralFailure      = 11050
)

// Reply layouts. ICMP_ECHO_REPLY is 40 bytes on 64-bit Windows (smaller on
// 32-bit) with Status at offset 4. ICMPV6_ECHO_REPLY starts with a packed
// 26-byte address, so Status sits at offset 28 and the struct is 36 bytes.
const (
	echoReplySize    = 40
	echoStatusOff    = 4
	echo6ReplySize   = 36
	echo6StatusOff   = 28
	ioStatusBlockLen = 16
)

// ping4 uses the interface's address as the source. Windows routes by source
// address, which pins the request to that interface.
func ping4(ifc Interface, dst4 net.IP, seq, size int, timeout time.Duration) (time.Duration, error) {
	src4 := ifc.IP.To4()
	h, _, err := procIcmpCreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("IcmpCreateFile: %w", err)
	}
	defer procIcmpCloseHandle.Call(h)

	data := make([]byte, max(size, 1))
	for i := range data {
		data[i] = byte(i)
	}
	reply := make([]byte, echoReplySize+size+8+ioStatusBlockLen)
	ms := max(timeout.Milliseconds(), 1)

	start := time.Now()
	n, _, err := procIcmpSendEcho2Ex.Call(h, 0, 0, 0,
		uintptr(binary.LittleEndian.Uint32(src4)),
		uintptr(binary.LittleEndian.Uint32(dst4)),
		uintptr(unsafe.Pointer(&data[0])), uintptr(size),
		0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)),
		uintptr(ms))
	// The API reports whole milliseconds only, so time it ourselves.
	rtt := time.Since(start)

	return result(n, err, reply[echoStatusOff:], rtt)
}

func ping6(ifc Interface, dst net.IP, seq, size int, timeout time.Duration) (time.Duration, error) {
	h, _, err := procIcmp6CreateFile.Call()
	if windows.Handle(h) == windows.InvalidHandle {
		return 0, fmt.Errorf("Icmp6CreateFile: %w", err)
	}
	defer procIcmpCloseHandle.Call(h)

	sockaddr := func(ip net.IP) *windows.RawSockaddrInet6 {
		sa := &windows.RawSockaddrInet6{Family: windows.AF_INET6}
		copy(sa.Addr[:], ip.To16())
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			sa.Scope_id = uint32(ifc.Index)
		}
		return sa
	}
	src, to := sockaddr(ifc.IP6), sockaddr(dst)
	data := make([]byte, max(size, 1))
	for i := range data {
		data[i] = byte(i)
	}
	reply := make([]byte, echo6ReplySize+size+8+ioStatusBlockLen)
	ms := max(timeout.Milliseconds(), 1)

	start := time.Now()
	n, _, err := procIcmp6SendEcho2.Call(h, 0, 0, 0,
		uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(to)),
		uintptr(unsafe.Pointer(&data[0])), uintptr(size),
		0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)),
		uintptr(ms))
	rtt := time.Since(start)
	return result(n, err, reply[echo6StatusOff:], rtt)
}

// result turns the outcome of an echo call into Ping's return values. n is
// the reply count, err the call's last error, status the reply's Status field.
func result(n uintptr, err error, statusField []byte, rtt time.Duration) (time.Duration, error) {
	status := uint32(ipGeneralFailure)
	if n != 0 {
		status = binary.LittleEndian.Uint32(statusField)
	} else if errno, ok := err.(syscall.Errno); ok {
		status = uint32(errno)
	}
	switch status {
	case ipSuccess:
		return rtt, nil
	case ipReqTimedOut:
		return 0, ErrTimeout
	case ipDestNetUnreachable:
		return 0, errors.New("network unreachable")
	case ipDestHostUnreachable:
		return 0, errors.New("host unreachable")
	case ipTTLExpiredTransit:
		return 0, errors.New("ttl expired")
	case ipGeneralFailure:
		return 0, errors.New("general failure")
	}
	return 0, fmt.Errorf("icmp status %d", status)
}
