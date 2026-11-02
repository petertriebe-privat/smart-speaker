// Package mqtt: a small MQTT 3.1.1 client for the LAN (no TLS, QoS 0 only), enough for Home
// Assistant's MQTT discovery (#65): publish (retained), subscribe, a last will, keepalive and
// reconnects. No dependency: the Echo's binary stays small.
package mqtt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Message: one PUBLISH received.
type Message struct {
	Topic   string
	Payload []byte
}

// Options of a client. URL: mqtt://host[:1883].
type Options struct {
	URL, User, Password, ClientID string
	// Will: published by the broker when the connection dies (availability "offline").
	WillTopic, WillPayload string
	KeepAlive              time.Duration // 0 = 60 s
	// OnConnect runs after every (re)connect, before messages arrive: subscribe and publish the
	// retained state there.
	OnConnect func(c *Client)
	// OnMessage gets every PUBLISH of the subscriptions (on the reader goroutine: do not block).
	OnMessage func(m Message)
}

// Client keeps one connection to the broker.
type Client struct {
	opt  Options
	addr string

	mu     sync.Mutex
	conn   net.Conn
	nextID uint16
}

func New(opt Options) (*Client, error) {
	u, err := url.Parse(opt.URL)
	if err != nil || u.Scheme != "mqtt" && u.Scheme != "tcp" || u.Hostname() == "" {
		return nil, fmt.Errorf("mqtt: URL %q: want mqtt://host[:port]", opt.URL)
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), "1883")
	}
	if opt.KeepAlive == 0 {
		opt.KeepAlive = 60 * time.Second
	}
	return &Client{opt: opt, addr: addr}, nil
}

// Run connects and reconnects forever (retry: the wait after a failure). Start as a goroutine.
func (c *Client) Run(retry time.Duration) {
	for {
		err := c.session()
		log.Printf("[MQTT] %v, reconnecting in %s", err, retry)
		time.Sleep(retry)
	}
}

// Connected: a connection to the broker stands.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) session() error {
	conn, err := net.DialTimeout("tcp", c.addr, 10*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(connectPacket(c.opt)); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	kind, body, err := readPacket(r)
	if err != nil {
		return err
	}
	if kind != 2 || len(body) != 2 {
		return fmt.Errorf("unexpected packet %d instead of CONNACK", kind)
	}
	if body[1] != 0 {
		return fmt.Errorf("broker refused the login (code %d)", body[1])
	}
	conn.SetDeadline(time.Time{})
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
	}()
	log.Printf("[MQTT] connected to %s", c.addr)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(c.opt.KeepAlive / 2):
				c.write([]byte{0xC0, 0}) // PINGREQ
			}
		}
	}()
	if c.opt.OnConnect != nil {
		c.opt.OnConnect(c)
	}
	for {
		// The broker answers a ping within the keepalive; silence longer than that is a dead link.
		conn.SetReadDeadline(time.Now().Add(c.opt.KeepAlive + 15*time.Second))
		kind, body, err := readPacket(r)
		if err != nil {
			return err
		}
		if kind == 3 && c.opt.OnMessage != nil {
			if m, ok := parsePublish(body); ok {
				c.opt.OnMessage(m)
			}
		}
	}
}

// Publish sends a QoS 0 message; an error when not connected (the next OnConnect publishes
// the state again).
func (c *Client) Publish(topic string, payload []byte, retain bool) error {
	return c.write(publishPacket(topic, payload, retain))
}

// Subscribe asks for topics at QoS 0 (wildcards allowed).
func (c *Client) Subscribe(topics ...string) error {
	c.mu.Lock()
	c.nextID++
	if c.nextID == 0 {
		c.nextID = 1
	}
	id := c.nextID
	c.mu.Unlock()
	return c.write(subscribePacket(id, topics))
}

func (c *Client) write(packet []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return errors.New("mqtt: not connected")
	}
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(packet)
	if err != nil {
		c.conn.Close() // the reader ends the session
	}
	return err
}

// --- Packets ------------------------------------------------------------------------------

func appendString(b []byte, s string) []byte {
	return append(append(b, byte(len(s)>>8), byte(len(s))), s...)
}

func appendLength(b []byte, n int) []byte {
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		b = append(b, digit)
		if n == 0 {
			return b
		}
	}
}

func packet(header byte, body []byte) []byte {
	return append(appendLength([]byte{header}, len(body)), body...)
}

func connectPacket(o Options) []byte {
	flags := byte(0x02) // clean session
	if o.WillTopic != "" {
		flags |= 0x04 | 0x20 // will, will retain, QoS 0
	}
	if o.User != "" {
		flags |= 0x80
		if o.Password != "" {
			flags |= 0x40
		}
	}
	keep := int(o.KeepAlive / time.Second)
	b := appendString(nil, "MQTT")
	b = append(b, 4, flags, byte(keep>>8), byte(keep))
	b = appendString(b, o.ClientID)
	if o.WillTopic != "" {
		b = appendString(appendString(b, o.WillTopic), o.WillPayload)
	}
	if o.User != "" {
		b = appendString(b, o.User)
		if o.Password != "" {
			b = appendString(b, o.Password)
		}
	}
	return packet(0x10, b)
}

func publishPacket(topic string, payload []byte, retain bool) []byte {
	header := byte(0x30)
	if retain {
		header |= 0x01
	}
	return packet(header, append(appendString(nil, topic), payload...))
}

func subscribePacket(id uint16, topics []string) []byte {
	b := []byte{byte(id >> 8), byte(id)}
	for _, t := range topics {
		b = append(appendString(b, t), 0)
	}
	return packet(0x82, b)
}

// readPacket: the packet type (upper nibble; for PUBLISH the flags are dropped) and its body.
func readPacket(r *bufio.Reader) (byte, []byte, error) {
	header, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, shift := 0, 0
	for {
		digit, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n |= int(digit&0x7F) << shift
		if digit&0x80 == 0 {
			break
		}
		if shift += 7; shift > 21 {
			return 0, nil, errors.New("mqtt: bad remaining length")
		}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	kind := header >> 4
	if kind == 3 && header&0x06 != 0 {
		return kind, nil, nil // QoS > 0 never subscribed: ignored
	}
	return kind, body, nil
}

func parsePublish(body []byte) (Message, bool) {
	if len(body) < 2 {
		return Message{}, false
	}
	n := int(body[0])<<8 | int(body[1])
	if len(body) < 2+n {
		return Message{}, false
	}
	return Message{Topic: string(body[2 : 2+n]), Payload: body[2+n:]}, true
}

// TopicID makes a name safe for a topic segment and an HA object id: lower case, [a-z0-9_].
func TopicID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'ä':
			b.WriteString("ae")
		case r == 'ö':
			b.WriteString("oe")
		case r == 'ü':
			b.WriteString("ue")
		case r == 'ß':
			b.WriteString("ss")
		default:
			if s := b.String(); s != "" && !strings.HasSuffix(s, "_") {
				b.WriteByte('_')
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
