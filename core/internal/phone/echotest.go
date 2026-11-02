package phone

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/textutil"
)

// The echo test (ctl echotest <number> [sentences]): what a caller hears of themselves on a call
// on the speaker at home, measured without a second person. The assistant dials the user's mobile phone
// from the landline - no GPT Live, no bridge of its own to the room, no ringback at home - and
// the Echo answers the phone's call itself, which goes onto the speaker and the microphone as any
// call on the phone does. On the landline leg the assistant then speaks a test sentence and listens for
// it to come back: landline -> phone -> the Echo's speaker -> its microphone -> phone -> landline,
// the way the caller's voice took when they heard themselves (17.10.2026).
//
// Speech, not beeps: the first version sent 1 kHz beeps, and the mobile network and the phone
// take steady tones for noise - the result scattered while a real caller heard herself clearly.
// The sentence and the line are compared by their loudness envelope (10 ms steps): the
// cross-correlation over 0..4 s gives the echo's delay and level, and a second peak means a
// second path (the user heard a voice "first quietly, then again loudly").

const (
	echoText   = "Hallo, hier spricht der Echotest. Eins, zwei, drei, vier, fünf, sechs."
	echoPause  = 4 * time.Second // after each sentence: the echo comes back meanwhile
	echoStep   = apiRate / 100   // 10 ms at 24 kHz, one envelope value
	echoMaxLag = 400             // steps: 4 s
	echoGap    = 15              // steps: two peaks this far apart are two paths
	echoLevel  = 0.3             // the sentence's peak, of full scale
	echoMinCor = 0.35            // below this the envelope did not come back
)

// echoProbe: the landline leg's received audio as an envelope, with when each sentence went out.
type echoProbe struct {
	mu    sync.Mutex
	start time.Time
	env   []float64 // RMS per 10 ms since start, full scale 1
	rest  []int16
	sent  []time.Time
}

func newEchoProbe() *echoProbe { return &echoProbe{} }

// record: the far end's audio (24 kHz, the line's receiving goroutine).
func (e *echoProbe) record(pcm []int16) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.start.IsZero() {
		e.start = time.Now().Add(-time.Duration(len(pcm)) * time.Second / apiRate)
	}
	e.rest = append(e.rest, pcm...)
	for len(e.rest) >= echoStep {
		e.env = append(e.env, envRMS(e.rest[:echoStep]))
		e.rest = e.rest[echoStep:]
	}
}

func envRMS(pcm []int16) float64 {
	var s float64
	for _, v := range pcm {
		f := float64(v) / 32768
		s += f * f
	}
	return math.Sqrt(s / float64(len(pcm)))
}

// envelope: the sentence's RMS per 10 ms.
func envelope(pcm []int16) []float64 {
	env := make([]float64, 0, len(pcm)/echoStep)
	for i := 0; i+echoStep <= len(pcm); i += echoStep {
		env = append(env, envRMS(pcm[i:i+echoStep]))
	}
	return env
}

// echoPath: one way the sentence came back.
type echoPath struct {
	lag   int     // steps
	cor   float64 // normalized cross-correlation of the envelopes
	level float64 // dB of the echo against the sentence
}

// paths: the strongest echo of a sentence sent at step at, and a second one if there is one.
// The second is searched after the first is taken out (its gain fitted, its copy subtracted):
// speech repeats itself in syllables, so the correlation has side peaks next to every echo.
func paths(sent, got []float64, at int) []echoPath {
	end := min(len(got), at+echoMaxLag+len(sent))
	if at < 0 || at >= end {
		return nil
	}
	work := append([]float64(nil), got[at:end]...)
	first, ok := strongest(sent, work, -1)
	if !ok {
		return nil
	}
	// Take the first path out: least squares gain of the sentence at its lag.
	var dot, ss float64
	for i, v := range sent {
		if first.lag+i < len(work) {
			dot += v * work[first.lag+i]
			ss += v * v
		}
	}
	if ss > 0 {
		g := dot / ss
		for i, v := range sent {
			if first.lag+i < len(work) {
				work[first.lag+i] = math.Max(0, work[first.lag+i]-g*v)
			}
		}
	}
	out := []echoPath{first}
	// The correlation does not care about the level: what is left of the line's noise after the
	// first path correlates too. A second path is one within 20 dB of the first, above -60 dB.
	if second, ok := strongest(sent, work, first.lag); ok && second.level > first.level-20 && second.level > -60 {
		out = append(out, second)
	}
	return out
}

