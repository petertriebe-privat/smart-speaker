// Package firetv: the Fire TV sticks through Home Assistant, as devices/atoms3r/src/services/firetv.cpp,
// from two integrations that complement each other:
//   - Android Debug Bridge (androidtv, local, needs ADB debugging on the stick): state and running
//     app, on/off, starting an app and remote keys, all as shell commands.
//   - Alexa Devices (cloud): a text command to the stick's own Alexa, the only way to play a film or
//     series by its title (Alexa resolves it through Amazon's catalog of the apps).
//
// The sticks come from HA at run time: Alexa's devices whose model names a Fire TV, ADB's media
// players; one of each with the same room (or the same name) is the same stick. Without ADB only
// the Alexa part works. Tool: fire_tv (slow: Alexa goes through Amazon's cloud).
package firetv

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"echo-assistant/internal/homeassistant"
	"echo-assistant/internal/tools"
)

// The sticks as HA knows them: "alexa" are Alexa's devices whose model names a Fire TV and that
// are reachable (an entity not unavailable), "adb" the media players of the Android Debug Bridge.
const tvsTemplate = "{% set ns = namespace(alexa=[], adb=[], seen=[]) %}" +
	"{% for e in integration_entities('alexa_devices') %}{% set d = device_id(e) %}" +
	"{% if d and d not in ns.seen and 'fire tv' in (device_attr(d, 'model') or '') | lower %}" +
	"{% set ns.seen = ns.seen + [d] %}" +
	"{% if device_entities(d) | reject('is_state', 'unavailable') | list | count > 0 %}" +
	"{% set ns.alexa = ns.alexa + [{'name': device_attr(d, 'name_by_user') or device_attr(d, 'name')," +
	" 'area': area_name(d) or '', 'device': d}] %}" +
	"{% endif %}{% endif %}{% endfor %}" +
	"{% for e in integration_entities('androidtv') | select('match', 'media_player[.]') %}" +
	"{% set ns.adb = ns.adb + [{'name': state_attr(e, 'friendly_name'), 'area': area_name(e) or '', 'player': e}] %}" +
	"{% endfor %}{{ {'alexa': ns.alexa, 'adb': ns.adb} | tojson }}"

// Shell commands through androidtv.adb_command. The integration's own app detection, app start
// (select_source) and key names fail on Fire OS 8 ("dumpsys window windows" lost mCurrentFocus),
// its shell works. Every command stays short: one that ran ~10 s lost its answer.
const (
	statusCommand = "dumpsys power | grep mWakefulness=; dumpsys window | grep mCurrentFocus; " +
		"dumpsys media_session | grep -E 'package=|state=PlaybackState'; " +
		"dumpsys activity activities | grep 'cmp=android/com.android.internal.app.ResolverActivity' | head -1"
	wakeCommand = "if dumpsys power 2>/dev/null | grep -qE 'mWakefulness=(Asleep|Dozing)'; then " +
		"input keyevent 224; input keyevent 3; echo woken; fi"
	appsCommand = "cmd package query-activities --brief -a android.intent.action.MAIN " +
		"-c android.intent.category.LEANBACK_LAUNCHER | grep / | cut -d/ -f1 | sort -u"
	// One line per labelled or focused element: "focused|text|content-desc" (uiautomator keeps
	// this attribute order). The \n and \1.. are meant literally for sed.
	screenCommand = `uiautomator dump /sdcard/assistant_ui.xml >/dev/null 2>&1; ` +
		`sed 's/<node/\n<node/g' /sdcard/assistant_ui.xml | ` +
		`sed -n 's/.* text="\([^"]*\)".* content-desc="\([^"]*\)".* focused="\([a-z]*\)".*/\3|\1|\2/p' | ` +
		`grep -v '^false||$'`
)

const (
	maxRepeat      = 10
	alexaPolls     = 8                      // after an Alexa command: look this often ...
	alexaPoll      = 800 * time.Millisecond // ... at this interval (plus ~1 s per look)
	maxPresses     = 2                      // OK presses on an app that does not start playing
	screenMaxChars = 1500                   // screen text for the model
)

// tv is one stick: the Alexa device id and/or the ADB media player ("" = not in that integration).
type tv struct {
	Name, Area, Alexa, Player string
}

// keys: the remote keys of the tool and their Android key codes.
var keys = map[string]int{
	"home": 3, "back": 4, "up": 19, "down": 20, "left": 21, "right": 22,
	"select": 23, "menu": 82, "play_pause": 85, "rewind": 89, "fast_forward": 90,
}

