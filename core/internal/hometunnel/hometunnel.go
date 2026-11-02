// Package hometunnel: the phone's way home (#75, devices/atoms3r/docs/mobile.md). The phone's
// The assistant reaches the house's services through local ports; each connection goes on over TLS with
// the phone's client certificate to the home gateway (the Raspberry Pi), first in the LAN, else over
// the VPS, whose public port is passed through the WireGuard link to the Pi. The gateway checks the
// certificate (one per device, revoked by taking its name off the gateway's list) and picks the
// service by the name asked for (SNI): rpc.jarvis (the primary's peer calls), mqtt.jarvis (the
// broker), ha.jarvis (Home Assistant). No VPN on the phone; nothing is open while nothing is used.
package hometunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The service names the gateway routes by (services/home-gateway).
const (
	RPC  = "rpc.assistant"
	MQTT = "mqtt.assistant"
	HA   = "ha.assistant"
)

// Files in dir (state_dir/home, put there by devices/android/tools/install_phone.sh).
const (
	caFile   = "ca.crt"
	certFile = "client.crt"
	keyFile  = "client.key"
)

// dialTimeout per endpoint: the LAN address answers at once at home and not at all away.
const dialTimeout = 3 * time.Second

// Tunnel: the endpoints and the phone's TLS identity.
type Tunnel struct {
	endpoints []string
	tls       *tls.Config
	good      atomic.Int32 // the endpoint that worked last: tried first
	mu        sync.Mutex
	reported  string
}

// New: endpoints "host:port" in order (the gateway in the LAN, then the VPS); the certificates from
// dir. Nil and no error without endpoints (the tunnel is not set up).
func New(endpoints, dir string) (*Tunnel, error) {
	var list []string
	for _, e := range strings.Split(endpoints, ",") {
		if e = strings.TrimSpace(e); e != "" {
			list = append(list, e)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, caFile))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.crt: no certificate")
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
	if err != nil {
		return nil, err
	}
	return &Tunnel{endpoints: list, tls: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert},
		MinVersion: tls.VersionTLS12}}, nil
}

// Listen forwards connections to 127.0.0.1:port to the gateway's service name, until ctx ends.
func (t *Tunnel) Listen(ctx context.Context, port int, name string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("[Home tunnel] %s: %v", name, err)
				}
				return
			}
			go t.forward(c, name)
		}
	}()
	return nil
}

func (t *Tunnel) forward(local net.Conn, name string) {
	defer local.Close()
	remote, err := t.Dial(name)
	if err != nil {
		t.report(fmt.Sprintf("%s: %v", name, err))
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(remote, local)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(local, remote)
		if cw, ok := local.(*net.TCPConn); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// Dial: a TLS connection to the gateway for the service name, over the endpoint that worked last
// first, then the others.
func (t *Tunnel) Dial(name string) (net.Conn, error) {
	first := int(t.good.Load())
	var errs []string
	for i := range t.endpoints {
		n := (first + i) % len(t.endpoints)
		cfg := t.tls.Clone()
		cfg.ServerName = name
		d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
		c, err := tls.DialWithDialer(d, "tcp", t.endpoints[n], cfg)
		if err == nil {
			if n != first {
				log.Printf("[Home tunnel] over %s", t.endpoints[n])
			}
			t.good.Store(int32(n))
			t.report("")
			return c, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

// report logs a failure once until the tunnel works again.
func (t *Tunnel) report(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if text != "" && text != t.reported {
		log.Printf("[Home tunnel] %s", text)
	}
	t.reported = text
}
