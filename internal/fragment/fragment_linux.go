//go:build linux

package fragment

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// EnableDetection enables ancillary data for fragmented IPv4 packets.
func EnableDetection(conn *net.UDPConn) bool {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	ok := true
	controlErr := rawConn.Control(func(fd uintptr) {
		if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, unix.IP_RECVFRAGSIZE, 1); err != nil {
			ok = false
		}
	})
	if controlErr != nil {
		return false
	}
	return ok
}

// IsFragmentedOOB reports whether recvmsg ancillary data indicates fragmentation.
func IsFragmentedOOB(oob []byte) bool {
	if len(oob) == 0 {
		return false
	}
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return false
	}
	for _, msg := range msgs {
		if msg.Header.Level == syscall.IPPROTO_IP && msg.Header.Type == unix.IP_RECVFRAGSIZE {
			return true
		}
	}
	return false
}