// appNames: names for packages that do not say what they are (generic Fire TV apps, not configuration).
var appNames = map[string]string{
	"com.amazon.firebat":            "Prime Video",
	"com.amazon.bueller.music":      "Amazon Music",
	"com.amazon.firetv.youtube":     "YouTube",
	"com.amazon.spiderpork":         "Silk Browser",
	"com.amazon.tv.launcher":        "home screen",
	"com.amazon.ftv.screensaver":    "screensaver",
	"com.amazon.vizzini":            "Alexa overlay",
	"com.netflix.ninja":             "Netflix",
	"com.disney.disneyplus":         "Disney+",
	"com.apple.atve.amazon.appletv": "Apple TV",
	"de.cbc.tvnow.firetv":           "RTL+",
	"de.telekom.magentatv.firetv":   "MagentaTV",
}

// state is what statusCommand found on a stick.
type state struct {
	power    string // on, screensaver, standby ("" = unknown)
	front    string // package in front
	playing  string // package playing (or paused, see paused)
	paused   bool
	position int    // ms into what plays
	chooser  string // link of an open "Open with" dialog
}

// Service is the fire_tv tool.
type Service struct {
	ha    *homeassistant.HA
	calls atomic.Uint32
}

// New creates the service on a Home Assistant connection.
func New(ha *homeassistant.HA) *Service { return &Service{ha: ha} }

func lower(text string) string { return strings.ToLower(strings.TrimSpace(text)) }

func lines(text string) []string {
	var result []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result = append(result, line)
		}
	}
	return result
}

// leadingInt parses the number a string starts with (Arduino's toInt()); 0 if none.
func leadingInt(text string) int {
	end := 0
	for end < len(text) && (text[end] >= '0' && text[end] <= '9' || end == 0 && text[end] == '-') {
		end++
	}
	n, _ := strconv.Atoi(text[:end])
	return n
}

func appName(pkg string) string {
	if name, ok := appNames[pkg]; ok {
		return name
	}
	return pkg
}

func label(t tv) string {
	if t.Area != "" {
		return t.Name + " (" + t.Area + ")"
	}
	return t.Name
}

func names(tvs []tv) string {
	parts := make([]string, len(tvs))
	for i, t := range tvs {
		parts[i] = label(t)
	}
	return strings.Join(parts, ", ")
}

// sameStick: the ADB player that is the same stick as an Alexa device: same room, else same name.
func sameStick(alexa, adb tv) bool {
	if alexa.Area != "" && adb.Area != "" {
		return lower(alexa.Area) == lower(adb.Area)
	}
	return lower(alexa.Name) == lower(adb.Name)
}

func (s *Service) loadTvs() ([]tv, string) {
	status, body, err := s.ha.Rest("POST", "/api/template", map[string]any{"template": tvsTemplate})
	if err != nil || status != 200 {
		return nil, homeassistant.Failure(status, body, err)
	}
	var response struct {
		Alexa []struct{ Name, Area, Device string } `json:"alexa"`
		Adb   []struct{ Name, Area, Player string } `json:"adb"`
	}
	json.Unmarshal(body, &response)
	var tvs []tv
	for _, item := range response.Alexa {
		tvs = append(tvs, tv{Name: item.Name, Area: item.Area, Alexa: item.Device})
	}
	for _, item := range response.Adb {
		adb := tv{Name: item.Name, Area: item.Area, Player: item.Player}
		var stick *tv
		for i := range tvs {
			if tvs[i].Player == "" && sameStick(tvs[i], adb) {
				stick = &tvs[i]
				break
			}
		}
		if stick == nil {
			tvs = append(tvs, adb)
			continue
		}
		stick.Player = adb.Player
		if stick.Area == "" {
			stick.Area = adb.Area
		}
	}
	if len(tvs) == 0 {
		return nil, "Error: Home Assistant reports no reachable Fire TV (integrations Alexa Devices, Android Debug Bridge)."
	}
	return tvs, ""
}

// find returns the stick meant by wanted: its exact name, else the only one in that room or with
// the name part. Error text otherwise.
func find(tvs []tv, wanted string) (*tv, string) {
	key := lower(wanted)
	for i := range tvs {
		if lower(tvs[i].Name) == key {
			return &tvs[i], ""
		}
	}
	var hits []tv
	for _, t := range tvs {
		if key != "" && (lower(t.Area) == key || strings.Contains(lower(t.Name), key)) {
			hits = append(hits, t)
		}
	}
	if len(hits) == 1 {
		return &hits[0], ""
	}
	switch {
	case key == "":
		return nil, "Error: device missing. Fire TVs: " + names(tvs)
	case len(hits) == 0:
		return nil, "Error: no Fire TV \"" + wanted + "\". Fire TVs: " + names(tvs)
	}
	return nil, "Error: several Fire TVs match \"" + wanted + "\": " + names(hits) + ". Name one."
}