// strongest: the lag where got looks most like sent (normalized cross-correlation of the
// envelopes), not within echoGap of skip.
func strongest(sent, got []float64, skip int) (echoPath, bool) {
	n := len(sent)
	var sentEnergy, meanSent float64
	for _, v := range sent {
		sentEnergy += v * v
		meanSent += v
	}
	meanSent /= float64(n)
	var best echoPath
	found := false
	for lag := 0; lag < echoMaxLag && lag+n <= len(got); lag++ {
		if skip >= 0 && abs(lag-skip) < echoGap {
			continue
		}
		window := got[lag : lag+n]
		var mean float64
		for _, v := range window {
			mean += v
		}
		mean /= float64(n)
		var dot, energy, varSent, got2 float64
		for i, v := range window {
			a, b := sent[i]-meanSent, v-mean
			dot += a * b
			energy += b * b
			varSent += a * a
			got2 += v * v
		}
		if energy == 0 || varSent == 0 {
			continue
		}
		if c := dot / math.Sqrt(energy*varSent); c > echoMinCor && c > best.cor {
			best, found = echoPath{lag: lag, cor: c, level: db(got2 / sentEnergy)}, true
		}
	}
	return best, found
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// result: per sentence the echoes found.
func (e *echoProbe) result(sentence []float64) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.env) < 100 || len(e.sent) == 0 {
		return fmt.Sprintf("Error: too little audio came back to measure (%d steps).", len(e.env))
	}
	var lines []string
	echoes, doubled := 0, 0
	for i, t := range e.sent {
		at := int(t.Sub(e.start) / (10 * time.Millisecond))
		found := paths(sentence, e.env, at)
		if len(found) == 0 {
			lines = append(lines, fmt.Sprintf("sentence %d: no echo", i+1))
			continue
		}
		echoes++
		var parts []string
		for _, p := range found {
			parts = append(parts, fmt.Sprintf("after %s at %.0f dB (r %.2f)", time.Duration(p.lag)*10*time.Millisecond, p.level, p.cor))
		}
		if len(found) > 1 {
			doubled++
		}
		lines = append(lines, fmt.Sprintf("sentence %d: echo %s", i+1, strings.Join(parts, ", second ")))
	}
	summary := fmt.Sprintf("%d of %d sentences came back", echoes, len(e.sent))
	if doubled > 0 {
		summary += fmt.Sprintf(", %d on two paths", doubled)
	}
	return summary + ". " + strings.Join(lines, "; ")
}

func db(ratio float64) float64 {
	if ratio <= 0 {
		return -120
	}
	return 10 * math.Log10(ratio)
}

// echoSentence: the test sentence, 24 kHz, scaled to echoLevel; from the clip store (made once).
func (p *Phone) echoSentence() ([]int16, error) {
	pcm, ok := p.clips.get(echoText)
	if !ok {
		if p.deps.Speech == nil {
			return nil, fmt.Errorf("no text to speech on this device")
		}
		p.clips.make(echoText, p.deps.Speech)
		if pcm, ok = p.clips.get(echoText); !ok {
			return nil, fmt.Errorf("the test sentence could not be made")
		}
	}
	peak := 1
	for _, v := range pcm {
		peak = max(peak, int(v), -int(v))
	}
	scaled := make([]int16, len(pcm))
	for i, v := range pcm {
		scaled[i] = int16(float64(v) * echoLevel * 32767 / float64(peak))
	}
	return scaled, nil
}

// EchoTest runs the echo test with n sentences and returns the measurement; it blocks for the
// call (n x ~8 s plus the dialling). The mobile phone must be this speaker's hands-free unit,
// the number its own.
func (p *Phone) EchoTest(number string, n int, nogate bool) string {
	// The cross-check: without the gate the sentence must come back, or the test is blind.
	if nogate {
		saved := micGatePct
		micGatePct = 100
		defer func() { micGatePct = saved }()
	}
	switch {
	case !p.Configured() || !p.Registered():
		return "error: the landline is not registered"
	case !p.mobileOnline():
		return "error: no mobile phone is connected as hands-free unit (its calls switch, or mobile_handsfree on)"
	case p.InCall() || p.MobileBusy():
		return "error: a call is running"
	}
	number = textutil.PhoneNormalized(number)
	if number == "" {
		return "error: echotest <number of the paired mobile phone> [sentences]"
	}
	sentence, err := p.echoSentence()
	if err != nil {
		return "error: " + err.Error()
	}
	n = max(1, min(n, 10))
	probe := newEchoProbe()
	p.mu.Lock()
	p.mob.autoAnswer = true
	p.wanted = &callRequest{number: number, name: "Echotest", direct: true, probe: probe, at: time.Now()}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.mob.autoAnswer = false
		p.mu.Unlock()
	}()
	log.Printf("[Echotest] calling %s, %d sentences", number, n)
	// The landline picks up once the Echo has answered the phone's call.
	deadline := time.Now().Add(ringTimeout + 10*time.Second)
	var line *Line
	dialled := false
	for line == nil && time.Now().Before(deadline) {
		p.mu.Lock()
		ours := p.call.probe == probe
		if ours && p.call.answered && p.call.line != nil && p.call.line.Active() {
			line = p.call.line
		}
		gone := dialled && !ours // it was dialled and is over (busy, rejected, no answer)
		dialled = dialled || ours
		p.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if line == nil {
		p.mu.Lock()
		if p.call.probe == probe {
			p.hangUp("echo test: no answer")
		}
		p.wanted = nil
		p.mu.Unlock()
		return "error: the call was not answered"
	}
	// The phone's call needs a moment at the speaker (SCO, the announcement tones).
	time.Sleep(3 * time.Second)
	length := time.Duration(len(sentence)) * time.Second / apiRate
	for i := 0; i < n; i++ {
		// Queued now, it goes out after the line's jitter buffer (PHONE_JITTER_MS).
		probe.mu.Lock()
		probe.sent = append(probe.sent, time.Now().Add(time.Duration(line.jitter)*time.Second/apiRate))
		probe.mu.Unlock()
		line.Queue(sentence)
		time.Sleep(length + echoPause)
	}
	p.mu.Lock()
	if p.call.probe == probe {
		p.hangUp("echo test over")
	}
	p.mu.Unlock()
	out := probe.result(envelope(sentence))
	log.Printf("[Echotest] %s", out)
	return out
}
