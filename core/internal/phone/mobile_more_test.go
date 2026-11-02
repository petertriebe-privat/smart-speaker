package phone

import (
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/tools"
)

func TestMobileQuietHours(t *testing.T) {
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local)
	for hour, want := range map[int]bool{21: false, 22: true, 23: true, 0: true, 6: true, 7: false, 12: false} {
		if got := mobileQuiet(day.Add(time.Duration(hour) * time.Hour)); got != want {
			t.Errorf("%d o'clock quiet %v, want %v", hour, got, want)
		}
	}
}

// The announcement is made once by text to speech and kept: the first cycle has only the tones,
// the next ones the name - at night without tones and at half the level.
func TestMobileAnnouncementIsKeptAndQuietAtNight(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	var mu sync.Mutex
	made := 0
	h.p.deps.Speech = func(text string) ([]int16, error) {
		mu.Lock()
		made++
		mu.Unlock()
		return constant(10000, 2400), nil
	}
	h.p.mob.mode = "on"
	day := time.Date(2026, 10, 17, 15, 0, 0, 0, time.Local)
	h.p.pollMobile(day)
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "incoming", Number: "07161 123456"})
	h.p.pollMobile(day)
	waitFor(t, "the clip made", func() bool { mu.Lock(); defer mu.Unlock(); return made == 1 })
	time.Sleep(mobileNameIn + 100*time.Millisecond)
	if peak := h.speaker.peak(); peak >= 10000 {
		t.Fatalf("the first cycle played the name before it was made (peak %d)", peak)
	}
	h.p.pollMobile(day.Add(mobileRing))
	waitFor(t, "the name in the second cycle", func() bool { return h.speaker.peak() == 10000 })
	if made != 1 {
		t.Errorf("made the clip %d times", made)
	}

	// A new call at night: no tones, the kept clip at half the level.
	hf.set()
	h.p.pollMobile(day.Add(2 * mobileRing))
	speaker := &fakeSpeaker{}
	h.p.deps.Speaker = speaker
	night := time.Date(2026, 10, 17, 23, 30, 0, 0, time.Local)
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall02", State: "incoming", Number: "07161 123456"})
	h.p.pollMobile(night)
	waitFor(t, "the name at night", func() bool { return speaker.peak() > 0 })
	if speaker.toneCount() != 0 {
		t.Errorf("%d tones at night", speaker.toneCount())
	}
	if peak := speaker.peak(); peak != 5000 {
		t.Errorf("night level %d, want 5000", peak)
	}
}

