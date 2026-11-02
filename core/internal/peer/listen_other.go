//go:build !linux

package peer

import "net"

// bindHost: tests on the PC listen on the loopback only (unicast to configured addresses); on
// all interfaces every new test binary raised a firewall prompt on Windows.
const bindHost = "127.0.0.1"

// listen: tests on the PC (no broadcast option).
func listen(port int) (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(bindHost), Port: port})
}
