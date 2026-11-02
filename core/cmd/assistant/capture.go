package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/live"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

// The first words after the wake word or the button (rebuilt 28.10.2026, #61): the device listens
// itself, sends the first utterance to the transcription once the user pauses, and decides:
//   - no words (noise, the TV's murmur, nobody): a wake word ends unnoticed; the button opens GPT
//     Live, which greets;
//   - a quick command (the rules in quick.go, home.go, timers.go; a short sentence with a music word
//     goes to the Decisions API, intent.go): the device runs it, GPT Live is never connected;
//   - anything else: only now GPT Live connects and gets everything said since the start as its
//     first audio, then the microphone live.
//
// Until then GPT Live connected in parallel and its voice and calls were held until the device
// decided (#59, 18.10.2026). That raced: GPT Live missed a quiet question said while it
// connected, its held answer was dropped for the device's transcript, the rest of the dropped
// answer played from the middle of a sentence and the real answer came twice (28.10.2026). One
// path at a time costs a question ~2 s (the connection) and saves every false start and quick
// command its GPT Live session.

const (
	captureMinPeak  = 120                    // voice threshold at least (as voice_peak; 300 missed commands from afar); else 4x the room's level
	captureSilence  = 600 * time.Millisecond // a pause that ends the command
	captureLong     = 4 * time.Second        // longer than any command: GPT Live at once (the button)
	captureCut      = 10 * time.Second       // an utterance without a pause goes to the transcription now
	captureQuickMax = 30 * time.Second       // a quick command still running: listening ends anyway
	captureQuiet    = 4 * time.Second        // nothing (more) said: decided without words
	captureMax      = 30 * audio.MicRate     // what is kept for GPT Live at most (samples)
	// voice above the threshold an utterance has at least: only blocks over it count, so a spoken
	// "spiel Musik" can reach just ~200 ms; less than this is a click
	captureMinVoiceMs = 80
	// voice without words under music: "Wie bitte, Sir?" (the words drowned in it); 400 ms, not
	// 100: the music's fading tail after the wake word counted (20.10.2026)
	captureRepeatVoiceMs = 400
)

type capture struct {
	mu         sync.Mutex
	active     bool
	gen        int          // each start; a late transcription of an earlier one is ignored
	req        live.Request // what it opens: the wake word or the button
	all        []int16      // everything since the start, for GPT Live
	firstVoice int          // where in all the first voice began, -1 none yet
	voiceEnd   int          // where in all the last voice ended
	pcm        []int16      // the current utterance with ~300 ms before it
	started    time.Time
	floor      int // quietest 20 ms block so far: the room (music ducked, fan)
	voiceStart time.Time
	lastVoice  time.Time
	voicedMs   int  // voice in the current utterance (a scrap is no command)
	totalMs    int  // voice since the start
	deciding   int  // transcriptions and intent questions under way
	used       bool // a quick command ran (once)
	acked      bool // the button's "Ja, Sir?" played
	again      bool // "Wie bitte, Sir?" asked once
}

// listening: the device takes the first words itself (the ring shows it).
func (d *device) listening() bool {
	c := &d.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

// listenedFor: how long the device has been taking the first words.
func (d *device) listenedFor() time.Duration {
	c := &d.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.started)
}

// listen starts taking the first words; false when a conversation or another start runs.
func (d *device) listen(req live.Request) bool {
	if d.session.Open() {
		return false
	}
	c := &d.capture
	c.mu.Lock()
	if c.active {
		c.mu.Unlock()
		return false
	}
	c.active, c.req, c.gen = true, req, c.gen+1
	c.restartLocked()
	c.used, c.acked, c.again = false, false, false
	c.mu.Unlock()
	log.Printf("[Listen] %s", map[bool]string{true: "after the wake word", false: "after the button"}[req.Wake])
	if d.music != nil && d.music.Playing() {
		d.music.HushFor(12 * time.Second) // until decided
	}
	return true
}

// toggle: the action button. It ends a conversation or the first words; else the device listens.
func (d *device) toggle() {
	switch {
	case d.listening(): // first: the session may be open, connected ahead
		d.stopListening("the button", true)
	case d.session.Open():
		d.session.Close()
	default:
		d.listen(live.Request{})
	}
}

