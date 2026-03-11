//go:build linux

package frontend

import (
	"net"
	"syscall"

	"github.com/ErwinsExpertise/light-udp-proxy/internal/config"
)

// soReusePort is SO_REUSEPORT on Linux (decimal 15 / 0xf). The standard
// library's syscall package does not export this constant, so we define it here.
const soReusePort = 15

// newListenConfig returns a net.ListenConfig whose Control hook applies the
// requested kernel socket options before the socket is bound.
// On Linux this supports SO_RCVBUF, SO_SNDBUF, and SO_REUSEPORT.
func newListenConfig(socketCfg config.SocketConfig) net.ListenConfig {
	return net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var lastErr error
			_ = c.Control(func(fd uintptr) {
				ifd := int(fd)
				if socketCfg.RcvBuf > 0 {
					if err := syscall.SetsockoptInt(ifd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, socketCfg.RcvBuf); err != nil {
						lastErr = err
					}
				}
				if socketCfg.SndBuf > 0 {
					if err := syscall.SetsockoptInt(ifd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, socketCfg.SndBuf); err != nil {
						lastErr = err
					}
				}
				if socketCfg.ReusePort {
					if err := syscall.SetsockoptInt(ifd, syscall.SOL_SOCKET, soReusePort, 1); err != nil {
						lastErr = err
					}
				}
			})
			return lastErr
		},
	}
}
