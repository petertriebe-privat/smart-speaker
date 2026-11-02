package netx

import (
	"cmp"
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
)

// Android (the Android apps, board.Android) has no /etc/resolv.conf: Go's resolver would ask
// localhost and find nothing. The app passes the network's DNS servers in ASSISTANT_DNS
// ("192.168.1.1,1.1.1.1"); without them public ones. The phone changes networks: its app writes
// the current network's servers into ASSISTANT_DNS_FILE at every change, read at each lookup. With the
// home Wi-Fi's router kept as the first server, nothing resolved on mobile data (01.11.2026): a
// UDP "connection" to it never fails, the question only goes unanswered.
func init() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		return
	}
	fixed := parseServers(cmp.Or(os.Getenv("ASSISTANT_DNS"), "1.1.1.1,8.8.8.8"))
	file := os.Getenv("ASSISTANT_DNS_FILE")
	var next atomic.Uint32
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		servers := fixed
		if file != "" {
			if data, err := os.ReadFile(file); err == nil {
				if s := parseServers(string(data)); len(s) > 0 {
					servers = s
				}
			}
		}
		// Each dial starts at the next server: the resolver's retry of an unanswered question
		// goes to another one.
		start := int(next.Add(1) - 1)
		var d net.Dialer
		var last error
		for i := range servers {
			conn, err := d.DialContext(ctx, network, servers[(start+i)%len(servers)])
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
}

func parseServers(list string) []string {
	var servers []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			servers = append(servers, net.JoinHostPort(s, "53"))
		}
	}
	return servers
}
