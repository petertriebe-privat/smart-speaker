package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Quick music commands, as Alexa does them: "Hey Jarvis, Pause" should take a moment, not the
// round trip through GPT Live and the delegation model. When the user's whole first utterance
// is one of these, the device runs it and ends the conversation at once; anything else (or a
// failing command) stays with GPT Live.

type musicCommand struct {
	action string // pause, resume, next, previous, volume, liked, artist, play, home (home.go); "weiter" is resume-or-next
	step   int    // volume: up or down
	level  int    // volume: set to this percent (> 0)
	what   string // artist: the name
	where  string // a speaker or room the user named ("im Wohnzimmer"); "" = here / where it plays
}

// filler words that do not change the command ("Hey Jarvis, bitte mal etwas leiser").
var quickFiller = map[string]bool{"hey": true, "bitte": true, "mal": true, "doch": true,
	"jetzt": true, "etwas": true, "noch": true, "ein": true, "bisschen": true, "bißchen": true, "danke": true,
	"die": true, "der": true, "das": true, "dem": true, "den": true, "mach": true, "machen": true, "okay": true, "ok": true,
	// The assistant's own "Ja, Sir?" lands in GPT Live's transcript now and then (18.10.2026)
	"ja": true, "sir": true, "mir": true, "uns": true, "einfach": true}

// what a command acts on: "Pausiere die Musik", "Lied überspringen" (GPT Live's transcript of
// "Pausiere die Musik" missed the quick path, 18.10.2026). Dropped when the words alone match
// nothing.
var videoWords = map[string]bool{"video": true, "youtube": true, "youtubevideo": true, "film": true,
	"fernseher": true, "fernsehen": true, "tv": true}

var quickObjects = map[string]bool{"musik": true, "wiedergabe": true, "spotify": true, "lied": true,
	"song": true, "titel": true, "track": true}

var quickPhrases = map[string]musicCommand{
	"pause": {action: "pause"}, "pausieren": {action: "pause"}, "pausiere": {action: "pause"},
	"pausier": {action: "pause"}, "stoppe": {action: "pause"}, "stopp": {action: "pause"}, "stop": {action: "pause"},
	"stoppen": {action: "pause"}, "halt": {action: "pause"}, "aus": {action: "pause"}, "ausmachen": {action: "pause"},
	"anhalten": {action: "pause"}, "musik aus": {action: "pause"}, "musik stopp": {action: "pause"},
	"musik stop": {action: "pause"}, "musik pause": {action: "pause"},
	"weiter": {action: "weiter"}, "weiterspielen": {action: "resume"}, "spiel weiter": {action: "resume"},
	"weiter spielen": {action: "resume"}, "fortsetzen": {action: "resume"}, "play": {action: "resume"},
	"musik an": {action: "resume"}, "musik weiter": {action: "resume"},
	"nächstes": {action: "next"}, "nächster": {action: "next"}, "nächstes lied": {action: "next"},
	"nächster song": {action: "next"}, "nächster titel": {action: "next"}, "nächstes stück": {action: "next"},
	"überspringen": {action: "next"}, "skip": {action: "next"}, "next": {action: "next"},
	"zurück": {action: "previous"}, "vorheriges": {action: "previous"}, "vorheriges lied": {action: "previous"},
	"vorheriger song": {action: "previous"}, "vorheriger titel": {action: "previous"},
	"letztes lied": {action: "previous"}, "eins zurück": {action: "previous"}, "lied zurück": {action: "previous"},
	"spiel meine lieblingsmusik": {action: "liked"}, "spiel meine lieblingssongs": {action: "liked"},
	"spiel meine lieblingslieder": {action: "liked"}, "spiel meine lieblingssong": {action: "liked"},
	"meine lieblingsmusik": {action: "liked"}, "meine lieblingssongs": {action: "liked"},
	// music without saying what: on with what was heard last, here (the user's wish, 18.10.2026)
	"spiel meine musik": {action: "continue"}, "spiel meine spotify musik": {action: "continue"},
	"spiel meine spotify": {action: "continue"}, "spiel meine spotifymusik": {action: "continue"},
	"spiel spotify":    {action: "continue"},
	"hol musik zurück": {action: "move_here"}, "hol musik zu dir zurück": {action: "move_here"},
	"hol musik hierher": {action: "move_here"}, "musik hierher": {action: "move_here"},
	"spiel hier weiter": {action: "move_here"}, "spiel spotify musik": {action: "continue"},
	"spiel musik": {action: "continue"}, "spiel spotifymusik": {action: "continue"},
	"lauter": {action: "volume", step: 10}, "viel lauter": {action: "volume", step: 25},
	"leiser": {action: "volume", step: -10}, "viel leiser": {action: "volume", step: -25},
	// what the transcription made of them in tests
	"lizer": {action: "volume", step: -10}, "leise": {action: "volume", step: -10},
	"laiser": {action: "volume", step: -10}, "lauta": {action: "volume", step: 10},
}

