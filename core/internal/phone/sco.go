package phone

import (
	"bufio"
	"encoding/binary"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// The audio of a call on the mobile phone (issue #51): with SCORouting=PCM the Bluetooth chip
// hands it to ALSA device hw:0,2, S16_LE at 8 kHz mono - the same telephone band as the
// landline, without G.711. The chip clocks the device only while SCO runs; before that a read
// ends with an I/O error. arecord and aplay as their own processes, as pacat for the speaker:
// no cgo, and a hung device cannot block the assistant.

// scoLine: one call's audio; *scoProc satisfies it, the tests fake it.
type scoLine interface {
	// Start: deliver gets the far end, 8 kHz, about every 20 ms, from its own goroutine.
	Start(deliver func(pcm8 []int16)) error
	// Write: the room for the far end, 8 kHz. Never blocks: what does not fit is dropped.
	Write(pcm8 []int16)
	// Lead: how far the way to the phone runs ahead of real time, and the frames dropped for it.
	Lead() (time.Duration, int64)
	Stop()
}

// scoLead: the way to the phone keeps at most this much ahead of real time. aplay's stdin is a
// pipe of 64 KB - four seconds at 8 kHz - and whatever stalls aplay once (its start, an underrun)
// fills it and stays as delay: the caller heard herself three seconds late (17.10.2026). The
// landline caps the same way (micLag, the RTP sender's own clock).
var scoLead = ms("MOBILE_OUT_LEAD_MS", 160)

const scoDevice = "hw:0,2"

// Small buffers both ways: aplay's default is half a second, which the far end would hear as
// delay (and the room's echo that much later, see the gate in mic.go).
var scoArgs = []string{"-q", "-D", scoDevice, "-f", "S16_LE", "-r", "8000", "-c", "1", "-t", "raw",
	"-B", "80000", "-F", "20000"}

type scoProc struct {
	mu     sync.Mutex
	rec    *exec.Cmd
	play   *exec.Cmd
	out    chan []int16 // to aplay; a few frames, the rest is dropped (micLag on the landline)
	closed bool

	lead    atomic.Int64 // ns ahead of real time, the writer's last look
	dropped atomic.Int64
}

// pacer: what goes to aplay never runs more than scoLead ahead of real time since the start.
type pacer struct {
	start   time.Time
	written int64 // samples at 8 kHz
}

// admit: whether n samples may go now, and how far ahead the line is before them.
func (p *pacer) admit(now time.Time, n int) (time.Duration, bool) {
	if p.start.IsZero() {
		p.start = now
	}
	ahead := time.Duration(p.written)*time.Second/lineRate - now.Sub(p.start)
	if ahead < 0 { // aplay ran dry (nothing came): real time starts over from here
		p.start, p.written, ahead = now, 0, 0
	}
	if ahead > scoLead {
		return ahead, false
	}
	p.written += int64(n)
	return ahead, true
}

func (s *scoProc) Lead() (time.Duration, int64) {
	return time.Duration(s.lead.Load()), s.dropped.Swap(0)
}

func newSCO() scoLine { return &scoProc{} }

func (s *scoProc) Start(deliver func([]int16)) error {
	rec := exec.Command("/usr/bin/arecord", scoArgs...)
	stdout, err := rec.StdoutPipe()
	if err != nil {
		return err
	}
	play := exec.Command("/usr/bin/aplay", append(scoArgs, "-")...)
	stdin, err := play.StdinPipe()
	if err != nil {
		return err
	}
	if err := rec.Start(); err != nil {
		return err
	}
	if err := play.Start(); err != nil {
		rec.Process.Kill()
		rec.Wait()
		return err
	}
	s.mu.Lock()
	s.rec, s.play, s.out = rec, play, make(chan []int16, 4)
	out := s.out
	s.mu.Unlock()
	go func() {
		r := bufio.NewReaderSize(stdout, 4*frameLen)
		buf := make([]byte, frameLen*2)
		for {
			if _, err := io.ReadFull(r, buf); err != nil {
				log.Printf("[Mobile] SCO in ended: %v", err)
				rec.Wait()
				return
			}
			pcm := make([]int16, frameLen)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			deliver(pcm)
		}
	}()
	go func() {
		defer play.Wait()
		defer stdin.Close()
		var pace pacer
		for pcm := range out {
			ahead, ok := pace.admit(time.Now(), len(pcm))
			s.lead.Store(int64(ahead))
			if !ok {
				s.dropped.Add(1)
				continue
			}
			buf := make([]byte, len(pcm)*2)
			for i, v := range pcm {
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
			}
			if _, err := stdin.Write(buf); err != nil {
				log.Printf("[Mobile] SCO out ended: %v", err)
				for range out { // drain until Stop closes it
				}
				return
			}
		}
	}()
	return nil
}

func (s *scoProc) Write(pcm8 []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.out == nil || s.closed {
		return
	}
	select {
	case s.out <- pcm8:
	default: // aplay does not keep up: late audio is worth nothing on a call
	}
}

func (s *scoProc) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.out != nil {
		close(s.out)
	}
	for _, cmd := range []*exec.Cmd{s.rec, s.play} {
		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}
