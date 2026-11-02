package peer

import (
	"context"
	"net"
	"strconv"
	"syscall"
)

// bindHost: the Echo listens on every interface (LAN, and Tailscale as the spare way).
const bindHost = ""

// listen: the link's socket, allowed to send to the broadcast address.
func listen(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var err error
		c.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
		return err
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
