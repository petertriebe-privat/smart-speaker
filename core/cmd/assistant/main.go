// The assistant on the Amazon Echo (first generation): GPT Live voice assistant, started with the
// action button. Same persona, prompts and protocol as the M5 version in devices/atoms3r/.
//
//	assistant                run the assistant
//	jarvis ctl <command>   talk to the running one (start, stop, status, say, inject, led, ...)
//	jarvis rawcapture <seconds> <dir>   the DSP's raw microphones as WAV (rawcapture.go)
package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"echo-assistant/internal/acknowledge"
	"echo-assistant/internal/assistant"
	"echo-assistant/internal/audio"
	"echo-assistant/internal/board"
	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/hamqtt"
	"echo-assistant/internal/initiative"
	"echo-assistant/internal/input"
	"echo-assistant/internal/leds"
	"echo-assistant/internal/live"
	"echo-assistant/internal/memguard"
	"echo-assistant/internal/music"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/openai"
	"echo-assistant/internal/peer"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/timers"
	"echo-assistant/internal/tools"
	"echo-assistant/internal/wakeword"
)

var (
	// Defaults for the Echo Gen 1 and the Dot (hardware/dot2/tools/assistant.sh), so ctl needs no flags there.
	secretsPath  = flag.String("secrets", board.Path("/usr/local/echo-dev/private/jarvis-secrets.json", "/data/echo-dot-dev/private/jarvis-secrets.json"), "private values (from secrets.h)")
	settingsPath = flag.String("settings", board.Path("/usr/local/echo-dev/jarvis/settings.json", "/data/echo-dot-dev/jarvis/settings.json"), "device settings")
	audioScript  = flag.String("audio", "/usr/local/echo-dev/echo-audio.sh", "starts the DSP audio chain (Echo Gen 1)")
	micPath      = flag.String("mic", "/dev/shm/BMicsOut.shm", "DSP microphone buffer (Echo Gen 1)")
	ctlPath      = flag.String("ctl", board.Path("/mnt/jarvis/ctl.sock", "/run/jarvis/ctl.sock"), "control socket")
)

// version is the release the binary belongs to (-X main.version=v1.2.0, by the owner's release
// workflow), or the commit for a build from the PC (deploy.sh); commit is the commit it was built
// from (-X main.commit=..., else the build info). ctl status reports both, so a deploy can check
// that the new version is the one running.
var (
	version = "dev"
	commit  = ""
)

// buildCommit: the commit this binary was built from.
func buildCommit() string {
	return cmp.Or(commit, revision())
}

type device struct {
	cfg            *config.Config
	initiative     *initiative.Pass // the primary's initiative pass (#82); nil elsewhere
	lastRoomMu     sync.Mutex
	lastRoom       map[string]roomSeen // person -> the room of their last conversation at an Echo (initiative.go)
	session        *live.Session       // the conversation at the speaker
	phoneSession   *live.Session       // a phone call, in parallel
	speaker        *audio.Speaker
	mic            *audio.Mic
	ring           *leds.Ring
	registry       *tools.Registry
	svc            services
	muted          atomic.Bool
	injecting      atomic.Bool
	hearMu         sync.Mutex   // hear: the microphone and ctl inject one after the other
	errorTill      atomic.Int64 // unix ms: show the error pattern until then
	wake           *wakeword.Detector
	enroll         enrollRef          // a wake word recording runs (enroll.go); the wake word is off meanwhile
	guide          guideRef           // the guided training runs (enroll_guided.go)
	capture        capture            // the first words after the button / wake word, for quick commands
	music          *music.Player      // the Echo as a Spotify Connect speaker (nil = off)
	btMusic        *audio.BTMusic     // a phone's music over Bluetooth at the assistant's volume (nil = no Bluetooth)
	musicRing      atomic.Bool        // the ring shows the music (settings music_ring; ctl musicring)
	mobileDuck     atomic.Bool        // the mobile phone rings or a call on it runs: the music stays down
	callPress      bool               // the action button went down during a call: decided on release (input goroutine)
	roomPress      bool               // the same for a call at another Echo (roomlink.go)
	wakeStale      bool               // a conversation ran: the detector starts over (only the mic goroutine)
	wakeClips      wakeClips          // the audio before detections and near misses, and the second chance (#61)
	acks           *acknowledge.Clips // "Ja, Sir?" and "Wie bitte, Sir?"
	peers          *peer.Service      // the other Echos (nil = alone, peers.go)
	timers         *timers.Service    // timers and alarms on this Echo (timers.go, #25)
	ringingTimer   atomic.Bool        // the alarm loop runs
	levels         levels             // the microphone's level, for the peer link's wake claims
	ha             *hamqtt.Device     // this speaker in Home Assistant over MQTT (hadevice.go; nil = off)
	haAssistant    *hamqtt.Device     // the household's assistant in HA, on the primary (haassistant.go)
	wakeShift      atomic.Int64       // the wake cutoffs' shift in thousandths, HA's sensitivity (hacontrols.go)
	night          atomic.Bool        // HA's night mode: the night volume in force, the ring dimmed
	dayVolume      int                // night mode: the volume to go back to (under volMu)
	nightSave      *time.Timer        // the night volume is saved a few seconds after the ring stops
	nightVolume    atomic.Int32
	pttHeld        bool         // the Fire TV remote's microphone key, in hear (tvboard.go)
	pttLast        time.Time    // its last sample with sound
	pttButton      bool         // this press was the action button (no conversation was open)
	realtimeChoice atomic.Value // the Fire TV's voice model chosen in HA (tvboard.go)
	tvTurn         tvTurns      // the Fire TV's voice turns: speech to text, backend, text to speech (tvturn.go)
	screenLive     screenLive   // state and words to the TV in this room (screenlive.go)
	ringLevel      atomic.Int32 // the ring's brightness 1..255 (never off: hacontrols.go ringFromHA)
	lastPerson     atomic.Value // string: who talked here last (HA's "Zuletzt gesprochen")

	volMu       sync.Mutex
	volume      int
	volApplied  int
	volFromSpot bool // the last change came from Spotify (app slider): not sent back
	volKick     chan struct{}
}