// call runs a HA service; "" on success, else the error text.
func (s *Service) call(service string, data map[string]any) string {
	status, body, err := s.ha.Rest("POST", "/api/services/"+service, data)
	if err == nil && status == 200 {
		return ""
	}
	return homeassistant.Failure(status, body, err)
}

// shell runs a command on the stick; its output is the player's attribute adb_response afterwards
// (the action returns only when the command is done). HA keeps the old answer when a command
// prints nothing or times out, so every command ends with its own marker. Returns the output
// and "" or the error text.
func (s *Service) shell(t tv, command string) (string, string) {
	marker := fmt.Sprintf("@end%d-%d", s.calls.Add(1), time.Now().UnixMilli())
	if errText := s.call("androidtv/adb_command",
		map[string]any{"entity_id": t.Player, "command": command + "; echo " + marker}); errText != "" {
		return "", errText
	}
	status, body, err := s.ha.Rest("GET", "/api/states/"+t.Player, nil)
	if err != nil || status != 200 {
		return "", homeassistant.Failure(status, body, err)
	}
	var response struct {
		State      string `json:"state"`
		Attributes struct {
			AdbResponse string `json:"adb_response"`
		} `json:"attributes"`
	}
	json.Unmarshal(body, &response)
	if response.State == "unavailable" {
		return "", "Error: " + t.Name + " does not answer over ADB (off the network, or ADB debugging off)"
	}
	output := strings.TrimSpace(response.Attributes.AdbResponse)
	if !strings.HasSuffix(output, marker) {
		return "", "Error: no answer from " + t.Name + " (command took too long)"
	}
	return output[:len(output)-len(marker)], ""
}

func noAdb(t tv) string {
	return "Error: " + t.Name + " is not in the Android Debug Bridge integration of Home Assistant; " +
		"use action alexa instead"
}

func parseState(output string) state {
	st := state{position: -1}
	pkg := ""
	for _, line := range lines(output) {
		switch {
		case strings.HasPrefix(line, "mWakefulness="):
			switch line[13:] {
			case "Awake":
				st.power = "on"
			case "Dreaming":
				st.power = "screensaver"
			default:
				st.power = "standby"
			}
		case strings.HasPrefix(line, "mCurrentFocus="):
			to, from := strings.Index(line, "/"), strings.Index(line, " u0 ")
			if from >= 0 {
				end := to
				if to <= from {
					end = strings.LastIndex(line, "}")
				}
				if end >= from+4 {
					st.front = line[from+4 : end]
				}
			}
		case strings.HasPrefix(line, "package="):
			pkg = line[8:]
		case strings.HasPrefix(line, "state=PlaybackState {state="):
			code := leadingInt(line[27:]) // 3 playing, 2 paused
			better := code == 3 && (st.playing == "" || st.paused)
			if better || (code == 2 && st.playing == "") {
				st.playing = pkg
				st.paused = code == 2
				st.position = -1
				if at := strings.Index(line, "position="); at >= 0 {
					st.position = leadingInt(line[at+9:])
				}
			}
		case strings.Contains(line, "ResolverActivity") && strings.Contains(line, " dat="):
			from := strings.Index(line, " dat=") + 5
			end := strings.Index(line[from:], " ")
			if end < 0 {
				st.chooser = line[from:]
			} else {
				st.chooser = line[from : from+end]
			}
		}
	}
	return st
}

func (s *Service) readState(t tv) (state, string) {
	output, errText := s.shell(t, statusCommand)
	if errText != "" {
		return state{}, errText
	}
	return parseState(output), ""
}

func describe(st state) string {
	text := "state unknown"
	if st.power != "" {
		text = st.power
	}
	if st.front != "" {
		text += ", in front: " + appName(st.front)
	}
	if st.playing != "" {
		if st.paused {
			text += ", paused: " + appName(st.playing)
		} else {
			text += ", playing: " + appName(st.playing)
		}
	}
	if st.chooser != "" {
		text += ", an \"Open with\" dialog waits"
	}
	return text
}

