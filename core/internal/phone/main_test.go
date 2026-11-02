package phone

import (
	"net"
	"os"
	"testing"
)

// TestMain: the tests talk over the loopback only (see listenIP).
func TestMain(m *testing.M) {
	listenIP = net.IPv4(127, 0, 0, 1)
	os.Exit(m.Run())
}