// During a conversation at the speaker it does not ring: the conversation is told once, and the
// music goes down while it rings and comes back after.
func TestMobileTellsTheConversationAndDucksTheMusic(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	var mu sync.Mutex
	var notes []string
	var ducks []bool
	h.p.deps.HomeNote = func(text string) { mu.Lock(); notes = append(notes, text); mu.Unlock() }
	h.p.deps.Duck = func(on bool) { mu.Lock(); ducks = append(ducks, on); mu.Unlock() }
	h.p.mob.mode = "on"
	now := time.Now()
	h.p.pollMobile(now)
	h.busy = true
	hf.set(hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "incoming", Number: "07161 123456"})
	h.p.pollMobile(now)
	h.p.pollMobile(now.Add(mobileRing))
	waitFor(t, "the note", func() bool { mu.Lock(); defer mu.Unlock(); return len(notes) == 1 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(notes) != 1 || !strings.Contains(notes[0], "mobile_call") {
		t.Errorf("notes %q", notes)
	}
	mu.Unlock()
	if h.speaker.toneCount() != 0 {
		t.Error("rang into the conversation")
	}
	hf.set()
	h.p.pollMobile(now.Add(2 * mobileRing))
	waitFor(t, "the music back up", func() bool { mu.Lock(); defer mu.Unlock(); return len(ducks) == 2 })
	if !ducks[0] || ducks[1] {
		t.Errorf("ducks %v, want [true false]", ducks)
	}
}

func TestDialRoute(t *testing.T) {
	h, hf, _, _ := mobileHarness(t)
	owner := tools.Context{Channel: tools.Voice}
	other := tools.Context{Channel: tools.Voice, Person: "someone-else"}
	route := func(via string, ctx tools.Context) string {
		r, err := h.p.dialRoute(via, ctx)
		if err != "" {
			return err
		}
		return r
	}
	// No phone known to oFono yet, the landline down: nothing works.
	if got := route("", owner); !strings.HasPrefix(got, "Error") {
		t.Errorf("without any line: %q", got)
	}
	h.p.pollMobile(time.Now()) // the phone is paired, hands-free off
	if got := route("", owner); got != "phone" {
		t.Errorf("landline down, phone paired: %q, want phone for this call", got)
	}
	h.p.reg.registered = true
	if got := route("", owner); got != "landline" {
		t.Errorf("hands-free off: %q, want landline", got)
	}
	if got := route("phone", owner); got != "phone" {
		t.Errorf("'über mein Handy': %q", got)
	}
	h.p.mob.mode = "on"
	h.p.pollMobile(time.Now()) // powers the modem
	h.p.pollMobile(time.Now()) // the connection stands
	if got := route("", owner); got != "phone" {
		t.Errorf("hands-free on: %q, want phone", got)
	}
	if got := route("landline", owner); got != "landline" {
		t.Errorf("'über das Festnetz': %q", got)
	}
	// Somebody else in the household never dials over the owner's phone.
	if got := route("", other); got != "landline" {
		t.Errorf("another person, auto: %q, want landline", got)
	}
	if got := route("phone", other); !strings.Contains(got, "owner") {
		t.Errorf("another person over the phone: %q", got)
	}
	hf.down = true
	h.p.pollMobile(time.Now())
	if got := route("phone", owner); !strings.HasPrefix(got, "Error") {
		t.Errorf("without oFono: %q", got)
	}
}

// Over the phone for one call: it is connected, dialled, put on the speaker with the ringback,
// announced at the pick-up and disconnected again after the call.
func TestDialOverThePhoneForOneCall(t *testing.T) {
	h, hf, lines, _ := mobileHarness(t)
	hf.calls = nil
	now := time.Now()
	h.p.pollMobile(now)
	answer := h.p.dialForMe("07161 123456", "phone", tools.Context{Channel: tools.Voice})
	if !strings.Contains(answer, "mobile phone") || !strings.Contains(answer, "connected for this call") {
		t.Fatalf("answer %q", answer)
	}
	if !h.p.Dialling() {
		t.Error("the ring should run round while it connects")
	}
	var dialled []string
	hf2 := &dialRecorder{fakeHF: hf, dialled: &dialled}
	h.p.hf = hf2
	h.p.pollMobile(now.Add(time.Second)) // powers the modem for this call
	h.p.pollMobile(now.Add(2 * time.Second))
	if len(dialled) != 1 || dialled[0] != "07161123456" {
		t.Fatalf("dialled %v", dialled)
	}
	call := hfCall{Path: "/hfp/adapter_phone/voicecall01", State: "dialing", Number: "07161123456"}
	hf.set(call)
	h.p.pollMobile(now.Add(3 * time.Second))
	if len(*lines) != 1 || h.p.bridge.Mode() != modeTalk {
		t.Fatal("the ringback should be on the speaker while it rings at the other end")
	}
	tones := h.speaker.toneCount()
	call.State = "active"
	hf.set(call)
	h.p.pollMobile(now.Add(4 * time.Second))
	waitFor(t, "the pick-up tones", func() bool { return h.speaker.toneCount() >= tones+2 })
	hf.set()
	h.p.pollMobile(now.Add(5 * time.Second))
	h.p.pollMobile(now.Add(6 * time.Second))
	if hf.modem.Powered {
		t.Error("hands-free stays on after a call made for itself only")
	}
}

// The phone does not connect: the landline takes the call, and the room is told.
func TestDialOverThePhoneFallsBackToTheLandline(t *testing.T) {
	h, hf, _, said := mobileHarness(t)
	h.p.reg.registered = true
	h.p.pollMobile(time.Now())
	hf.away = true
	now := time.Now()
	h.p.dialForMe("07161 123456", "phone", tools.Context{Channel: tools.Voice})
	h.p.pollMobile(now)
	h.p.pollMobile(now.Add(mobileDialWait + time.Second))
	h.p.mu.Lock()
	wanted := h.p.wanted
	h.p.mu.Unlock()
	if wanted == nil || wanted.number != "07161123456" || !wanted.direct {
		t.Fatalf("the landline did not take the call: %+v", wanted)
	}
	waitFor(t, "the notice", func() bool { return len(*said) == 1 })
	if !strings.Contains((*said)[0], "Festnetz") {
		t.Errorf("said %q", (*said)[0])
	}
}

// While the owner's phone is this speaker's hands-free unit, the assistant does not call it: they are
// at home, and the call would come back out of the speaker.
func TestNoCallToThePhoneThatIsConnectedHere(t *testing.T) {
	h, _, _, _ := mobileHarness(t)
	h.p.reg.registered = true
	h.p.mob.mode = "on"
	h.p.pollMobile(time.Now())
	h.p.pollMobile(time.Now())
	admin := h.p.people.Admin()
	got := h.p.placeCall(h.p.people.Name(admin), "Weckruf", tools.Context{Channel: tools.Agent})
	if !strings.Contains(got, "start_conversation") {
		t.Errorf("phone_call to the connected phone: %q", got)
	}
}

// dialRecorder: fakeHF that records what is dialled.
type dialRecorder struct {
	*fakeHF
	dialled *[]string
}

func (d *dialRecorder) Dial(modem, number string) error {
	*d.dialled = append(*d.dialled, number)
	return nil
}