func (s *Service) status(tvs []tv) string {
	var text strings.Builder
	for _, t := range tvs {
		text.WriteString(label(t) + ": ")
		if t.Player == "" {
			text.WriteString("state unknown (Alexa only)")
		} else if st, errText := s.readState(t); errText != "" {
			text.WriteString(errText)
		} else {
			text.WriteString(describe(st))
		}
		text.WriteString("\n")
	}
	return text.String()
}

var xmlUnescaper = strings.NewReplacer("&#10;", " ", "&quot;", "\"", "&apos;", "'", "&lt;", "<", "&gt;", ">", "&amp;", "&")

// screen: what the screen shows: the labelled elements in order, the focused one marked. A
// focused element without a label (a tile) marks the first label inside it.
func (s *Service) screen(t tv) string {
	if t.Player == "" {
		return noAdb(t)
	}
	st, errText := s.readState(t)
	if errText != "" {
		return errText
	}
	output, errText := s.shell(t, screenCommand)
	if errText != "" {
		return errText
	}
	text := t.Name + ": " + describe(st) + "\nScreen (> = focused):\n"
	last := ""
	focusPending, focusShown := false, false
	for _, line := range lines(output) {
		first := strings.Index(line, "|")
		if first < 0 {
			continue
		}
		second := strings.Index(line[first+1:], "|")
		if second < 0 {
			continue
		}
		second += first + 1
		focused := line[:first] == "true"
		lbl := xmlUnescaper.Replace(line[first+1 : second])
		description := xmlUnescaper.Replace(line[second+1:])
		entry := lbl
		if lbl == "" {
			entry = description
		} else if description != "" && lower(description) != lower(lbl) {
			entry += " (" + description + ")"
		}
		if entry == "" {
			if focused {
				focusPending = true
			}
			continue
		}
		mark := !focusShown && (focused || focusPending)
		if mark {
			focusShown, focusPending = true, false
		}
		if entry == last && !mark {
			continue
		}
		last = entry
		if len(text)+len(entry) > screenMaxChars {
			text += "...\n"
			break
		}
		if mark {
			text += "> " + entry + "\n"
		} else {
			text += "- " + entry + "\n"
		}
	}
	if last == "" {
		text += "(nothing readable: video, Alexa overlay or an app drawing its own screen)\n"
	}
	return text
}

// findApp: the installed app meant by wanted (name or package): equal, else the only one
// containing it. "" with the error text otherwise, listing the apps.
func findApp(output, wanted string) (string, string) {
	key := lower(wanted)
	hit := ""
	var list []string
	several := false
	for _, pkg := range lines(output) {
		name := appName(pkg)
		if name == pkg {
			list = append(list, pkg)
		} else {
			list = append(list, name+" ("+pkg+")")
		}
		if lower(pkg) == key || lower(name) == key {
			return pkg, ""
		}
		if !strings.Contains(lower(pkg), key) && !strings.Contains(lower(name), key) {
			continue
		}
		several = several || hit != ""
		hit = pkg
	}
	if hit != "" && !several {
		return hit, ""
	}
	return "", "Error: no single app \"" + wanted + "\". Apps: " + strings.Join(list, ", ")
}

// openLink opens a link from the stick's own "Open with" dialog in the installed app whose
// package contains the link's domain name (www.disneyplus.com -> com.disney.disneyplus). Only
// links of plain characters reach the shell. "" if it went, else why not.
func (s *Service) openLink(t tv, link string) string {
	for i := 0; i < len(link); i++ {
		c := link[i]
		isAlnum := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !isAlnum && !strings.ContainsRune(":/._-?=&%#+~", rune(c)) {
			return "unusual link"
		}
	}
	scheme := strings.Index(link, "://")
	if scheme < 0 {
		return "no domain"
	}
	hostStart := scheme + 3
	host := link[hostStart:]
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	dot := strings.LastIndex(host, ".")
	if dot <= 0 {
		return "no domain"
	}
	host = host[:dot]
	domain := host[strings.LastIndex(host, ".")+1:]
	output, errText := s.shell(t, appsCommand)
	if errText != "" {
		return errText
	}
	pkg := ""
	for _, line := range lines(output) {
		if strings.Contains(lower(line), lower(domain)) {
			pkg = line
		}
	}
	if pkg == "" {
		return "no app for " + domain
	}
	if _, errText := s.shell(t, "am start -a android.intent.action.VIEW -d '"+link+"' -p "+pkg+" >/dev/null 2>&1"); errText != "" {
		return errText
	}
	return ""
}

