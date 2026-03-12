//go:build windows

package frontend

import (
	"net"
	"syscall"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// newListenConfig returns a net.ListenConfig that sets SO_RCVBUF and SO_SNDBUF
// on Windows. SO_REUSEPORT is not supported on Windows.
func newListenConfig(socketCfg config.SocketConfig) net.ListenConfig {
	return net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var lastErr error
			_ = c.Control(func(fd uintptr) {
				handle := syscall.Handle(fd)
				if socketCfg.RcvBuf > 0 {
					if err := syscall.SetsockoptInt(handle, syscall.SOL_SOCKET, syscall.SO_RCVBUF, socketCfg.RcvBuf); err != nil {
						lastErr = err
					}
				}
				if socketCfg.SndBuf > 0 {
					if err := syscall.SetsockoptInt(handle, syscall.SOL_SOCKET, syscall.SO_SNDBUF, socketCfg.SndBuf); err != nil {
						lastErr = err
					}
				}
			})
			return lastErr
		},
	}
}
