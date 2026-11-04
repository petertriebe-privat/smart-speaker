// Package netx: network helpers for the Echo. The old kernel has no IPv6 socket support, so
// every connection goes over IPv4; the stock root CAs are old, so Mozilla's are embedded.
package netx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

var roots = func() *x509.CertPool {
	pool := x509.NewCertPool()
	for cert := range bundle.Roots() {
		c, err := x509.ParseCertificate(cert.Certificate)
		if err != nil {
			continue
		}
		if cert.Constraint != nil {
			pool.AddCertWithConstraint(c, cert.Constraint)
		} else {
			pool.AddCert(c)
		}
	}
	return pool
}()

// TLSConfig for outgoing connections.
func TLSConfig() *tls.Config { return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12} }

var dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// Dial connects over IPv4 only.
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp6":
		network = "tcp4"
	case "udp", "udp6":
		network = "udp4"
	}
	return dialer.DialContext(ctx, network, addr)
}

// Transport for HTTP clients: IPv4, embedded CAs.
func Transport() *http.Transport {
	return &http.Transport{
		DialContext:         Dial,
		TLSClientConfig:     TLSConfig(),
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 4,
		ForceAttemptHTTP2:   true,
	}
}

// Client with a timeout.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: Transport(), Timeout: timeout}
}

// TimeSynced: the clock was set this run (the Echo starts in 1970: TLS fails before).
var TimeSynced bool

// SyncTime sets the system clock via SNTP; tries the servers in turn.
func SyncTime(servers ...string) (time.Time, error) {
	lastErr := errors.New("no servers")
	for _, server := range servers {
		t, err := sntp(server)
		if err != nil {
			lastErr = err
			continue
		}
		if err := setClock(t); err != nil {
			return t, err
		}
		TimeSynced = true
		return t, nil
	}
	return time.Time{}, lastErr
}

func sntp(server string) (time.Time, error) {
	conn, err := Dial(context.Background(), "udp", net.JoinHostPort(server, "123"))
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	req := make([]byte, 48)
	req[0] = 0x1b // LI 0, version 3, client
	sent := time.Now()
	if _, err := conn.Write(req); err != nil {
		return time.Time{}, err
	}
	resp := make([]byte, 48)
	if n, err := conn.Read(resp); err != nil || n < 48 {
		if err == nil {
			err = errors.New("short reply")
		}
		return time.Time{}, err
	}
	rtt := time.Since(sent)
	secs := binary.BigEndian.Uint32(resp[40:])
	frac := binary.BigEndian.Uint32(resp[44:])
	if secs == 0 {
		return time.Time{}, errors.New("server not synchronized")
	}
	const ntpEpoch = 2208988800
	t := time.Unix(int64(secs)-ntpEpoch, int64(frac)*1e9>>32)
	return t.Add(rtt / 2), nil
}