// inApp: an app's own screen, not the home screen, Alexa's overlay, the screensaver or a system dialog.
func inApp(front string) bool {
	return front != "" && front != "com.amazon.tv.launcher" && front != "com.amazon.vizzini" &&
		front != "com.amazon.ftv.screensaver" && front != "android"
}

// alexaCommand sends text to the stick's Alexa. With ADB it then watches the stick (each look is
// one short shell command): it opens a link Alexa left in an "Open with" dialog in the matching
// app, presses OK when an app stays in front without playing (profile selection: the preselected
// profile; details page: play or continue), and reports whether something new plays.
func (s *Service) alexaCommand(t tv, text string) string {
	if t.Alexa == "" {
		return "Error: " + t.Name + " is not in Alexa Devices, Alexa commands are not possible"
	}
	var before, now, last state
	// A sleeping stick: Alexa would wake it, but not its TV; HOME after waking does (HDMI-CEC).
	// Woken in the same command as the first look.
	watch, woken := false, false
	if t.Player != "" {
		output, errText := s.shell(t, wakeCommand+"; "+statusCommand)
		if errText == "" {
			watch = true
			woken = strings.Contains(output, "woken")
			before = parseState(output)
		}
	}
	if errText := s.call("alexa_devices/send_text_command",
		map[string]any{"device_id": t.Alexa, "text_command": text}); errText != "" {
		return errText
	}
	result := ""
	if woken {
		result = "Switched " + t.Name + " on. "
	}
	result += "Sent to Alexa on " + t.Name + ": " + text
	if !watch {
		return result
	}
	started := time.Now()

	presses := 0
	press, overlay := false, false
	for poll := 0; poll < alexaPolls; poll++ {
		if poll > 0 {
			log.Printf("[Fire TV] %d ms: %s", time.Since(started).Milliseconds(), describe(now))
		}
		var output, errText string
		if press {
			if output, errText = s.shell(t, "input keyevent 23; sleep 2; "+statusCommand); errText != "" {
				continue
			}
			presses++
			press = false
		} else {
			time.Sleep(alexaPoll)
			if output, errText = s.shell(t, statusCommand); errText != "" {
				continue // one slow answer is no reason to stop
			}
		}
		last = now
		now = parseState(output)
		if now.front == "com.amazon.vizzini" {
			overlay = true
		}
		if now.chooser != "" {
			if why := s.openLink(t, now.chooser); why != "" {
				return result + ". An \"Open with\" dialog waits for " + now.chooser + " (" + why + ")"
			}
			result += " (opened Alexa's link in the app)"
			continue
		}
		fresh := now.playing != "" && !now.paused &&
			(now.playing != before.playing || before.paused || now.position < before.position)
		if fresh {
			extra := ""
			if presses > 0 {
				extra = fmt.Sprintf(" (pressed OK %dx: preselected profile or play button)", presses)
			}
			return result + ". Now playing: " + appName(now.playing) + extra
		}
		// Alexa is done once her overlay is gone; the old video playing on means she started nothing.
		settled := (overlay || poll >= 2) && now.front != "com.amazon.vizzini"
		oldPlays := now.playing != "" && !now.paused
		if settled && now.front == before.front && now.playing == before.playing && (!inApp(now.front) || oldPlays) {
			return result + ". Nothing new started: Alexa probably asked back on the TV (e.g. which app); " +
				"repeat the command with the app ('... auf Netflix'). Now: " + describe(now)
		}
		press = settled && inApp(now.front) && !oldPlays && now.front == last.front && presses < maxPresses
	}
	return fmt.Sprintf("%s. Not playing yet (pressed OK %dx). Now: %s. Look with screen, then use key.",
		result, presses, describe(now))
}

func (s *Service) openApp(t tv, app string) string {
	// Without ADB Alexa opens the app; the command is German like Alexa's language.
	if t.Player == "" {
		return s.alexaCommand(t, "Öffne "+app)
	}
	output, errText := s.shell(t, appsCommand)
	if errText != "" {
		return errText
	}
	pkg, errText := findApp(output, app) // from the stick's own list: safe in the shell
	if pkg == "" {
		return errText
	}
	output, errText = s.shell(t, "monkey -p "+pkg+" -c android.intent.category.LEANBACK_LAUNCHER 1 2>&1 | grep -c 'Events injected'")
	if errText != "" {
		return errText
	}
	if strings.TrimSpace(output) == "1" {
		return t.Name + " opens " + appName(pkg)
	}
	return "Error: " + appName(pkg) + " did not start"
}