func main() {
	flag.Parse()
	if flag.NArg() > 0 && flag.Arg(0) == "ctl" {
		os.Exit(control(strings.Join(flag.Args()[1:], " ")))
	}
	if flag.NArg() > 0 && flag.Arg(0) == "rawcapture" {
		if board.Dot2 {
			fmt.Fprintln(os.Stderr, "rawcapture reads the Echo Gen 1's DSP buffers; the Dot has none")
			os.Exit(2)
		}
		os.Exit(rawCapture(flag.Args()[1:]))
	}
	if flag.NArg() > 0 && flag.Arg(0) == "event" {
		// librespot's --onevent: the Spotify volume (app slider) is the Echo's volume.
		// Never write to stdout here: it is librespot's (see internal/music).
		if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
			os.Stdout = devnull
		}
		if os.Getenv("PLAYER_EVENT") == "volume_changed" {
			os.Exit(control("spotifyvolume " + os.Getenv("VOLUME")))
		}
		os.Exit(0)
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("[Assistant] %s starting, version %s commit %s", board.Name(), version, buildCommit())
	guardSeccomp() // tvboard.go
	cfg, err := config.Load(*secretsPath, *settingsPath)
	if err != nil {
		log.Fatal(err)
	}
	// The assistant's name and the household's own persona (#29), before any prompt is read.
	prompts.SetName(cfg.Secrets.String("ASSISTANT_NAME"))
	if err := prompts.LoadPersona(filepath.Join(cfg.Settings.StateDir, "persona.json")); err != nil {
		log.Printf("[Assistant] own persona not loaded, the built-in one is used: %v", err)
	}
	if board.Phone {
		startHomeTunnel(cfg) // phoneboard.go: the house's services over the home gateway
	}
	if loc, err := time.LoadLocation(prompts.Text("CALENDAR_TIMEZONE")); err == nil {
		time.Local = loc
	}
	d := &device{cfg: cfg, registry: &tools.Registry{}, volKick: make(chan struct{}, 1), lastRoom: map[string]roomSeen{}}
	if board.Android {
		// Android keeps the clock and forbids an app to set it (seccomp: SIGSYS). Synced it is:
		// without the flag the prompt had no date and time (29.10.2026, the weather of "morgen").
		netx.TimeSynced = true
	} else {
		go d.keepTime()
	}
	d.ring, err = leds.Open(cfg.Settings.LedOffset, cfg.Settings.LedLevel)
	if err != nil {
		log.Printf("[LED] %v", err)
	}
	d.initHASettings() // what Home Assistant set: night mode, ring, wake word (hacontrols.go)
	d.startAudio()
	d.speaker = audio.NewSpeaker(prompts.Int("SILENCE_PEAK", 300))
	openai.APIKey = cfg.Secrets.String("OPENAI_API_KEY")
	store.Dir = cfg.Settings.StateDir
	live.SetVoicePeak(cfg.Settings.VoicePeak)
	d.musicRing.Store(cfg.Settings.MusicRing)
	if err := os.MkdirAll(store.Dir, 0700); err != nil {
		log.Printf("[Store] %v: memory and settings will not persist", err)
	}
	errorlog.Open(store.Dir)
	// A working SSH connection comes first: the assistant ends itself before the Echo starves
	// (internal/memguard); the service loop starts him again.
	limit := int64(max(32, cfg.Settings.MemoryLimitMB)) << 20
	memguard.Start(memguard.Limits{Hard: limit, Soft: limit / 2, LowAvail: 24 << 20}, func(reason string) {
		log.Printf("[Memory] %s: ending, the service loop restarts the assistant", reason)
		errorlog.Add("Memory guard: %s; the assistant restarted", reason)
		d.haFire("restarting", map[string]any{"reason": "memory guard: " + reason}) // HA can tell the owner
		time.Sleep(300 * time.Millisecond)
		os.Exit(70)
	})
	errorlog.Add("The assistant started (system up %s)", systemUptime())
	go d.selfTest()
	d.setupPeers()
	d.settleRole() // who is the primary, before any poller starts (failover.go)
	d.setupServices()
	acks := acknowledge.New(store.Dir)
	d.acks = acks
	go acks.Run(context.Background())
	// The assistant does things in parallel: the conversation at the speaker and a phone call are two
	// independent sessions that can run at the same time and pass each other messages.
	hooks := func(label string) live.Hooks {
		h := live.Hooks{
			Instructions: assistant.LiveInstructions,
			Backend:      d.backendInstructions, // with the TV's rule when one is on here (screenlive.go)
			OnState:      func(st live.State) { d.onSessionState(label, st); d.screenState(label, st) },
			OnWords:      func(user bool, line string) { d.screenWords(label, user, line) }, // the TV (screenlive.go)
			Delegate:     d.delegate,                                                        // a Realtime session's backend (tvboard.go)
			OnEnd:        d.onSessionEnd,
			OnOver: func(e live.Ended) {
				if label == "speaker" && !e.Request.Phone && e.Request.Reason == "" {
					d.wakeClips.conversationOver(e)
				}
				if label == "speaker" && e.ClosedBy == "content" && e.HeardUser {
					d.resumeAfterFilter(e)
				}
			},
			Closing: func(transcript string) string {
				if d.svc.agenda == nil {
					return ""
				}
				return d.agendaClosing(label, transcript) // the primary's on a secondary (shared.go)
			},
		}
		h.OnError = func(message string) { errorlog.Add("Conversation (%s) failed: %s", label, message) }
		if label == "speaker" {
			h.OnError = func(message string) {
				errorlog.Add("Conversation (%s) failed: %s", label, message)
				d.errorTill.Store(time.Now().Add(4 * time.Second).UnixMilli())
			}
			h.Triggers = intentTriggers()
			h.Unfulfilled = d.fulfil
			h.Acknowledge = func() time.Duration {
				if board.Android {
					// push to talk: the screen shows he listens; the clip muted the start of the
					// question ("Ja?" over "Wie wird das Wetter …", 29.10.2026)
					return 0
				}
				clip := acks.Pick()
				if clip == nil {
					return 0
				}
				d.speaker.Queue(clip)
				return time.Duration(len(clip)) * time.Second / audio.SpeakerRate
			}
		}
		return h
	}
	apiKey := cfg.Secrets.String("OPENAI_API_KEY")
	d.session = live.New(live.Options{APIKey: apiKey, EchoGate: cfg.Settings.EchoGate, Label: "speaker", Room: cfg.Settings.Room,
		Realtime: liveRealtime(cfg.Settings.RealtimeModel)},
		hooks("speaker"), d.registry, d.speaker, d.mic)
	d.phoneSession = live.New(live.Options{APIKey: apiKey, Label: "phone"}, hooks("phone"), d.registry, d.speaker, nil)
	d.registry.Add(live.EndCallTool())
	d.addLiveTools()
	d.setupTimers()
	d.volume, d.dayVolume = cfg.Settings.Volume, cfg.Settings.Volume
	if d.night.Load() { // HA's night mode was on: its own volume (hacontrols.go)
		d.volume = int(d.nightVolume.Load())
	}
	d.speaker.SetVolume(d.volume)
	audio.SetDotBluetoothVolume(d.volume)
	d.volApplied = d.volume
	go d.volumeWorker()
	d.setupShared()
	d.setupRooms()
	d.setupRoomLink()
	d.setupIntercom()
	d.setupHADevice()
	go d.watchRole()
	go d.reportTakeover()
	if prompts.Bool("WAKE_WORD_ENABLED", true) && cfg.Settings.WakeWord {
		model, window := wakeword.Builtin(), prompts.Int("WAKE_SLIDING_WINDOW", 5)
		if path := cfg.Settings.WakeModel; path != "" {
			if !filepath.IsAbs(path) {
				path = filepath.Join(store.Dir, path)
			}
			if m, win, err := wakeword.LoadFile(path); err != nil {
				errorlog.Add("Wake word: model %s: %v; the built-in one listens", path, err)
			} else {
				model = m
				if win > 0 {
					window = win
				}
				log.Printf("[Wake word] model %s (window %d)", path, window)
			}
		}
		w, err := wakeword.New(model, d.wakeCutoff(cfg.Settings.WakeCutoff), window)
		if err != nil {
			errorlog.Add("Wake word: %v", err)
		} else {
			w.OnNearMiss = func(peak float64) {
				music := d.music != nil && d.music.Playing()
				log.Printf("[Wake word] near miss %.2f (cutoff %.2f, music %v, mic peak %d)", peak,
					w.Cutoff(), music, d.mic.Peak())
				if d.wakeClips.nearMiss(peak, w.Cutoff()) {
					log.Printf("[Wake word] second near miss within %v: counts as heard", wakeSecondWithin)
					d.wakeClips.save("second", peak, w.Cutoff(), music)
				} else if peak >= wakeClipMin {
					d.wakeClips.save("near", peak, w.Cutoff(), music)
				}
			}
			d.wake = w
		}
	}
	if d.mic != nil {
		go d.mic.Run(context.Background(), func(pcm []int16) {
			if !d.injecting.Load() {
				d.hear(pcm)
			}
		})
	}
	events := make(chan input.Event, 32)
	if m, err := input.MutedNow(); err == nil {
		d.setMuted(m) // the hardware latch keeps its state across restarts
		log.Printf("[Button] mute %v at start", m)
	} else {
		log.Printf("[Input] mute state: %v", err)
	}
	input.Run(events)
	if board.Phone && audio.OnTV() {
		go d.phoneKeys() // the app's assistant button (phoneboard.go): needs the session
	}
	go d.serveControl()
	go d.showState()
	d.speaker.Tone(660, 120, 0.15) // ready
	if board.Dot2 {
		go d.checkDotSpeaker()
	}
	log.Printf("[Assistant] ready: action button or wake word (%v) starts a conversation", d.wake != nil)
	for ev := range events {
		d.onInput(ev)
	}
}

