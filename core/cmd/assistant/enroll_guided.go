package main

import (
	"context"
	"crypto/sha1"
	"echo-assistant/internal/prompts"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/audio"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

// The guided wake word training (#61): `assistant ctl enroll <person> guided`. The assistant announces each
// part (where to stand, what to say), waits until the person is there, records the part as
// `enroll <person> <session>` does, and goes on to the next one by himself. The action button or
// `enroll stop` ends it. About 15 minutes; the wake word stays off throughout.
//
// The announcements are made once with OpenAI's TTS and kept in <state_dir>/voice/enroll-*.pcm.

type guideStep struct {
	session string
	say     string        // announced before the part
	wait    time.Duration // to get there after the announcement
	takes   int           // beeps; 0 = free talk for seconds
	seconds int
	music   bool   // music plays on this Echo during the part (The assistant starts and stops it)
	after   string // announced after the part
}

var guideSteps = []guideStep{
	{session: "near", takes: 20, wait: 4 * time.Second,
		say: "Wir trainieren jetzt das Aktivierungswort. Das dauert etwa fünfzehn Minuten, und ich sage vor jedem " +
			"Abschnitt, wo du stehen sollst. Nach jedem Piepton sagst du einmal " + prompts.Name() + ", ganz normal. Mit der Aktionstaste " +
			"kannst du jederzeit abbrechen. Abschnitt eins von acht: Stell dich etwa einen Meter vor mich."},
	{session: "3m", takes: 20, wait: 8 * time.Second,
		say: "Abschnitt zwei: Geh bitte etwa drei Meter weg."},
	{session: "far", takes: 20, wait: 10 * time.Second,
		say: "Abschnitt drei: Geh so weit weg, wie es im Raum geht, gern fünf bis sechs Meter."},
	{session: "quiet", takes: 20, wait: 8 * time.Second,
		say: "Abschnitt vier: Komm auf etwa drei Meter zurück und sag " + prompts.Name() + " jetzt leise, wie abends, wenn jemand schläft."},
	{session: "music", takes: 20, wait: 5 * time.Second, music: true,
		say:   "Abschnitt fünf: Gleich spiele ich Musik. Bleib etwa drei Meter weg und sag " + prompts.Name() + " ganz normal, nicht lauter als sonst.",
		after: "Die Musik ist aus."},
	{session: "tv", takes: 20, wait: 20 * time.Second,
		say: "Abschnitt sechs: Schalte bitte den Fernseher ein, in normaler Lautstärke, am besten mit Sprache, zum Beispiel " +
			"Nachrichten. Bleib etwa drei Meter weg. Ich beginne in zwanzig Sekunden."},
	{session: "negative-tv", seconds: 90, wait: 3 * time.Second,
		say: "Abschnitt sieben: Lass den Fernseher laufen und rede jetzt neunzig Sekunden lang ganz normal. Lies etwas vor " +
			"oder erzähl von deinem Tag, aber sag nicht " + prompts.Name() + ". Es geht los nach dem Ton.",
		after: "Danke. Du kannst den Fernseher wieder ausschalten."},
	{session: "negative", seconds: 90, wait: 15 * time.Second,
		say: "Abschnitt acht, der letzte: Noch einmal neunzig Sekunden normal reden, ohne " + prompts.Name() + ", diesmal ohne Fernseher. " +
			"Ich beginne in fünfzehn Sekunden."},
}

const (
	guideDone = "Fertig, vielen Dank. Die Aufnahmen sind gespeichert."
	// before the first part when the training goes on where it stopped
	guideResumeText = "Wir machen mit dem Training des Aktivierungsworts weiter. Nach jedem Piepton sagst du einmal " +
		"{{ASSISTANT_NAME}}. Mit der Aktionstaste kannst du jederzeit abbrechen. "
)

// guide: the running guided training.
type guide struct {
	person string
	from   int // the first part (1-based)
	stop   chan struct{}
	once   sync.Once
	step   atomic.Int32 // 1-based, 0 = starting
}

type guideRef = atomic.Pointer[guide]

func (g *guide) cancel() { g.once.Do(func() { close(g.stop) }) }

func (g *guide) stopped() bool {
	select {
	case <-g.stop:
		return true
	default:
		return false
	}
}

func (d *device) startGuide(person string, from int) string {
	if d.session.Open() {
		return "error: a conversation is open"
	}
	g := &guide{person: person, from: from, stop: make(chan struct{})}
	if d.enroll.Load() != nil || !d.guide.CompareAndSwap(nil, g) {
		return "error: an enrollment is running"
	}
	go func() { // the later announcements ready before they are needed (the first one makes runGuide)
		for i, s := range guideSteps[from-1:] {
			for _, text := range []string{s.say, s.after} {
				if text != "" && (i > 0 || text != s.say) {
					enrollSpeech(text)
				}
			}
		}
		enrollSpeech(guideDone)
	}()
	go d.runGuide(g)
	return fmt.Sprintf("guided training of %s started at part %d of %d", person, from, len(guideSteps))
}

func (d *device) runGuide(g *guide) {
	defer d.guide.CompareAndSwap(g, nil)
	log.Printf("[Enroll] guided training of %s from part %d", g.person, g.from)
	for i, step := range guideSteps {
		if i+1 < g.from {
			continue
		}
		if g.stopped() {
			break
		}
		g.step.Store(int32(i + 1))
		say := step.say
		if i+1 == g.from && i > 0 {
			say = guideResume() + say
		}
		d.announceEnroll(g, say)
		if step.music && !d.startEnrollMusic(g) {
			d.announceEnroll(g, "Die Musik startet nicht, ich überspringe diesen Abschnitt.")
			continue
		}
		select {
		case <-g.stop:
		case <-time.After(step.wait):
		}
		if g.stopped() {
			break
		}
		d.runGuideStep(g, step)
		if step.music {
			d.registry.Call("music_control", fmt.Sprintf(`{"action": "pause", "speaker": %q}`, d.cfg.Settings.SpotifyName),
				tools.Context{Channel: tools.Console, Origin: "Konsole"})
		}
		if step.after != "" && !g.stopped() {
			d.announceEnroll(g, step.after)
		}
	}
	if g.stopped() {
		log.Printf("[Enroll] guided training of %s stopped at part %d", g.person, g.step.Load())
		d.speaker.Tone(330, 300, 0.15)
		return
	}
	d.announceEnroll(g, guideDone)
	log.Printf("[Enroll] guided training of %s done", g.person)
}

// runGuideStep records one part and waits for it; the guide's stop ends it too.
func (d *device) runGuideStep(g *guide, step guideStep) {
	count := step.takes
	if step.takes == 0 {
		count = step.seconds
	}
	e := d.newEnrollment(g.person, step.session, count)
	if !d.enroll.CompareAndSwap(nil, e) {
		log.Printf("[Enroll] part %s: another enrollment runs", step.session)
		return
	}
	go func() {
		select {
		case <-g.stop:
			e.finish()
		case <-e.done:
		}
	}()
	d.runEnrollment(e)
}

// startEnrollMusic: the user's most played songs on this Echo, until they play (30 s).
func (d *device) startEnrollMusic(g *guide) bool {
	if d.music == nil {
		return false
	}
	if !d.music.Playing() {
		out := d.registry.Call("play_music", fmt.Sprintf(`{"what": "", "type": "top", "speaker": %q}`, d.cfg.Settings.SpotifyName),
			tools.Context{Channel: tools.Console, Origin: "Konsole"})
		log.Printf("[Enroll] music: %s", out)
	}
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !g.stopped(); time.Sleep(500 * time.Millisecond) {
		if d.music.Playing() {
			return true
		}
	}
	return false
}

// announceEnroll speaks text (made once, then from the state directory) and waits until it has
// played. Without TTS it beeps twice, so the training still runs.
func (d *device) announceEnroll(g *guide, text string) {
	pcm, err := enrollSpeech(text)
	if err != nil {
		log.Printf("[Enroll] announcement: %v", err)
		d.speaker.Tone(660, 150, 0.15)
		d.speaker.Tone(660, 150, 0.15)
		return
	}
	if d.music != nil {
		d.music.Duck(true)
		defer d.music.Duck(false)
	}
	const slice = audio.SpeakerRate / 5
	for i := 0; i < len(pcm) && !g.stopped(); i += slice {
		d.speaker.Queue(pcm[i:min(i+slice, len(pcm))])
	}
	if g.stopped() {
		d.speaker.Flush()
		return
	}
	for d.speaker.Speaking(300*time.Millisecond) && !g.stopped() {
		time.Sleep(50 * time.Millisecond)
	}
}

func enrollSpeech(text string) ([]int16, error) {
	sum := sha1.Sum([]byte(text))
	path := filepath.Join(store.Dir, "voice", "enroll-"+hex.EncodeToString(sum[:6])+".pcm")
	data, err := os.ReadFile(path)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if data, err = openai.Speech(ctx, text, "pcm"); err != nil {
			return nil, err
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			log.Printf("[Enroll] keeping the announcement: %v", err)
		}
	}
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return pcm, nil
}

// guideResume: guideResumeText with the name to say.
func guideResume() string {
	return strings.ReplaceAll(guideResumeText, "{{ASSISTANT_NAME}}", prompts.Name())
}
