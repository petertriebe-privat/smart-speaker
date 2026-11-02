package audio

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// On the Fire TV (board.FireTV) the assistant app (devices/android/firetv) is microphone and speaker.
// The assistant reaches it over local TCP at ASSISTANT_AUDIO (e.g. 127.0.0.1:47160), one connection each:
//
//	"mic\n"                       the app streams 16 kHz s16le mono in real time: the remote's
//	                              microphone while its key is held, silence otherwise
//	"play <rate> <channels> <name>\n" then s16le samples: a playback stream (an AudioTrack);
//	                              closing the connection stops it at once
//	"keys\n"                      the phone app's assistant button, lines both ways
//	                              (cmd/assistant/phoneboard.go)
//
// The phone app (board.Phone) speaks the same protocol.
var tvAddr = os.Getenv("ASSISTANT_AUDIO")

// OnTV: the app plays and records (ASSISTANT_AUDIO set).
func OnTV() bool { return tvAddr != "" }

const tvMicRing = 1 << 18 // 16 s

type tvMic struct {
	ring    []int16
	written atomic.Uint64
}

// TVMic: the app's microphone stream; it reconnects when the app restarts its side.
func TVMic() *Mic {
	t := &tvMic{ring: make([]int16, tvMicRing)}
	go t.run()
	return &Mic{tv: t}
}

func (t *tvMic) run() {
	for {
		err := t.read()
		log.Printf("[Audio] TV microphone: %v, again in 1 s", err)
		time.Sleep(time.Second)
	}
}

func (t *tvMic) read() error {
	conn, err := net.DialTimeout("tcp", tvAddr, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "mic\n"); err != nil {
		return err
	}
	r := bufio.NewReaderSize(conn, 4096)
	buf := make([]byte, 640) // 20 ms
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		n := t.written.Load()
		for i := 0; i+1 < len(buf); i += 2 {
			t.ring[(n+uint64(i/2))%tvMicRing] = int16(uint16(buf[i]) | uint16(buf[i+1])<<8)
		}
		t.written.Add(uint64(len(buf) / 2))
	}
}

// AppKeys opens the phone app's button connection.
func AppKeys() (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", tvAddr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, "keys\n"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// tvStream opens a playback stream in the app.
func tvStream(rate, channels int, name string) (io.WriteCloser, error) {
	conn, err := net.DialTimeout("tcp", tvAddr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("TV speaker: %w", err)
	}
	if _, err := fmt.Fprintf(conn, "play %d %d %s\n", rate, channels, name); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