// keepTime sets the clock (the Echo starts in 1970) and keeps it right.
func (d *device) keepTime() {
	servers := []string{"pool.ntp.org", "time.google.com", "time.cloudflare.com"}
	for {
		t, err := netx.SyncTime(servers...)
		if err != nil {
			log.Printf("[Time] %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("[Time] set to %s", t.Local().Format("02.01.2006 15:04:05 MST"))
		time.Sleep(6 * time.Hour)
	}
}

func (d *device) startAudio() {
	if board.Android {
		d.startTVAudio() // tvboard.go
		return
	}
	if board.Dot2 {
		// no DSP chain: the assistant opens the codecs itself (internal/audio/dot.go)
		if err := audio.StartDot(d.cfg.Settings.MicGainDB, d.cfg.Settings.SpeakerGainDB); err != nil {
			log.Printf("[Audio] %v", err)
			return
		}
		d.mic, _ = audio.DotMic()
		if addr := d.cfg.Settings.BTSpeaker; addr != "" {
			exe, _ := os.Executable()
			name := cmp.Or(d.cfg.Settings.Name, prompts.Name()) // what the speaker lists
			audio.StartDotBluetooth(filepath.Join(filepath.Dir(exe), "btspeaker"), filepath.Join(d.cfg.Settings.StateDir, "bt"), addr, name)
		}
		return
	}
	if _, err := os.Stat("/mnt/echo-audio/pulse-native"); err != nil {
		log.Print("[Audio] starting the DSP chain")
		out, err := exec.Command("/bin/sh", *audioScript, "start").CombinedOutput()
		log.Printf("[Audio] %s %v", strings.TrimSpace(string(out)), err)
	}
	for i := 0; i < 20; i++ {
		mic, err := audio.OpenMic(*micPath)
		if err == nil {
			d.mic = mic
			return
		}
		log.Printf("[Audio] mic: %v", err)
		time.Sleep(time.Second)
	}
}

// selfTest: the hardware answers (as the M5's checkMicrophone / checkAmplifier). A failure goes to
// the error log and shows yellow on the ring.
func (d *device) selfTest() {
	fail := func(format string, args ...any) {
		errorlog.Add("Self-test: "+format, args...)
		d.errorTill.Store(time.Now().Add(10 * time.Second).UnixMilli())
	}
	switch {
	case d.mic == nil:
		fail("no microphone (%s)", *micPath)
	case !d.mic.Alive(3 * time.Second):
		fail("the microphone delivers nothing (DSP chain)")
	}

	if err := d.speaker.Queue(make([]int16, audio.SpeakerRate/50)); err != nil {
		fail("speaker: %v", err)
	}
}

// checkDotSpeaker: the ready tone must come back on the ADCs' loopback (after the software
// volume, so even at 10 % well above the silence's 0): the Dot's DAC plays.
func (d *device) checkDotSpeaker() {
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if audio.DotLoopback() > 20 {
			return
		}
	}
	errorlog.Add("Self-test: the speaker plays nothing (no loopback of the ready tone)")
	d.errorTill.Store(time.Now().Add(10 * time.Second).UnixMilli())
}

// systemUptime: since when the Echo runs (tells a crash of the assistant from a reboot).
func systemUptime() string {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "?"
	}
	var seconds float64
	fmt.Sscanf(string(data), "%f", &seconds)
	return (time.Duration(seconds) * time.Second).String()
}