// restartLocked: listening from now (the start, or again after "Wie bitte, Sir?").
func (c *capture) restartLocked() {
	c.all, c.pcm, c.started, c.floor, c.firstVoice = c.all[:0], c.pcm[:0], time.Now(), 1<<30, -1
	c.voiceStart, c.lastVoice, c.voicedMs, c.totalMs, c.deciding = time.Time{}, time.Time{}, 0, 0, 0
}

// stopListening ends the first words without GPT Live: words=false is a start nobody spoke to.
func (d *device) stopListening(why string, words bool) {
	c := &d.capture
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return
	}
	c.active = false
	c.gen++
	req, used := c.req, c.used
	c.mu.Unlock()
	log.Printf("[Listen] ended: %s", why)
	if d.session.Warm() {
		d.session.Close() // connected ahead for nothing
	}
	if !used && d.music != nil {
		d.music.HushFor(0) // the music comes back
	}
	if req.Wake {
		d.wakeClips.conversationOver(live.Ended{Request: req, Silent: !words})
		if !words {
			d.haFire("wake_word_silent", nil) // a likely false start, unnoticed
		}
	}
}

// connectLive: not a quick command; GPT Live takes over with what was said so far (heard: the
// device's transcript, "" when it has none).
func (d *device) connectLive(heard, why string) {
	c := &d.capture
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return
	}
	c.active = false
	c.gen++
	req := c.req
	req.Decided, req.Heard = true, heard
	// from 300 ms before the first voice to 1 s after the last: the silence around it would only
	// delay the words (GPT Live takes a few seconds at once, live.micBurst; 1 s is its end of turn);
	// nothing when nobody spoke
	if c.firstVoice >= 0 {
		end := min(len(c.all), c.voiceEnd+audio.MicRate)
		req.Audio = append([]int16(nil), c.all[max(0, c.firstVoice-audio.MicRate*3/10):end]...)
	}
	// under the lock: the next microphone packet goes to the session (hear feeds the capture first)
	if !d.session.Commit(req.Audio, heard) {
		d.session.Start(req) // not warm (nobody spoke, or it failed meanwhile)
	}
	c.mu.Unlock()
	log.Printf("[Listen] GPT Live takes over (%s): %.1f s of audio", why, float64(len(req.Audio))/audio.MicRate)
	d.haFire("conversation_started", map[string]any{"reason": "", "person": ""})
	if d.music != nil {
		d.music.HushFor(0) // the conversation ducks it now
	}
}

// connectWithNote: a quick command failed; GPT Live tells the user (without the audio: it would
// answer the words as well).
func (d *device) connectWithNote(text, note string) {
	c := &d.capture
	c.mu.Lock()
	req := c.req
	c.active = false
	c.gen++
	c.mu.Unlock()
	req.Decided, req.Heard = true, text
	if !d.session.Commit(nil, text) && !d.session.Start(req) {
		return
	}
	go func() {
		if d.session.WaitLive(6 * time.Second) {
			d.session.Commentary(note)
		}
	}()
}

// endQuick: a quick command ran; nothing more to say.
func (d *device) endQuick(text string) {
	log.Printf("[Quick] %q done on the device, no GPT Live", text)
	d.logBehaviour("", "Befehl", text, d.cfg.Settings.Room) // behaviour.go
	d.wakeClips.note(func(r *wakeRecord) { r.Done = text })
	if d.music != nil {
		d.music.Duck(false)
	}
	d.stopListening("quick command", true)
}

// claimQuick: true once per start, for the command that runs.
func (d *device) claimQuick() bool {
	c := &d.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used {
		return false
	}
	c.used = true
	return true
}

