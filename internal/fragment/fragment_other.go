//go:build !linux

package fragment

import "net"

// EnableDetection is unsupported on this platform.
func EnableDetection(_ *net.UDPConn) bool { return false }

// IsFragmentedOOB always reports false on unsupported platforms.
func IsFragmentedOOB(_ []byte) bool { return false }