// WakeAndRun wakes the stick in room (its name or room) and runs command there, e.g. the start of
// the Jarvis screen app (#108). "" on success, else the error text.
func (s *Service) WakeAndRun(room, command string) string {
	tvs, errText := s.loadTvs()
	if errText != "" {
		return errText
	}
	t, errText := find(tvs, room)
	if t == nil {
		return errText
	}
	if t.Player == "" {
		return noAdb(*t)
	}
	_, errText = s.shell(*t, wakeCommand+"; "+command)
	return errText
}

// Fire OS undoes what was set for the assistant app over ADB (29.10.2026): a restart enabled Alexa's
// voice app again (the remote's key opened Alexa) and took the app's microphone; the PC that set
// it is not always on, and adbd refuses a connection from the stick itself. Home Assistant's ADB
// connection is always there: Keep and KeepRunning repair over it.
const (
	alexaVoice = "com.amazon.vizzini"
	// the console unseen: starts the service with the microphone and asks for a missing grant
	// (devices/android: BootReceiver, ConsoleActivity)
	startUnseen = "am start -n %s/.ConsoleActivity --ez background true"
)

// stickOf: the stick in room with ADB, or the error text.
func (s *Service) stickOf(room string) (*tv, string) {
	tvs, errText := s.loadTvs()
	if errText != "" {
		return nil, errText
	}
	t, errText := find(tvs, room)
	if t == nil {
		return nil, errText
	}
	if t.Player == "" {
		return nil, noAdb(*t)
	}
	return t, ""
}

// Keep repairs the assistant app pkg on the stick in room: Alexa's voice app off (unless alexa),
// the microphone and the overlay granted, no microphone symbol for the app (SystemUI's exemption
// list, read when it starts: from the next restart on). Returns what was repaired and the error.
func (s *Service) Keep(room, pkg string, alexa bool) ([]string, string) {
	t, errText := s.stickOf(room)
	if t == nil {
		return nil, errText
	}
	cmd := fmt.Sprintf("r=''; "+
		"if [ %t = false ] && pm list packages -e %s | grep -q %s; then pm disable-user --user 0 %s >/dev/null; r=\"$r alexa\"; fi; "+
		"dumpsys package %s | grep -q 'RECORD_AUDIO: granted=true' || { pm grant %s android.permission.RECORD_AUDIO; r=\"$r microphone\"; }; "+
		"appops get %s SYSTEM_ALERT_WINDOW | grep -q allow || { appops set %s SYSTEM_ALERT_WINDOW allow; r=\"$r overlay\"; }; "+
		"e=$(settings get global sysui_mic_disclosure_exempt); case \"$e\" in *%s*) ;; null|'') settings put global sysui_mic_disclosure_exempt %s; r=\"$r symbol\";; "+
		"*) settings put global sysui_mic_disclosure_exempt \"$e,%s\"; r=\"$r symbol\";; esac; echo \"repaired:$r\"",
		alexa, alexaVoice, alexaVoice, alexaVoice, pkg, pkg, pkg, pkg, pkg, pkg, pkg)
	out, errText := s.shell(*t, cmd)
	if errText != "" {
		return nil, errText
	}
	_, list, _ := strings.Cut(out, "repaired:")
	return strings.Fields(list), ""
}

// KeepRunning starts the assistant app pkg unseen on every stick that has it and where it does not
// run (a restart without the boot broadcast, a crash). The names of the sticks where it was
// started; sticks without ADB or without the app are left alone.
func (s *Service) KeepRunning(pkg string) []string {
	tvs, errText := s.loadTvs()
	if errText != "" {
		return nil
	}
	var started []string
	for _, t := range tvs {
		if t.Player == "" {
			continue
		}
		out, errText := s.shell(t, fmt.Sprintf("if pm path %s >/dev/null 2>&1 && ! pidof %s >/dev/null; then "+
			startUnseen+" >/dev/null; echo started; fi", pkg, pkg, pkg))
		if errText == "" && strings.Contains(out, "started") {
			started = append(started, t.Name)
		}
	}
	return started
}

// power: on is wakeCommand. An awake stick is left alone, HOME would leave the app in front.
func (s *Service) power(t tv, on bool) string {
	if t.Player == "" {
		return noAdb(t)
	}
	command := "input keyevent 223"
	if on {
		command = wakeCommand
	}
	output, errText := s.shell(t, command)
	if errText != "" {
		return errText
	}
	if !on {
		return t.Name + " switched off"
	}
	if strings.Contains(output, "woken") {
		return t.Name + " switched on"
	}
	return t.Name + " was already on (if its TV is dark: key home)"
}