// feedCapture takes microphone audio (16 kHz) while listening.
func (d *device) feedCapture(pcm []int16) {
	c := &d.capture
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	if d.speaker.Speaking(150 * time.Millisecond) {
		// The assistant's own "Ja, Sir?" is not the user's words (16.10.2026)
		c.mu.Unlock()
		return
	}
	peak := 0
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	c.floor = min(c.floor, peak)
	if len(c.all) < captureMax {
		c.all = append(c.all, pcm...)
	}
	// capped: talking on right after "Hey Jarvis" leaves no quiet block to measure the room
	voice := peak >= max(captureMinPeak, min(4*c.floor, 1500))
	if voice {
		if c.firstVoice < 0 {
			c.firstVoice = max(0, len(c.all)-len(pcm))
			// Someone speaks: GPT Live connects ahead, without audio, while the device decides
			// (~3 s it would otherwise add to every question; a start nobody speaks to costs
			// nothing, one with noise or a quick command a few seconds of GPT Live).
			d.session.Start(live.Request{Wake: c.req.Wake, Decided: true, Warm: true})
		}
		c.voiceEnd = len(c.all)
		if c.voiceStart.IsZero() {
			c.voiceStart = now
			c.pcm = c.pcm[:0] // keep ~300 ms before the voice only
		}
		c.lastVoice = now
		ms := len(pcm) * 1000 / audio.MicRate
		c.voicedMs += ms
		c.totalMs += ms
	}
	c.pcm = append(c.pcm, pcm...)
	if c.voiceStart.IsZero() {
		if keep := audio.MicRate * 3 / 10; len(c.pcm) > keep {
			c.pcm = append(c.pcm[:0], c.pcm[len(c.pcm)-keep:]...)
		}
		d.quietLocked(now) // unlocks
		return
	}
	// The button: a long utterance is a real question, GPT Live at once. After the wake word it
	// is decided as any other (most likely the TV or talk in the room, 28.10.2026), at the
	// latest after captureCut without a pause.
	if !c.req.Wake && now.Sub(c.voiceStart) > captureLong {
		c.mu.Unlock()
		d.connectLive("", "the user talks long")
		return
	}
	if now.Sub(c.lastVoice) < captureSilence && now.Sub(c.voiceStart) < captureCut {
		c.mu.Unlock()
		return
	}
	if c.voicedMs < captureMinVoiceMs { // a click or a knock
		c.voiceStart, c.voicedMs = time.Time{}, 0
		c.mu.Unlock()
		return
	}
	// The utterance goes to the transcription and the device listens on: an utterance may be a
	// scrap (the rest of "Jarvis", the music's tail after it was silenced) that comes back empty,
	// and the words right after it must not go unheard (20.10.2026).
	c.deciding++
	utterance, gen := append([]int16(nil), c.pcm...), c.gen
	c.voiceStart, c.voicedMs = time.Time{}, 0
	c.pcm = c.pcm[:0]
	c.mu.Unlock()
	go d.transcribeCommand(utterance, gen)
}

// quietLocked: nothing is being said. Called with the capture locked; unlocks.
func (d *device) quietLocked(now time.Time) {
	c := &d.capture
	// the button: "Ja, Sir?" when the user does not go on talking at once
	ack := !c.req.Wake && !c.acked && c.totalMs == 0 && now.Sub(c.started) > time.Duration(prompts.Int("ACK_WAIT_MS", 500))*time.Millisecond
	if ack {
		c.acked = true
	}
	since := c.started
	if !c.lastVoice.IsZero() {
		since = c.lastVoice
	}
	// not while a quick command runs (c.used): "Spiel …" took 5.9 s through Spotify, and the quiet
	// ended it as a start without words (28.10.2026); endQuick ends it
	quiet := c.deciding == 0 && !c.used && now.Sub(since) > captureQuiet
	stuck := c.used && now.Sub(c.started) > captureQuickMax
	spoke, wake, again := c.totalMs >= captureRepeatVoiceMs, c.req.Wake, c.again
	if quiet && spoke && !again && d.music != nil && d.music.Playing() {
		// voice but no words under music: they drowned in it; asked once
		c.again = true
		c.restartLocked()
		c.mu.Unlock()
		log.Print("[Quick] nothing understood under music: asking again")
		d.wakeClips.note(func(r *wakeRecord) { r.AskedAgain = true })
		if clip := d.acks.Repeat(); clip != nil {
			d.speaker.Queue(clip)
		} else {
			d.speaker.Tone(660, 120, 0.15)
		}
		return
	}
	c.mu.Unlock()
	if ack {
		if clip := d.acks.Pick(); clip != nil {
			d.speaker.Queue(clip)
		} else {
			d.speaker.Tone(880, 120, 0.15)
		}
	}
	switch {
	case stuck:
		d.stopListening("the quick command still runs", true)
	case !quiet:
	case wake:
		d.stopListening("no words after the wake word", false)
	default:
		d.connectLive("", "the user says nothing") // GPT Live greets
	}
}

// current: the start gen is still running (no later start, not decided).
func (d *device) current(gen int) bool {
	c := &d.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active && c.gen == gen
}

// decided: one transcription or intent question is over.
func (d *device) decided() {
	c := &d.capture
	c.mu.Lock()
	c.deciding--
	c.mu.Unlock()
}

