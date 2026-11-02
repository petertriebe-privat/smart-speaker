package hometunnel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type issued struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func issue(t *testing.T, cn string, parent *issued, server bool) *issued {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	if parent == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else if server {
		tmpl.DNSNames, tmpl.ExtKeyUsage = []string{RPC, MQTT, HA}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &issued{cert: cert, key: key, der: der}
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gateway: a TLS server as the home gateway: client certificates of the CA only; it answers each
// connection with the name asked for.
func gateway(t *testing.T, ca, server *issued) string {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{{Certificate: [][]byte{server.der}, PrivateKey: server.key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := c.(*tls.Conn)
				if tc.Handshake() != nil {
					return
				}
				io.WriteString(c, tc.ConnectionState().ServerName)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func TestTunnel(t *testing.T) {
	ca := issue(t, "test CA", nil, false)
	addr := gateway(t, ca, issue(t, "gateway", ca, true))
	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, caFile), "CERTIFICATE", ca.der)
	client := issue(t, "phone", ca, false)
	writePEM(t, filepath.Join(dir, certFile), "CERTIFICATE", client.der)
	keyDER, _ := x509.MarshalECPrivateKey(client.key)
	writePEM(t, filepath.Join(dir, keyFile), "EC PRIVATE KEY", keyDER)

	if tun, err := New("", dir); tun != nil || err != nil {
		t.Fatalf("no endpoints: %v %v", tun, err)
	}
	// The first endpoint is away (nothing listens): the second one is taken and kept first.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	tun, err := New(deadAddr+", "+addr, dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tun.Dial(MQTT)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != MQTT {
		t.Fatalf("the gateway saw %q", got)
	}
	if tun.good.Load() != 1 {
		t.Fatalf("the working endpoint is not tried first: %d", tun.good.Load())
	}

	// A certificate of another CA is refused.
	other := issue(t, "other CA", nil, false)
	stranger := issue(t, "stranger", other, false)
	writePEM(t, filepath.Join(dir, certFile), "CERTIFICATE", stranger.der)
	strangerKey, _ := x509.MarshalECPrivateKey(stranger.key)
	writePEM(t, filepath.Join(dir, keyFile), "EC PRIVATE KEY", strangerKey)
	tun, err = New(addr, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := tun.Dial(RPC); err == nil {
		// TLS 1.3 reports the refusal on the first read
		if b, _ := io.ReadAll(c); len(b) > 0 {
			t.Fatal("a stranger's certificate was taken")
		}
		c.Close()
	}
}