func (d *device) addVolumeTool() {
	d.registry.Add(tools.Tool{
		Name: "set_volume",
		Description: "Sets the speaker volume. Give either level (absolute value in percent) or step (relative " +
			"change, e.g. -10 for 'leiser', +10 for 'lauter'), not both. Returns the new volume.",
		Parameters: `{"type": "object", "additionalProperties": false,
			"properties": {"level": {"type": "integer", "minimum": 0, "maximum": 100},
			               "step": {"type": "integer", "minimum": -100, "maximum": 100}},
			"required": []}`,
		Run: func(args map[string]any, _ tools.Context) string {
			d.volMu.Lock()
			// a step wins: the model sends both, the other 0 ({"level":0,"step":-10} for
			// "leiser" set the volume to 0, found by ctl eval on 18.10.2026)
			level, hasLevel := tools.Int(args, "level")
			step, hasStep := tools.Int(args, "step")
			if hasStep && step != 0 {
				d.volume += step
			} else if hasLevel {
				d.volume = level
			} else {
				d.volMu.Unlock()
				return "Error: give level or step"
			}
			d.volume = max(0, min(100, d.volume))
			v := d.volume
			d.volMu.Unlock()
			d.kickVolume()
			return fmt.Sprintf("Volume is now %d.", v)
		},
	})
}

// --- Buttons, ring, LEDs -------------------------------------------------------------

func (d *device) onInput(ev input.Event) {
	switch ev.Kind {
	case input.ActionDown:
		log.Print("[Button] action")
		d.haFire("button", nil)
		// A ringing timer: the button stops it, nothing else (#25).
		if d.stopTimer("the action button") {
			return
		}
		// The wake word training (enroll.go): the button ends it.
		if !d.session.Open() && d.enrolling() {
			log.Printf("[Button] enrollment %s", d.stopEnroll())
			return
		}
		// The intercom with another Echo: the button ends it, on either side (intercom.go).
		if !d.session.Open() && intercomActive() != nil {
			log.Printf("[Button] %s", d.endIntercom(true, "the action button"))
			return
		}
		// During a call the button belongs to the call (#54) and is decided on release: short is
		// "yes / next" (answer, Jarvis joins or leaves), long is "end" (reject, hang up).
		if !d.session.Open() && d.svc.phone != nil && d.svc.phone.ButtonOwned() {
			d.callPress = true
			return
		}
		// A call at another Echo: the button fetches it here, as for a call here (roomlink.go).
		if d.roomOwnsButton() {
			d.roomPress = true
			return
		}
		// The mute button's red LED is switched by the hardware on every press: software cannot
		// turn it off. So the microphone follows the button only, and while it is muted the
		// action button gives a low tone instead of starting a conversation (16.10.2026).
		if !d.session.Open() && d.muted.Load() {
			d.speaker.Tone(220, 200, 0.15)
			d.ring.Show(leds.Solid(leds.Red), 1500*time.Millisecond)
			return
		}
		d.toggle()
	case input.ActionUp:
		if d.roomPress {
			d.roomPress = false
			log.Printf("[Button] call at another Echo, held %s: %s", ev.Held.Round(10*time.Millisecond), d.roomButton(ev.Held))
		}
		if d.callPress {
			d.callPress = false
			log.Printf("[Button] call, held %s: %s", ev.Held.Round(10*time.Millisecond), d.svc.phone.CallButton(ev.Held))
		}
	case input.Muted, input.Unmuted:
		on := ev.Kind == input.Muted
		log.Printf("[Button] mute %v", on)
		d.setMuted(on)
		if on {
			d.speaker.Tone(330, 150, 0.15)
		} else {
			d.speaker.Tone(660, 150, 0.15)
		}
	case input.Ring:
		d.volMu.Lock()
		// Never quite silent: at 0 the assistant would answer inaudibly (16.10.2026).
		d.volume = max(minVolume, min(100, d.volume-ev.Delta*2)) // clockwise (-1) is louder
		d.volMu.Unlock()
		d.kickVolume()
	}
}