// keepQuickClip: what the quick path sent to the transcription, with what came back, for finding
// why commands are not understood (with music right after the wake word, 20.10.2026): the newest
// quickClipsKept in <state_dir>/quick/ (never mirrored, never in the repository).
func (d *device) keepQuickClip(pcm []int16, text string) {
	const quickClipsKept = 50
	music := "quiet"
	if d.music != nil && d.music.Playing() {
		music = "music"
	}
	result := "none"
	if text != "" {
		result = "ok"
	}
	dir := filepath.Join(store.Dir, "quick")
	base := filepath.Join(dir, fmt.Sprintf("%s_%s_%s", time.Now().Format("20060102-150405"), music, result))
	data := wav(pcm, audio.MicRate)
	go func() {
		os.MkdirAll(dir, 0o700)
		os.WriteFile(base+".wav", data, 0o600)
		os.WriteFile(base+".txt", []byte(text+"\n"), 0o600)
		prune(dir, 2*quickClipsKept)
	}()
}

// transcribeCommand: the device's own transcript of an utterance decides.
func (d *device) transcribeCommand(pcm []int16, gen int) {
	defer d.decided()
	if len(pcm) < audio.MicRate*3/10 {
		// a scrap (the rest of "Jarvis"): the API rejects it as corrupted
		log.Printf("[Quick] transcription: only %d ms of audio", len(pcm)*1000/audio.MicRate)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	text, err := openai.TranscribeHint(ctx, wav(pcm, audio.MicRate), "command.wav", prompts.Text("QUICK_TRANSCRIBE_HINT"))
	if err != nil {
		log.Printf("[Quick] transcription: %v", err)
		if d.current(gen) {
			d.connectLive("", "the transcription failed") // GPT Live hears it itself
		}
		return
	}
	// On silence or noise the model returns its prompt (16.10.2026): nothing was understood. The
	// wake word alone (its rest before the command) is nothing either: the device listens on.
	lower := strings.ToLower(text)
	wakeOnly := true
	for _, w := range strings.Fields(lower) {
		if w = strings.Trim(w, ".,!?"); w != "hey" && !prompts.IsName(w) {
			wakeOnly = false
		}
	}
	if strings.TrimSpace(lower) == "" || wakeOnly || strings.Contains(lower, "stopp") &&
		strings.Contains(lower, "zurück") && strings.Contains(lower, "leiser") {
		d.keepQuickClip(pcm, "")
		log.Print("[Quick] transcription: nothing understood")
		return
	}
	d.keepQuickClip(pcm, text)
	log.Printf("[Quick] heard %q (%d ms, %.1f s audio)", text, time.Since(started).Milliseconds(),
		float64(len(pcm))/audio.MicRate)
	if !d.current(gen) {
		return
	}
	d.wakeClips.note(func(r *wakeRecord) {
		if r.Quick == "" {
			r.Quick = text
		}
	})
	if d.quickMobile(text) {
		d.endQuick(text)
		return
	}
	if d.quickMusic(text, true) { // runs it and ends the start itself
		return
	}
	// Everything else goes to the Decisions API (gpt-6-luna, ~0.3 s, a fraction of a cent): fast
	// and cheap enough for every utterance, and it knows commands the rules do not ("Kannst du die
	// Musik leiser machen?"). Until 28.10.2026 only short sentences with a music word went there.
	// After the wake word it also asks whether the words were meant for the assistant at all: the TV and
	// talk in the room reach the transcription too.
	d.capture.mu.Lock()
	wake := d.capture.req.Wake
	d.capture.mu.Unlock()
	cmd, addressed := classifyIntent(text, wake)
	if !d.current(gen) {
		return
	}
	if cmd.action == "time" || cmd.action == "date" {
		if d.claimQuick() {
			d.endQuick(text)
			go d.sayLocal(clockAnswer(cmd.action, time.Now()))
		}
		return
	}
	if cmd.action != "" {
		d.runMusic(cmd, text)
		return
	}
	if wake && addressed >= 0 && addressed < prompts.Float("FIRST_WORDS_ADDRESSED_NOT", 0.2) {
		d.stopListening(fmt.Sprintf("%q not meant for the assistant (%.2f)", text, addressed), false)
		return
	}
	d.connectLive(text, "not a command")
}

func wav(pcm []int16, rate int) []byte {
	var b bytes.Buffer
	size := uint32(len(pcm) * 2)
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, 36+size)
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v) // fmt: PCM, mono, 16 bit
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, size)
	binary.Write(&b, binary.LittleEndian, pcm)
	return b.Bytes()
}