// foldAccents: accents the transcription invents ("lízer") go; German umlauts stay.
func foldAccents(s string) string {
	return strings.NewReplacer("á", "a", "à", "a", "â", "a", "é", "e", "è", "e", "ê", "e", "í", "i", "ì", "i",
		"î", "i", "ó", "o", "ò", "o", "ô", "o", "ú", "u", "ù", "u", "û", "u").Replace(s)
}

// parseMusicCommand: the command the whole utterance is, or action "".
func parseMusicCommand(text string) musicCommand {
	words := strings.FieldsFunc(strings.ToLower(foldAccents(text)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	kept := words[:0]
	for _, w := range words {
		if w == "spiele" || w == "spielt" || w == "spielen" || w == "spiel'" {
			w = "spiel"
		}
		if !isFiller(w) {
			kept = append(kept, w)
		}
	}
	// "… im Wohnzimmer", "… auf dem Echo Küche": the place goes to the speaker argument.
	where := ""
	for j := len(kept) - 2; j >= 1; j-- {
		if kept[j] == "im" || kept[j] == "in" || kept[j] == "auf" {
			where, kept = strings.Join(withoutVerbs(kept[j+1:]), " "), kept[:j]
			break
		}
	}
	if where == "spotify" || where == "youtube" { // "auf Spotify" is the service, not a speaker
		where = ""
	}
	cmd := parseKept(kept)
	if cmd.action != "" {
		cmd.where = where
	}
	return cmd
}

func parseKept(kept []string) musicCommand {
	// "YouTube-Video pausieren", "das Video weiter": the same words for the video on the TV
	// (GPT Live announced "ich pausiere es" three times without doing it, 18.10.2026)
	var rest []string
	for _, w := range kept {
		if !videoWords[w] {
			rest = append(rest, w)
		}
	}
	if len(rest) < len(kept) && len(rest) > 0 {
		joined := strings.Join(rest, " ")
		if joined == "aus" || joined == "ausmachen" { // "Fernseher aus" is not a pause
			return musicCommand{}
		}
		switch quickPhrases[joined].action {
		case "pause":
			return musicCommand{action: "video_pause"}
		case "resume", "weiter":
			return musicCommand{action: "video_play"}
		}
		return musicCommand{}
	}
	if cmd, ok := quickPhrases[strings.Join(kept, " ")]; ok {
		return cmd
	}
	var bare []string
	for _, w := range kept {
		if !quickObjects[w] {
			bare = append(bare, w)
		}
	}
	if len(bare) > 0 && len(bare) < len(kept) {
		if cmd, ok := quickPhrases[strings.Join(bare, " ")]; ok {
			return cmd
		}
	}
	// "Spiel was von Deichkind": the artist, straight to Spotify. Any other play request ("Spiel
	// das Album …") goes to the backend model, which picks the search (GPT Live once said "ich
	// starte Deichkind" and never delegated, 16.10.2026). "spiel" may come second or third: the
	// transcription now and then makes "Servis" of "Hey Jarvis".
	for i := 0; i < min(3, len(kept)); i++ {
		if kept[i] != "spiel" {
			continue
		}
		rest := kept[i+1:]
		if len(rest) > 0 && (rest[0] == "was" || rest[0] == "musik" || rest[0] == "lieder" || rest[0] == "songs") {
			rest = rest[1:]
		}
		if len(rest) >= 2 && rest[0] == "von" {
			return musicCommand{action: "artist", what: strings.Join(rest[1:], " ")}
		}
		if len(kept) > i+1 {
			return musicCommand{action: "play"}
		}
	}
	// "Musik von Eminem" without "spiel" (the transcription heard "Die Musik von Eminem").
	if len(kept) >= 3 && (kept[0] == "musik" || kept[0] == "lieder" || kept[0] == "songs") && kept[1] == "von" {
		return musicCommand{action: "artist", what: strings.Join(kept[2:], " ")}
	}
	return musicCommand{}
}

// complete: the sentence is over: a full stop at the end, or the transcript settled; never when
// it trails off ("spiele Musik von...") or ends on a word that wants more ("von", "im").
func complete(text string, settled bool) bool {
	t := strings.TrimSpace(text)
	if strings.HasSuffix(t, "...") || strings.HasSuffix(t, "…") {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) > 0 {
		switch words[len(words)-1] {
		case "von", "im", "in", "auf", "mit", "und", "spiel", "spiele", "spielen":
			return false
		}
	}
	return settled || strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") || strings.HasSuffix(t, "?")
}

// playRequest: "Spiel …" straight to the backend model with the tools, without GPT Live. Music
// starting is the answer; a low tone if it found nothing to play.
func (d *device) playRequest(text string) string {
	started := time.Now()
	task := fmt.Sprintf("Der Nutzer hat gerade am Lautsprecher gesagt: „%s“. Spiele das sofort mit play_music "+
		"(Interpret, Album, Playlist oder Titel, wie es gemeint ist). Ist kein Lautsprecher genannt, spiele es hier "+
		"auf %q. Frage nichts zurück. Ist nicht klar, was gemeint ist (kein erkennbarer Interpret, Titel, Album, "+
		"Playlist oder Genre; die Abschrift kann verhört sein), setze fort, was zuletzt lief: music_control resume "+
		"auf %q. Nur wenn beides scheitert, antworte mit „Error:“ und dem Grund.",
		strings.TrimSpace(text), d.cfg.Settings.SpotifyName, d.cfg.Settings.SpotifyName)
	out := d.svc.agent.Run(task, agent.Options{Instructions: prompts.Text("TASK_INSTRUCTIONS"),
		Person: d.session.Person(), Origin: "Lautsprecher", Effort: "low"})
	log.Printf("[Quick] play %q: %.200s (%d ms)", text, out, time.Since(started).Milliseconds())
	return strings.TrimSpace(out)
}

// Quick commands for a call on the mobile phone (#51): "Jarvis, geh ran" while it rings must
// put the call on the speaker at once, not after a round trip through GPT Live - the caller is
// waiting. Only while the phone rings or a call on it runs.
var mobilePhrases = map[string]string{
	"geh ran": "answer", "geh dran": "answer", "ran": "answer", "rangehen": "answer", "rangehen bitte": "answer",
	"nimm ab": "answer", "abnehmen": "answer", "annehmen": "answer", "nimm an": "answer", "nimm anruf an": "answer",
	"anruf annehmen": "answer", "heb ab": "answer", "abheben": "answer",
	"lehne ab": "reject", "ablehnen": "reject", "anruf ablehnen": "reject", "drück weg": "reject",
	"drück ihn weg": "reject", "wegdrücken": "reject", "nicht rangehen": "reject", "geh nicht ran": "reject",
	"leg auf": "hangup", "auflegen": "hangup", "beenden": "hangup", "verbindung beenden": "hangup",
	"gespräch beenden": "hangup", "anruf beenden": "hangup",
}

// parseMobileCommand: answer, reject or hangup if the whole utterance is one, else "".
func parseMobileCommand(text string) string {
	words := strings.FieldsFunc(strings.ToLower(foldAccents(text)), func(r rune) bool { return !unicode.IsLetter(r) })
	kept := words[:0]
	for _, w := range words {
		if !isFiller(w) {
			kept = append(kept, w)
		}
	}
	return mobilePhrases[strings.Join(kept, " ")]
}

// quickMobile: a quick command for the phone's call; true if it was one.
func (d *device) quickMobile(text string) bool {
	p := d.svc.phone
	if p == nil || !p.MobileRinging() && !p.MobileActive() {
		return false
	}
	action := parseMobileCommand(text)
	if action == "" {
		return false
	}
	if !d.claimQuick() {
		return true
	}
	ctx := tools.Context{Channel: tools.Voice, Person: d.session.Person(), Origin: "Lautsprecher"}
	go func() {
		out := d.registry.Call("mobile_call", `{"action": "`+action+`"}`, ctx)
		log.Printf("[Quick] mobile %s: %s", action, out)
		if strings.HasPrefix(out, "Error") {
			d.speaker.Tone(220, 250, 0.15)
		}
	}()
	return true
}

// quickMusic: a quick command by the rules (phone call, music or the house) run at once; false =
// none (yet).
func (d *device) quickMusic(text string, settled bool) bool {
	if d.quickMobile(text) {
		return true
	}
	if d.quickTimer(text, settled) { // timers.go
		return true
	}
	cmd := parseMusicCommand(text)
	if cmd.action == "" && homeCandidate(text) && complete(text, settled) {
		cmd.action = "home" // HA's own rules decide (home.go)
	}
	if cmd.action == "" {
		return false
	}
	if (cmd.action == "play" || cmd.action == "artist") && !complete(text, settled) {
		return false // GPT Live's transcript still growing: wait for the whole sentence
	}
	d.runMusic(cmd, text)
	return true
}

// runMusic runs a quick command (once per conversation, whichever path is first). The
// conversation ends now and the command runs behind it (Spotify's Web API takes ~1 s); a low tone
// if it fails (nothing playing anywhere, say).
func (d *device) runMusic(cmd musicCommand, text string) {
	if !d.claimQuick() {
		return
	}
	if d.music != nil {
		// silent while the command is heard (#59): "Pause" keeps it so until Spotify paused it
		if cmd.action == "pause" && cmd.where == "" {
			d.music.HushFor(3 * time.Second)
		} else {
			d.music.HushFor(0)
		}
	}
	// The device keeps listening while the command runs. When it failed GPT Live says why - a low
	// tone alone told the user nothing ("YouTube runs on no TV", 18.10.2026).
	go func() {
		out := d.doMedia(cmd, text)
		if !strings.HasPrefix(out, "Error") {
			d.endQuick(text)
			if cmd.action == "home" {
				d.speaker.Tone(880, 80, 0.12) // the light may be in another room: done is heard
			}
			return
		}
		if strings.HasPrefix(out, homeUnknown) {
			// HA did not understand: GPT Live hears the words as if there had been no quick path
			// ("Kühlschrank, Licht aus", 22.10.2026)
			d.connectLive(text, "Home Assistant did not understand")
			return
		}
		d.connectWithNote(text, "System note, not from the user: the user said \""+text+"\". The device tried to do "+
			"it itself ("+cmd.action+") and it failed: "+out+" Tell the user in one short sentence what went wrong "+
			"and help if you can. Do not say you did it.")
	}()
}

// doMedia runs a music or video command and returns the result.
func (d *device) doMedia(cmd musicCommand, text string) string {
	if cmd.action == "play" {
		return d.playRequest(text)
	}
	if cmd.action == "home" {
		return d.homeCommand(text)
	}
	local := d.music != nil && d.music.Playing() && cmd.where == "" // ducked, but still playing here
	if cmd.action == "volume" && local {
		// the device volume is the music's: at once, and Spotify's slider follows
		d.volMu.Lock()
		if cmd.level > 0 {
			d.volume = max(minVolume, min(100, cmd.level))
		} else {
			d.volume = max(minVolume, min(100, d.volume+cmd.step))
		}
		v := d.volume
		d.volMu.Unlock()
		d.kickVolume()
		return fmt.Sprintf("Volume here now %d %%.", v)
	}
	tool, a := "music_control", map[string]any{"action": cmd.action, "speaker": cmd.where}
	switch cmd.action {
	case "weiter": // playing -> the next song; paused -> go on
		a["action"] = "resume"
		if local {
			a["action"] = "next"
		}
	case "volume":
		if cmd.level > 0 {
			a["level"] = cmd.level
		} else {
			a["step"] = cmd.step
		}
	case "liked": // the liked songs; without a place here
		tool, a = "play_music", map[string]any{"what": "", "type": "liked", "speaker": cmd.where}
	case "continue": // what was heard last, moved here when this speaker is idle
		a["action"] = "resume"
		if cmd.where == "" {
			a["speaker"] = d.cfg.Settings.SpotifyName
		}
	case "artist":
		tool, a = "play_music", map[string]any{"what": cmd.what, "type": "artist", "speaker": cmd.where}
	case "video_pause":
		tool, a = "control_video", map[string]any{"tv": cmd.where, "action": "pause"}
	case "video_play":
		tool, a = "control_video", map[string]any{"tv": cmd.where, "action": "play"}
	case "move": // to the speaker named
		if cmd.where == "" {
			return "Error: the user did not say to which speaker; ask."
		}
		tool, a = "music_speakers", map[string]any{"account": "", "action": "move", "level": 0, "speaker": cmd.where}
	case "move_here":
		tool, a = "music_speakers", map[string]any{"account": "", "action": "move", "level": 0,
			"speaker": d.cfg.Settings.SpotifyName}
	}
	raw, _ := json.Marshal(a)
	ctx := tools.Context{Channel: tools.Voice, Person: d.session.Person(), Origin: "Lautsprecher"}
	started := time.Now()
	out := d.registry.Call(tool, string(raw), ctx)
	log.Printf("[Quick] %s %s -> %.160s (%d ms)", tool, raw, out, time.Since(started).Milliseconds())
	if (cmd.action == "pause" || cmd.action == "video_pause") && strings.Contains(out, "No active device") {
		return "Nothing was playing." // silent is what the user wanted: no explanation needed
	}
	if cmd.action == "continue" && cmd.where == "" && !strings.HasPrefix(out, "Error") && d.music != nil {
		// Spotify said "Playing again" while the Echo stayed silent: its player had lost the
		// playback in a restart (18.10.2026). Silent after 2 s: the last track anew, from there.
		for i := 0; i < 20 && !d.music.Playing(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if !d.music.Playing() && d.svc.spotify != nil {
			out = d.svc.spotify.Restart(d.session.Person(), d.cfg.Settings.SpotifyName)
			log.Printf("[Quick] resumed but silent here, started anew: %.160s", out)
		}
	}
	return out
}

// isFiller: a filler word or the assistant's name.
func isFiller(w string) bool { return quickFiller[w] || prompts.IsName(w) }