const minVolume = 10

func (d *device) setMuted(on bool) {
	d.muted.Store(on)
	if d.mic != nil {
		d.mic.SetMuted(on)
	}
}

func (d *device) kickVolume() {
	select {
	case d.volKick <- struct{}{}:
	default:
	}
}

// volumeWorker applies the volume at most every 150 ms (the ring sends many detents) and
// shows the level on the ring.
func (d *device) volumeWorker() {
	for range d.volKick {
		time.Sleep(150 * time.Millisecond)
		d.volMu.Lock()
		v := d.volume
		changed := v != d.volApplied
		d.volApplied = v
		fromSpotify := d.volFromSpot
		d.volFromSpot = false
		d.volMu.Unlock()
		if changed && !fromSpotify && d.music != nil && d.music.Playing() {
			// the app's slider follows the ring (Spotify sends the event back: same value, no loop)
			go d.registry.Call("music_control", fmt.Sprintf(`{"action": "volume", "level": %d, "speaker": %q}`, v, d.cfg.Settings.SpotifyName),
				tools.Context{Channel: tools.Console, Origin: "Ring"})
		}
		if d.ring != nil {
			d.ring.Show(leds.Level(leds.White, float64(v)/100), time.Duration(prompts.Int("VOLUME_DISPLAY_MS", 1500))*time.Millisecond)
		}
		if changed {
			d.speaker.SetVolume(v)
			audio.SetDotBluetoothVolume(v)
			if d.music != nil {
				d.music.SetVolume(v)
			}
			if d.btMusic != nil {
				d.btMusic.SetVolume(v)
			}
			if d.night.Load() {
				d.nightVolumeChanged(v) // in night mode the ring, voice and Spotify set the night volume
				log.Printf("[Volume] %d (night)", v)
			} else {
				log.Printf("[Volume] %d", v)
			}
		}
	}
}

// showState keeps the ring in line with the device state.
func (d *device) showState() {
	if d.ring == nil {
		return
	}
	current := ""
	for range time.Tick(100 * time.Millisecond) {
		key, pattern := d.pattern()
		if key != current {
			current = key
			d.ring.Set(pattern)
		}
	}
}

func (d *device) pattern() (string, leds.Pattern) {
	// The household's wishes (16.10.2026): idle dark, listening turquoise, thinking purple as on the
	// M5, problems yellow, muted red, the volume as a white bar.
	if time.Now().UnixMilli() < d.errorTill.Load() {
		return "error", leds.Flash(leds.Yellow, 500*time.Millisecond)
	}
	state := d.session.State()
	if d.session.Warm() {
		state = live.Idle // connected ahead and dropped: nothing to show (capture.go)
	}
	micLevel := func() float64 {
		if d.mic == nil {
			return 0
		}
		return float64(d.mic.Peak()) / 8000
	}
	switch {
	case d.timers != nil && d.timers.Ringing():
		return "timer", leds.Flash(leds.White, 400*time.Millisecond)
	case d.listening(): // the first words, before GPT Live (capture.go)
		return "listening", leds.Live(leds.Teal, micLevel)
	case (state == live.Connecting || state == live.Starting) && d.session.Request().Heard != "":
		// the user has spoken, GPT Live connects to answer
		return "thinking", leds.Spinner(leds.Purple, leds.Teal.Scale(0.35), 1500*time.Millisecond)
	case state == live.Connecting || state == live.Starting:
		return "connecting", leds.Spinner(leds.Teal, leds.Teal.Scale(0.1), time.Second)
	case state == live.Live && d.session.Working() && !d.speaker.Speaking(0):
		return "thinking", leds.Spinner(leds.Purple, leds.Teal.Scale(0.35), 1500*time.Millisecond) // still listening: turquoise, purple running round
	case state == live.Live && d.speaker.Speaking(0):
		return "speaking", leds.Voice(leds.Teal, d.speaker.Level) // brighter towards white with his voice
	case state == live.Live:
		return "listening", leds.Live(leds.Teal, func() float64 {
			if d.mic == nil {
				return 0
			}
			return float64(d.mic.Peak()) / 8000
		})
	case state == live.Closing:
		return "closing", leds.Solid(leds.Teal.Scale(0.2))
	// It rings at the other end of a call the user makes themselves: a light running round while
	// they wait, next to the ringback on the speaker.
	// A call on the mobile phone rings at home: from the first second, slowly - the channel that
	// disturbs nobody (#51).
	// The pairing window is open: blue, as long as a phone may pair (#51).
	case d.svc.bluetooth != nil && d.svc.bluetooth.Open():
		return "pairing", leds.Pulse(leds.Blue, 0.6, 2*time.Second)
	case d.svc.phone != nil && d.svc.phone.MobileRinging():
		return "mobile ringing", leds.Pulse(leds.Green, 0.5, 3*time.Second)
	// The landline rings along on a secondary Echo: the same, the button takes it here (#63).
	case d.svc.phone != nil && d.svc.phone.LineRinging():
		return "ringing", leds.Pulse(leds.Green, 0.5, 3*time.Second)
	// Another Echo's call is audible here (roomlink.go): as a call on the speaker.
	case roomHere() != nil:
		return "call elsewhere", leds.Pulse(leds.Green, 0.6, time.Second)
	// The intercom with another Echo: an open microphone, so always visible.
	case intercomActive() != nil:
		return "intercom", leds.Pulse(leds.Teal, 0.6, time.Second)
	case d.svc.phone != nil && d.svc.phone.Dialling():
		return "calling", leds.Spinner(leds.Green, leds.Green.Scale(0.1), 1200*time.Millisecond)
	// The call is audible in the room: the ring follows the voices of the call, as it follows the
	// music of Spotify. Without audio in the room (the caller only hears it at their end) a
	// pulse, brighter and faster than a call the assistant has to itself.
	case d.svc.phone != nil && d.svc.phone.BridgeActive():
		if d.svc.phone.Bridge() == "monitor" {
			return "monitor", leds.Pulse(leds.Green, 0.6, time.Second)
		}
		return "bridge", leds.Spectrum(d.svc.phone.Spectrum)
	// A call runs (Amazon's colour for calls): The assistant's own, or one the user has themselves.
	case d.phoneSession != nil && d.phoneSession.Open() || d.svc.phone != nil && d.svc.phone.InCall():
		return "phone", leds.Pulse(leds.Green, 0.25, 2*time.Second)
	case d.muted.Load():
		return "muted", leds.Solid(leds.Red)
	case d.musicRing.Load() && d.music != nil && d.music.Playing():
		return "music", leds.Spectrum(d.music.Spectrum) // the ring as a music display (the user's idea)
	}
	return "idle", leds.Solid(leds.Off)
}