func (s *Service) key(t tv, name string, repeat int) string {
	if t.Player == "" {
		return noAdb(t)
	}
	code, ok := keys[name]
	if !ok {
		return "Error: unknown key \"" + name + "\""
	}
	repeat = min(max(repeat, 1), maxRepeat)
	command := "input keyevent"
	for i := 0; i < repeat; i++ {
		command += " " + strconv.Itoa(code)
	}
	if _, errText := s.shell(t, command); errText != "" {
		return errText
	}
	result := t.Name + ": " + name
	if repeat > 1 {
		result += " x" + strconv.Itoa(repeat)
	}
	return result
}

// YouTube runs a YouTube action on a stick directly over ADB: open (link), pause, play, stop.
// Each is one short shell command that wakes the stick first, so a stick in standby just does it
// (the TV follows over HDMI-CEC). DIAL and YouTube's lounge, the casting way, needed the app
// running and several round trips; a stick in standby failed and slow ones were reported as
// errors although they did it (18.10.2026). handled false: no such stick with ADB (or ADB does
// not answer): the caller uses its own way.
func (s *Service) YouTube(wanted, action, link string) (handled bool, result string) {
	if !s.ha.Configured() {
		return false, ""
	}
	tvs, errText := s.loadTvs()
	if errText != "" {
		return false, ""
	}
	t, _ := find(tvs, wanted)
	if t == nil {
		// DIAL's name of the stick ("Fire TV Arbeitszimmer"): its room is the last word
		if fields := strings.Fields(wanted); len(fields) > 1 {
			t, _ = find(tvs, fields[len(fields)-1])
		}
	}
	if t == nil || t.Player == "" {
		return false, ""
	}
	const wake = "input keyevent 224; " // WAKEUP: nothing when it is awake
	if action == "play" {
		return true, s.resumeYouTube(*t, link)
	}
	var command string
	switch action {
	case "open":
		command = wake + "am start -a android.intent.action.VIEW -d '" + link + "' -p com.amazon.firetv.youtube >/dev/null 2>&1"
	case "pause": // not woken: a sleeping stick plays nothing
		command = "input keyevent 127" // MEDIA_PAUSE: never toggles into play
	case "play":
		command = wake + "sleep 1; input keyevent 126" // MEDIA_PLAY, once the app is back after a wake
	case "stop":
		command = "input keyevent 86"
	default:
		return false, ""
	}
	started := time.Now()
	if _, errText := s.shell(*t, command); errText != "" {
		log.Printf("[Fire TV] YouTube %s on %s: %s", action, t.Name, errText)
		return false, ""
	}
	log.Printf("[Fire TV] YouTube %s on %s over ADB (%d ms)", action, t.Name, time.Since(started).Milliseconds())
	return true, t.Name
}

const youtubePackage = "com.amazon.firetv.youtube"

// resumeYouTube goes on with the video on a stick, also from standby, step by step until it
// plays: wake and the play key; else YouTube to the front and the play key again; else link (the
// video Jarvis started last, "" = unknown) anew at the position the stick had. Returns the
// stick's name, or an error text when nothing worked.
func (s *Service) resumeYouTube(t tv, link string) string {
	started := time.Now()
	before, _ := s.readState(t) // the paused video's position, before a wake can change it
	// plays: YouTube in front and its playback running ("playing: YouTube" behind the home screen
	// after a wake showed no picture, 18.10.2026)
	playing := func() bool {
		time.Sleep(1500 * time.Millisecond)
		st, errText := s.readState(t)
		return errText == "" && st.front == youtubePackage && st.playing == youtubePackage && !st.paused
	}
	// After a wake the home screen is in front and YouTube behind it (its playback state stale);
	// monkey did not bring it forward, its activity started by name does, and then it takes ~4 s.
	// Each step stays short: Home Assistant gives up on a command after 8 s.
	steps := []struct{ why, command string }{
		{"play key", "input keyevent 224; sleep 1; input keyevent 126"},
		{"YouTube to the front", "am start -n " + youtubePackage + "/dev.cobalt.app.MainActivity >/dev/null 2>&1"},
		{"play key in YouTube", "sleep 3; input keyevent 126"},
	}
	if link != "" {
		at := ""
		if before.playing == youtubePackage && before.position > 0 {
			at = fmt.Sprintf("&t=%ds", before.position/1000)
		}
		steps = append(steps, struct{ why, command string }{"the video anew",
			"am start -a android.intent.action.VIEW -d '" + link + at + "' -p " + youtubePackage + " >/dev/null 2>&1"})
	}
	for _, step := range steps {
		if _, errText := s.shell(t, step.command); errText != "" {
			// a stick waking from standby answers late: Home Assistant gave up after 8 s while
			// the stick did wake (18.10.2026). Look whether it worked instead of failing.
			log.Printf("[Fire TV] YouTube on %s: %s: %s", t.Name, step.why, errText)
			time.Sleep(2 * time.Second)
		}
		if step.why == "YouTube to the front" {
			continue // its stale "playing" would pass the check before the play key
		}
		if playing() {
			log.Printf("[Fire TV] YouTube plays on %s again (%s, was %s, %d ms)", t.Name, step.why, before.power,
				time.Since(started).Milliseconds())
			return t.Name
		}
		log.Printf("[Fire TV] YouTube on %s: %s did not start it", t.Name, step.why)
	}
	return "Error: the video does not play on " + t.Name + " (woken, play pressed, YouTube brought to the front" +
		map[bool]string{true: ", the last video started anew", false: ""}[link != ""] + "). Ask which video to play."
}

