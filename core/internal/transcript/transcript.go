// Package transcript collects what was said in a conversation, as
// devices/atoms3r/src/voice/transcript.cpp: deltas become lines, lines go to the log and, for the
// memory, into one text ("Du: ...", "Jarvis: ...").
package transcript

import (
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
)

type Speaker int

const (
	None Speaker = iota
	User
	Assistant
)

const linePause = 1500 * time.Millisecond

type Transcript struct {
	mu         sync.Mutex
	line       strings.Builder
	all        strings.Builder
	last       Speaker
	userName   string
	lastAppend time.Time
	limit      int
	// OnLine, if set, gets the current line after every delta (live captions); called with the
	// lock held: it must not call back.
	OnLine func(s Speaker, line string)
}

func New() *Transcript {
	return &Transcript{userName: "Du", limit: prompts.Int("MEMORY_TRANSCRIPT_CHARS", 16000)}
}

func (t *Transcript) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.flushLocked()
	t.all.Reset()
	t.last = None
}

func (t *Transcript) SetUserName(name string) {
	t.mu.Lock()
	t.userName = name
	t.mu.Unlock()
}

func (t *Transcript) Append(s Speaker, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s != t.last {
		t.flushLocked()
		t.last = s
	}
	t.line.WriteString(text)
	t.lastAppend = time.Now()
	if t.OnLine != nil {
		t.OnLine(s, t.line.String())
	}
}

func (t *Transcript) Flush() {
	t.mu.Lock()
	t.flushLocked()
	t.mu.Unlock()
}

func (t *Transcript) flushLocked() {
	text := strings.TrimSpace(t.line.String())
	t.line.Reset()
	if text == "" {
		return
	}
	name := prompts.Name()
	if t.last == User {
		name = t.userName
	}
	log.Printf("[%s] %s", name, text)
	if t.all.Len() < t.limit {
		entry := name + ": " + text + "\n"
		if t.all.Len()+len(entry) > t.limit {
			entry = prefix(entry, t.limit-t.all.Len())
		}
		t.all.WriteString(entry)
	}
}

// Update ends a line after a pause (call regularly).
func (t *Transcript) Update() {
	t.mu.Lock()
	if t.line.Len() > 0 && time.Since(t.lastAppend) > linePause {
		t.flushLocked()
	}
	t.mu.Unlock()
}

func (t *Transcript) LastSpeaker() Speaker {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

// Full: everything said so far (flushed lines).
func (t *Transcript) Full() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.all.String()
}

// prefix cuts s to at most n bytes without splitting a UTF-8 character.
func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// Tail: the last n bytes of s (whole characters).
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && s[i]&0xC0 == 0x80 {
		i++
	}
	return s[i:]
}