// --- Control socket (development and tests over SSH) ---------------------------------

func (d *device) serveControl() {
	os.MkdirAll(filepath.Dir(*ctlPath), 0700)
	os.Remove(*ctlPath)
	l, err := net.Listen("unix", *ctlPath)
	if err != nil {
		log.Printf("[Control] %v", err)
		return
	}
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			fmt.Fprintln(c, d.command(strings.TrimSpace(line)))
		}()
	}
}

func (d *device) command(line string) string {
	cmd, arg, _ := strings.Cut(line, " ")
	switch cmd {
	case "start":
		d.session.Start(live.Request{})
		return "ok"
	case "stop":
		d.session.Close()
		return "ok"
	case "toggle":
		d.toggle()
		return "ok"
	case "status":
		d.volMu.Lock()
		vol := d.volApplied
		d.volMu.Unlock()
		status := fmt.Sprintf("version=%s commit=%s state=%s muted=%v volume=%d gate=%v time_synced=%v mic_peak=%d underruns=%d",
			version, buildCommit(), d.session.State(), d.muted.Load(), vol, d.session.EchoGate(), netx.TimeSynced, d.mic.Peak(), d.speaker.Underruns())
		if board.Dot2 {
			status += fmt.Sprintf(" loopback_peak=%d", audio.DotLoopback()) // since the last status
		}
		return status
	case "say":
		d.session.Commentary(arg)
		return "ok"
	case "spotifyvolume":
		// from librespot's event: 0..65535
		raw, err := strconv.Atoi(arg)
		if err != nil {
			return "error: spotifyvolume 0..65535"
		}
		v := max(minVolume, min(100, (raw*100+32767)/65535))
		d.volMu.Lock()
		current := d.volume
		if d.music != nil && !d.music.Playing() {
			// Not playing here yet: Spotify sends the level it kept for this speaker (16.10.: a
			// test's 10 %) before the first sound. The device's own volume wins; the app's
			// slider is set to it.
			d.volMu.Unlock()
			if v != current {
				log.Printf("[Volume] Spotify's kept %d ignored, %d sent back", v, current)
				go d.registry.Call("music_control", fmt.Sprintf(`{"action": "volume", "level": %d, "speaker": %q}`,
					current, d.cfg.Settings.SpotifyName), tools.Context{Channel: tools.Console, Origin: "Spotify"})
			}
			return "ok"
		}
		d.volume = v
		d.volFromSpot = true
		d.volMu.Unlock()
		d.kickVolume()
		return "ok"
	case "musicring":
		d.musicRing.Store(arg == "on")
		return fmt.Sprintf("music ring %v", d.musicRing.Load())
	case "peers":
		return d.peersText()
	case "primary":
		// this device takes the primary role over from the current one (failover.go)
		return d.becomePrimary()
	case "wake":
		// the wake word's probability now and its peak since the last call (tuning the cutoff)
		if d.wake == nil {
			return "wake word off"
		}
		latest, peak := d.wake.Probability()
		return fmt.Sprintf("wake latest=%.2f peak=%.2f cutoff=%.2f (device %.2f, sensitivity %s)", latest, peak,
			d.wake.Cutoff(), d.cfg.Settings.WakeCutoff, d.sensitivityText())
	case "tool":
		// tool <name> [json]: runs a tool directly (tests without a paid conversation)
		name, args, _ := strings.Cut(arg, " ")
		out := d.registry.Call(name, args, tools.Context{Channel: tools.Console, Origin: "Konsole"})
		return strings.ReplaceAll(out, "\n", "\\n")
	case "describe":
		// describe <file>: the tool list as the backend gets it in a voice conversation (JSON)
		list, _ := json.MarshalIndent(d.registry.Describe(tools.Context{Channel: tools.Voice}), "", " ")
		if err := os.WriteFile(arg, list, 0o600); err != nil {
			return "error: " + err.Error()
		}
		return fmt.Sprintf("%d bytes", len(list))
	case "eval":
		// eval <cases.json> [runs] [id prefix]: the backend against example requests, no tool runs
		return d.startEval(arg)
	case "tools":
		return strings.Join(d.registry.Names(), " ")
	case "parcels":
		// one parcel pass now, then 17TRACK's answer through HA and the kept state (#7)
		if d.svc.parcels == nil {
			return "error: no parcel tracking here (primary device with HA and watched mailboxes only)"
		}
		return strings.ReplaceAll(d.svc.parcels.Report(), "\n", "\\n")
	case "prefer":
		// prefer <mail_digest|briefing|evening|urgent>: today's value for the admin from the rules
		// (asked in the background: the first call may give the start value)
		starts := map[string][2]any{"mail_digest": {"MAILWATCH_DIGEST_HOUR", 18}, "briefing": {"AGENDA_BRIEFING_FROM_HOUR", 4},
			"evening": {"AGENDA_EVENING_HOUR", 18}, "urgent": {"AGENDA_URGENT_MIN", 15}}
		s, ok := starts[arg]
		if !ok || d.svc.prefs == nil {
			return "error: prefer mail_digest|briefing|evening|urgent"
		}
		start := prompts.Int(s[0].(string), s[1].(int))
		value := 0
		for i := 0; i < 50; i++ { // up to 10 s for the background question
			if arg == "urgent" {
				value = d.svc.prefs.Minutes(d.svc.people.Admin(), arg, []int{5, 10, 15, 20, 30, 45, 60}, start)
			} else {
				value = d.svc.prefs.Hour(d.svc.people.Admin(), arg, start)
			}
			if value != start || i > 15 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Sprintf("%s today: %d (start value %d)", arg, value, start)
	case "initiative":
		// initiative: a pass now; initiative weigh <kind>: <text>: a verdict, nothing recorded
		return d.initiativeCommand(arg)
	case "errors":
		return strings.ReplaceAll(strings.TrimSpace(errorlog.Read()), "\n", "\\n")
	case "gate":
		d.session.SetEchoGate(arg == "on")
		return fmt.Sprintf("echo gate %v", d.session.EchoGate())
	case "tone":
		d.speaker.Tone(880, 150, 0.2)
		return "ok"
	case "enroll":
		return d.enrollCommand(arg)
	case "echotest":
		// echotest <mobile number> [sentences] [nogate]: the landline calls the paired phone, the Echo answers
		// it on the speaker, and the test sentences that come back measure what a caller hears of himself
		if d.svc.phone == nil {
			return "error: no phone"
		}
		parts := strings.Fields(arg)
		if len(parts) == 0 {
			return "error: echotest <number> [sentences] [nogate]"
		}
		sentences := 4
		if len(parts) > 1 {
			sentences, _ = strconv.Atoi(parts[1])
		}
		// "nogate": the cross-check - without the gate the sentence must come back
		return d.svc.phone.EchoTest(parts[0], sentences, len(parts) > 2 && parts[2] == "nogate")
	case "volume":
		v, err := strconv.Atoi(arg)
		if err != nil {
			return "error: volume N"
		}
		d.volMu.Lock()
		d.volume = max(minVolume, min(100, v))
		d.volMu.Unlock()
		d.kickVolume()
		return "ok"
	case "inject":
		// Feeds a raw s16le 16 kHz mono file as microphone input (tests without a person).
		data, err := os.ReadFile(arg)
		if err != nil {
			return "error: " + err.Error()
		}
		go d.inject(data)
		return fmt.Sprintf("injecting %.1f s", float64(len(data))/2/audio.MicRate)
	case "ledtest":
		// one red LED running round clockwise from 12 o'clock, one position a second
		if d.ring == nil {
			return "error: no ring"
		}
		d.ring.Show(leds.Chase(leds.Red, time.Second), 13*time.Second)
		return "ok"
	case "led":
		// led <index> <rrggbb>: calibration of the ring's order and offset
		parts := strings.Fields(arg)
		if len(parts) != 2 || d.ring == nil {
			return "error: led <index> <rrggbb>"
		}
		i, _ := strconv.Atoi(parts[0])
		c, _ := strconv.ParseUint(parts[1], 16, 32)
		d.ring.Show(leds.Single(i, leds.RGB{R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c)}), 10*time.Second)
		return "ok"
	}
	return "error: unknown command (start, stop, toggle, status, say, tool, tools, gate, tone, enroll, volume, inject, led)"
}