func (s *Service) run(args map[string]any, _ tools.Context) string {
	action, text := tools.String(args, "action"), tools.String(args, "text")
	tvs, errText := s.loadTvs()
	if errText != "" {
		return errText
	}
	if action == "status" {
		return s.status(tvs)
	}
	switch action {
	case "alexa", "open_app", "on", "off", "key", "screen":
	default:
		return "Error: action is status, alexa, open_app, on, off, key or screen"
	}
	t, errText := find(tvs, tools.String(args, "device"))
	if t == nil {
		return errText
	}
	if action == "on" || action == "off" {
		return s.power(*t, action == "on")
	}
	if action == "screen" {
		return s.screen(*t)
	}
	if text == "" {
		return "Error: text missing"
	}
	if action == "alexa" {
		return s.alexaCommand(*t, text)
	}
	if action == "open_app" {
		return s.openApp(*t, text)
	}
	repeat, ok := tools.Int(args, "repeat")
	if !ok {
		repeat = 1
	}
	return s.key(*t, text, repeat)
}

// AddTools registers the fire_tv tool (nothing without Home Assistant, as on the M5). It is slow
// (up to ~20 s); the Go registry has no Slow flag, every call runs in its own goroutine anyway.
func (s *Service) AddTools(r *tools.Registry) {
	if !s.ha.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "fire_tv",
		Description: "Controls the Amazon Fire TV sticks at home through Home Assistant. action status: every stick with " +
			"power, the app in front and what plays (device not needed). alexa: sends German text to the stick's own " +
			"Alexa as if the user had said it; the way to play a film or series by title. Always name the app ('Spiele " +
			"Stranger Things auf Netflix'): without one Alexa asks back on the TV and nobody can answer. If the user " +
			"names no app, ask, or take the one you are fairly sure has it. It presses OK by itself when the app " +
			"stops at a profile selection (takes the preselected profile) or a details page (play/continue), and says " +
			"whether it plays. It also switches a sleeping stick and its TV on: no status or on before it. 'Now " +
			"playing' means done: do not check again. If not playing: look with screen and go on with key. Say " +
			"'Moment' first, it takes up to 20 s. " +
			"screen: the texts and buttons on the screen, the focused one marked. key: press a remote key (text: " +
			"home, back, up, down, left, right, select, menu, play_pause, rewind, fast_forward; repeat for several " +
			"presses); with screen you can operate any app, e.g. another profile when the user asks for it. " +
			"open_app: start an app by name or package (text, e.g. " +
			"'Netflix'); an unclear name returns the installed apps. on/off: wake the stick and its TV, or put both " +
			"to sleep (HDMI-CEC). YouTube videos go through play_video instead. device: the stick's name or its " +
			"room; an unclear device returns the list.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"device": {"type": "string", "description": "e.g. 'Fire TV Wohnzimmer' or 'Wohnzimmer'; '' for status"},
                           "action": {"type": "string", "enum": ["status", "alexa", "screen", "key", "open_app", "on", "off"]},
                           "text": {"type": "string", "description": "alexa: the German command without 'Alexa'; open_app: the app; key: the key; else ''"},
                           "repeat": {"type": "integer", "description": "key: how often, 1-10"}},
            "required": ["device", "action", "text", "repeat"]}`,
		Channels: tools.Everywhere,
		Run:      s.run,
	})
}