// hear: 16 kHz microphone audio (or injected test audio). The conversation takes it; while none
// is open the wake word listens, as on the M5: not while muted, nor while the speaker plays
// (an announcement saying "Jarvis" must not wake it).
func (d *device) hear(pcm []int16) {
	// One packet at a time: ctl inject began while the microphone's packet was still in here, and
	// two at once in the wake word model panicked (index out of range, 28.10.2026).
	d.hearMu.Lock()
	defer d.hearMu.Unlock()
	d.pushToTalk(pcm)  // the Fire TV's remote: its microphone key is the action button (tvboard.go)
	d.feedCapture(pcm) // first: the packet that hands over to GPT Live goes to the session (capture.go)
	d.session.Feed(pcm)
	d.levels.feed(pcm)
	d.hearRoom(pcm)     // another Echo's call has its room here (roomlink.go)
	d.hearIntercom(pcm) // the intercom with another Echo (intercom.go)
	// A call on the microphone at home (speaker_call talk, the intercom): the voice goes onto the
	// line as well.
	bridging := false
	if d.svc.phone != nil {
		d.svc.phone.Hear(pcm)
		// Ringing too: "Jarvis, geh ran" must be heard while the ring plays (the DSP takes it out
		// of the microphone as it does the call; the announcement never says "Jarvis").
		bridging = d.svc.phone.BridgeActive() || d.svc.phone.MobileRinging()
	}
	// The intercom plays the other room: the wake word stays on for "Jarvis, Verbindung beenden"
	// (the DSP takes the speaker out of the microphone). A ringing timer too: "Jarvis" stops it.
	timerRings := d.timers != nil && d.timers.Ringing()
	bridging = bridging || intercomActive() != nil || timerRings
	if d.enrolling() {
		if e := d.enroll.Load(); e != nil {
			e.feed(pcm)
		}
		d.wakeStale = true
		return
	}
	if d.wake == nil {
		return
	}
	// While a call plays on the speaker the wake word stays on, although the speaker is loud: the
	// DSP takes the call out of the microphone again, so what is left is the room - and without
	// the wake word nobody at home could say "Verbindung beenden".
	if d.session.Open() || d.listening() || d.muted.Load() || !bridging && d.speaker.Speaking(300*time.Millisecond) {
		d.wakeStale = true
		return
	}
	if d.wakeStale {
		d.wake.Reset() // warms up again (~1 s), as the M5 after a session
		d.wakeStale = false
	}
	music := d.music != nil && d.music.Playing()
	// A ringing phone or a call on the speaker is the same case as music: the DSP takes the
	// speaker out of the microphone and the user's voice with it in part ("Jarvis, geh ran"
	// scored 0.35 while the ring played, 17.10.2026).
	ringing := d.svc.phone != nil && d.svc.phone.MobileRinging()
	if ringing {
		// lower still: it scored 0.40-0.50 while the ring played, and a false hit only answers
		d.wake.SetCutoff(prompts.Float("MOBILE_WAKE_CUTOFF", 0.4))
	} else if music || bridging {
		d.wake.SetCutoff(d.wakeCutoff(d.cfg.Settings.WakeCutoffMusic))
	} else {
		d.wake.SetCutoff(d.wakeCutoff(d.cfg.Settings.WakeCutoff))
	}
	d.wakeClips.feed(pcm)
	heard := d.wake.Feed(pcm)
	if music && !heard && d.wake.Latest() >= 0.25 {
		// "Hey Jar…" under music: silent already, as Alexa does, so the command after the wake
		// word is not lost in the music (it was, 18.10.2026); a false start costs a short dip
		d.music.HushFor(1500 * time.Millisecond)
	}
	if heard || d.wakeClips.takeSecond() {
		_, peak := d.wake.Probability()
		if heard {
			log.Printf("[Wake word] heard (%.2f, music %v, mic peak %d)", peak, music, d.mic.Peak())
			d.wakeClips.save("hit", peak, d.wake.Cutoff(), music)
		}
		// "Jarvis" while a timer rings stops it, as Alexa's "Alexa, Stopp" does; no conversation.
		if timerRings && d.stopTimer("the wake word") {
			d.wakeClips.handled("timer")
			return
		}
		// "Jarvis" while the phone rings picks it up at once: the first try opened a conversation
		// and answered "geh ran" 9 s later, the caller waiting (17.10.2026). To turn it away, the
		// action button while it rings is answer too - "Jarvis, leg auf" ends it after.
		if ringing && d.svc.phone.MobileButton() != "" {
			log.Print("[Wake word] the phone rang: answered")
			d.wakeClips.handled("phone")
			return
		}
		// During a call on the speaker "Jarvis" brings it into it (#54) - a conversation of its
		// own at the speaker held the call for a minute and left it 1.5 s late (17.10.2026).
		if d.svc.phone != nil && d.svc.phone.ConferenceActive() {
			d.wakeClips.handled("call")
			return // he is in the call and listening
		}
		if d.svc.phone != nil && d.svc.phone.CallOnSpeaker() {
			log.Printf("[Wake word] during a call: %s", d.svc.phone.StartConference())
			d.wakeClips.handled("call")
			return
		}
		// The device takes the first words itself and decides before GPT Live connects
		// (capture.go); the ring shows it listens, no cue. Another Echo may have heard it too: the
		// louder one keeps it (peers.go).
		if d.listen(live.Request{Wake: true}) && d.peers != nil {
			go d.arbitrate(peak, time.Now())
		}
		d.haFire("wake_word", map[string]any{"score": round2(peak)})
	}
}

func (d *device) inject(data []byte) {
	d.injecting.Store(true)
	defer d.injecting.Store(false)
	pcm := make([]int16, len(data)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	const chunk = audio.MicRate / 50
	next := time.Now()
	for i := 0; i < len(pcm); i += chunk {
		d.hear(pcm[i:min(i+chunk, len(pcm))])
		next = next.Add(20 * time.Millisecond)
		time.Sleep(time.Until(next))
	}
}

func control(command string) int {
	c, err := net.Dial("unix", *ctlPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer c.Close()
	fmt.Fprintln(c, command)
	reply, _ := bufio.NewReader(c).ReadString('\n')
	fmt.Print(reply)
	if strings.HasPrefix(reply, "error") {
		return 1
	}
	return 0
}
