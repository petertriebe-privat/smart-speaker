package spotify

import (
	"log"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/tools"
)

// stringOr reads a string argument, def if it is missing or empty.
func stringOr(args map[string]any, name, def string) string {
	if v := tools.String(args, name); v != "" {
		return v
	}
	return def
}

func onOff(on bool) string {
	if on {
		return "true"
	}
	return "false"
}

// timeRange maps the model's period to Spotify's.
func timeRange(period string) string {
	switch period {
	case "4_weeks":
		return "short_term"
	case "all_time":
		return "long_term"
	}
	return "medium_term"
}

func notFound(missed string) string {
	if missed == "" {
		return ""
	}
	return ". Not found: " + missed
}

func (s *Service) playMusic(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	typ, what := stringOr(args, "type", "track"), tools.String(args, "what")
	queue := tools.Bool(args, "queue", false)

	var contextURI, trackInContext, described, missed string
	var uris []string
	switch typ {
	case "track", "episode", "album", "artist", "playlist", "show", "audiobook":
		if what == "" {
			return "Error: what to play is missing"
		}
		item, errText := s.searchFirst(sess, what, typ)
		if item == nil {
			return errText
		}
		described = describe(item)
		switch {
		case typ == "track" && str(item, "album", "uri") != "":
			// in its album, as the Spotify app does: "nächstes Lied" then goes on (a single
			// track left nothing to skip to and Spotify paused, 16.10.2026)
			contextURI, trackInContext = str(item, "album", "uri"), str(item, "uri")
		case typ == "track" || typ == "episode":
			uris = append(uris, str(item, "uri"))
		default:
			contextURI = str(item, "uri")
		}
	case "my_playlist":
		all, errText := s.loadPlaylists(sess)
		if errText != "" {
			return errText
		}
		pl, errText := findPlaylist(all, what, false)
		if pl == nil {
			return errText
		}
		contextURI = pl.uri
		described = "playlist '" + pl.name + "'"
	case "liked", "top":
		path := "/me/tracks?limit=50"
		if typ == "top" {
			path = "/me/top/tracks?limit=50&time_range=" + timeRange(tools.String(args, "period"))
		}
		doc, errText := s.getJSON(sess, path)
		if errText != "" {
			return errText
		}
		for _, entry := range list(doc, "items") {
			track := entry
			if typ == "liked" {
				track = get(entry, "track")
			}
			if track != nil {
				uris = append(uris, str(track, "uri"))
			}
		}
		kind := "top songs"
		if typ == "liked" {
			kind = "liked songs"
		}
		if len(uris) == 0 {
			return "Error: no " + kind + " yet."
		}
		described = "the " + kind + " (" + strconv.Itoa(len(uris)) + ")"
	case "tracks":
		wanted, _ := args["tracks"].([]any)
		uris, described, missed = s.resolveTracks(sess, wanted)
		if len(uris) == 0 {
			text := "Error: none of the songs found on Spotify"
			if missed != "" {
				text += ": " + missed
			}
			return text
		}
		described = strconv.Itoa(len(uris)) + " songs: " + described
	default:
		return "Error: unknown type"
	}

	devices, errText := s.loadDevices(sess)
	if errText != "" {
		return errText
	}
	wanted := s.localName(tools.String(args, "speaker"))
	devices = s.withLocal(sess, devices, wanted)
	dev, errText := s.target(devices, wanted)
	if dev == nil {
		return errText
	}
	on := "device_id=" + dev.id

	if queue && trackInContext != "" {
		uris, contextURI = []string{trackInContext}, ""
	}
	if queue {
		if len(uris) == 0 {
			return "Error: only songs and episodes can be queued, not a whole " + typ
		}
		for _, uri := range uris {
			if errText := s.do(sess, "POST", "/me/player/queue?uri="+urlEncode(uri)+"&"+on, nil); errText != "" {
				return errText
			}
		}
		return "Queued on " + dev.name + ": " + described + sess.suffix() + notFound(missed)
	}

	// > 0: the strict schema makes the model send "volume": 0 for "no change" (that silenced the
	// Echo in the living room on 16.10.2026)
	if volume, ok := tools.Int(args, "volume"); ok && volume > 0 && dev.volumeOk {
		s.do(sess, "PUT", "/me/player/volume?volume_percent="+strconv.Itoa(clamp(volume, 0, 100))+"&"+on, nil)
		s.forgetDuck(dev.id)
	}
	body := map[string]any{}
	if contextURI != "" {
		body["context_uri"] = contextURI
		if trackInContext != "" {
			body["offset"] = map[string]any{"uri": trackInContext}
		}
	} else {
		body["uris"] = uris
	}
	payload := marshal(body)
	status, _, errText := s.call(sess, "PUT", "/me/player/play?"+on, payload)
	if status == 404 || status == 502 { // an idle device: wake it with a transfer, then again
		s.do(sess, "PUT", "/me/player", marshal(map[string]any{"device_ids": []string{dev.id}, "play": false}))
		time.Sleep(s.settle)
		status, _, errText = s.call(sess, "PUT", "/me/player/play?"+on, payload)
	}
	if status < 200 || status >= 300 {
		return errText
	}
	if errText := s.phonePlays(sess, dev); errText != "" {
		return errText
	}
	var extra string
	if shuffle, ok := args["shuffle"].(bool); ok {
		if failed := s.do(sess, "PUT", "/me/player/shuffle?state="+onOff(shuffle)+"&"+on, nil); failed != "" {
			extra = " (shuffle not set: " + failed + ")"
		} else if shuffle {
			extra = " in shuffle"
		}
	}
	return "Playing on " + dev.name + ": " + described + extra + sess.suffix() + notFound(missed)
}

// --- music_control ----------------------------------------------------------------------

// state tells what plays where ("Nothing is playing" when Spotify knows no playback).
func (s *Service) state(sess session) string {
	doc, status, errText := s.callJSON(sess, "GET", "/me/player?additional_types=episode", nil)
	if errText != "" {
		return errText
	}
	item := get(doc, "item")
	if status == 204 || item == nil {
		return "Nothing is playing" + sess.suffix() + "."
	}
	text := "Paused: "
	if flag(doc, "is_playing") {
		text = "Playing: "
	}
	text += describe(item)
	if !isNull(item, "album") {
		text += ", album '" + str(item, "album", "name") + "'"
	}
	text += ", " + clock(int64(intOr(doc, 0, "progress_ms"))) + " of " + clock(int64(intOr(item, 0, "duration_ms")))
	device := orDefault(str(doc, "device", "name"), "?")
	text += ", on " + device + " (" + str(doc, "device", "type")
	if volume, ok := num(doc, "device", "volume_percent"); ok {
		text += ", volume " + strconv.Itoa(int(volume)) + "%"
	}
	text += ")"
	shuffle, repeat := "off", orDefault(str(doc, "repeat_state"), "off")
	if flag(doc, "shuffle_state") {
		shuffle = "on"
	}
	text += ", shuffle " + shuffle + ", repeat " + repeat
	contextType, contextURI := str(doc, "context", "type"), str(doc, "context", "uri")
	if contextType == "playlist" {
		id := contextURI[strings.LastIndex(contextURI, ":")+1:]
		if pl, errText := s.getJSON(sess, "/playlists/"+id+"?fields=name"); errText == "" {
			text += ", from playlist '" + str(pl, "name") + "'"
		}
	} else if contextType != "" {
		text += ", from the " + contextType
	}
	if link := str(item, "external_urls", "spotify"); link != "" {
		text += ". Link: " + link
	}
	return text + sess.suffix()
}

// Restart plays what was heard last on speaker anew from where it stopped: the track in its
// album or playlist, else the track alone, else the latest of recently played. For a speaker
// that took "resume" without playing: librespot after a restart has lost a playback that was a
// list of tracks ("context is not available", 18.10.2026) while Spotify reports it playing.
func (s *Service) Restart(person, speaker string) string {
	sess, errText := s.session(person, "")
	if errText != "" {
		return errText
	}
	query, name, errText := s.deviceQuery(sess, speaker)
	if errText != "" {
		return errText
	}
	doc, _, _ := s.callJSON(sess, "GET", "/me/player", nil)
	track, position := str(doc, "item", "uri"), intOr(doc, 0, "progress_ms")
	contextURI := str(doc, "context", "uri")
	if track == "" {
		recent, errText := s.getJSON(sess, "/me/player/recently-played?limit=1")
		if errText != "" {
			return errText
		}
		items := list(recent, "items")
		if len(items) == 0 {
			return "Error: nothing was played lately."
		}
		track, position, contextURI = str(items[0], "track", "uri"), 0, str(items[0], "context", "uri")
	}
	if track == "" {
		return "Error: nothing was played lately."
	}
	body := map[string]any{"position_ms": position}
	// an offset only in an album or a playlist: an artist's context answered "HTTP 400: Can't have
	// offset for context type: ARTIST" (21.10.2026); elsewhere the track alone, from its position
	if strings.Contains(contextURI, ":album:") || strings.Contains(contextURI, ":playlist:") {
		body["context_uri"] = contextURI
		body["offset"] = map[string]any{"uri": track}
	} else {
		body["uris"] = []string{track}
	}
	if errText := s.do(sess, "PUT", withQuery("/me/player/play", query), marshal(body)); errText != "" {
		return errText
	}
	return "Playing on " + name + " again, from where it stopped" + sess.suffix() + "."
}

func (s *Service) musicControl(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	action := stringOr(args, "action", "status")
	query, deviceName, errText := s.deviceQuery(sess, tools.String(args, "speaker"))
	if errText != "" {
		return errText
	}

	switch action {
	case "status":
		return s.state(sess)
	case "queue":
		doc, errText := s.getJSON(sess, "/me/player/queue")
		if errText != "" {
			return errText
		}
		if isNull(doc, "currently_playing") {
			return "Nothing is playing" + sess.suffix() + "."
		}
		text := "Now: " + describe(get(doc, "currently_playing")) + ". Next:"
		shown := 0
		for _, item := range list(doc, "queue") {
			if shown++; shown > 10 {
				shown--
				break
			}
			text += "\n" + strconv.Itoa(shown) + ". " + describe(item)
		}
		if shown == 0 {
			return text + " nothing queued"
		}
		return text
	case "pause", "resume":
		path := "/me/player/play"
		if action == "pause" {
			path = "/me/player/pause"
		}
		status, _, errText := s.call(sess, "PUT", withQuery(path, query), nil)
		if action == "resume" && (status == 404 || status == 502) && strings.HasPrefix(query, "device_id=") {
			// an idle speaker: the account's last playback moves to it and goes on - what was
			// heard last ("spiel meine Spotify-Musik" started a random song, 18.10.2026)
			errText = s.do(sess, "PUT", "/me/player", marshal(map[string]any{
				"device_ids": []string{strings.TrimPrefix(query, "device_id=")}, "play": true}))
		}
		if errText != "" {
			return errText
		}
		if action == "pause" {
			return "Paused" + sess.suffix()
		}
		return "Playing again" + sess.suffix()
	case "next", "previous":
		times := 1
		if action == "previous" {
			// A speaker goes back to the start of the song once it played more than ~3 s; "der
			// vorherige Titel" means the one before (16.10.2026: it took two "zurück").
			if doc, _, errText := s.callJSON(sess, "GET", "/me/player", nil); errText == "" && intOr(doc, 0, "progress_ms") > 3000 {
				times = 2
			}
		}
		for i := 0; i < times; i++ {
			if i > 0 {
				time.Sleep(400 * time.Millisecond)
			}
			if errText := s.do(sess, "POST", withQuery("/me/player/"+action, query), nil); errText != "" {
				return errText
			}
		}
		time.Sleep(s.settle)
		return s.state(sess)
	case "shuffle":
		on := tools.Bool(args, "on", true)
		if errText := s.do(sess, "PUT", withQuery("/me/player/shuffle?state="+onOff(on), query), nil); errText != "" {
			return errText
		}
		if on {
			return "Shuffle on"
		}
		return "Shuffle off"
	case "repeat":
		mode := stringOr(args, "mode", "context")
		if mode != "off" && mode != "track" && mode != "context" {
			return "Error: mode is off, track or context"
		}
		if errText := s.do(sess, "PUT", withQuery("/me/player/repeat?state="+mode, query), nil); errText != "" {
			return errText
		}
		return "Repeat " + mode
	case "seek", "volume":
	default:
		return "Error: unknown action"
	}

	doc, status, errText := s.callJSON(sess, "GET", "/me/player", nil)
	if errText != "" {
		return errText
	}
	if status == 204 || isNull(doc, "device") {
		return "Error: nothing is playing" + sess.suffix() + "."
	}
	if action == "seek" {
		seconds, _ := tools.Int(args, "seconds")
		to := int64(intOr(doc, 0, "progress_ms")) + int64(seconds)*1000
		if to < 0 {
			to = 0
		}
		if errText := s.do(sess, "PUT", withQuery("/me/player/seek?position_ms="+strconv.FormatInt(int64(int32(to)), 10), query), nil); errText != "" {
			return errText
		}
		return "Now at " + clock(to)
	}
	id := str(doc, "device", "id")
	if query != "" {
		id = strings.TrimPrefix(query, "device_id=")
	}
	if query == "" && !flag(doc, "device", "supports_volume") {
		return "Error: " + orDefault(str(doc, "device", "name"), "this speaker") + " does not let Spotify set its volume."
	}
	// A step wins over a level: the model sends both ("level": 0, "step": 10 for "lauter" set
	// the volume to 0, 16.10.2026).
	level, _ := tools.Int(args, "level")
	if step, _ := tools.Int(args, "step"); step != 0 {
		base := intOr(doc, 50, "device", "volume_percent")
		if s.LocalVolume != nil && s.LocalSpeaker != "" && lower(str(doc, "device", "name")) == lower(s.LocalSpeaker) {
			base = s.LocalVolume() // Spotify's number for the Echo is not its real volume
		}
		level = base + step
	}
	level = clamp(level, 0, 100)
	errText = s.do(sess, "PUT", "/me/player/volume?volume_percent="+strconv.Itoa(level)+"&device_id="+id, nil)
	s.forgetDuck(id)
	if errText != "" {
		return errText
	}
	name := deviceName
	if name == "" {
		name = str(doc, "device", "name")
	}
	return "Volume " + strconv.Itoa(level) + "% on " + name
}

// --- music_speakers ---------------------------------------------------------------------

// snapshot: what plays now as a play request: the song from its second, in its album or
// playlist, else with the queue after it; nil if nothing plays.
func (s *Service) snapshot(sess session) map[string]any {
	doc, _, _ := s.callJSON(sess, "GET", "/me/player", nil)
	track := str(doc, "item", "uri")
	if track == "" {
		return nil
	}
	body := map[string]any{"position_ms": intOr(doc, 0, "progress_ms")}
	if context := str(doc, "context", "uri"); context != "" && !strings.Contains(context, ":collection") {
		body["context_uri"], body["offset"] = context, map[string]any{"uri": track}
		return body
	}
	uris := []string{track}
	if queue, errText := s.getJSON(sess, "/me/player/queue"); errText == "" {
		for _, item := range list(queue, "queue") {
			if uri := str(item, "uri"); uri != "" && uri != track && len(uris) < 50 {
				uris = append(uris, uri)
			}
		}
	}
	body["uris"] = uris
	return body
}

// phonePlays: a phone took the command and plays within a few seconds; else an error, with the
// phone's hint when there is one. Spotify answered "playing" for a phone whose app had no network
// (the battery saver, 06.11.2026) and the model told the user for minutes that it played.
func (s *Service) phonePlays(sess session, dev *device) string {
	if dev.typ != "Smartphone" {
		return ""
	}
	for i := 0; i < 8; i++ {
		time.Sleep(500 * time.Millisecond)
		state, errText := s.getJSON(sess, "/me/player")
		if errText == "" && flag(state, "is_playing") && str(state, "device", "id") == dev.id {
			return ""
		}
	}
	text := "Error: Spotify took the command, but " + dev.name + " does not play."
	if s.PhoneHint != nil {
		if hint := s.PhoneHint(); hint != "" && !strings.HasPrefix(hint, "Error") {
			return text + " " + hint
		}
	}
	return text + " Ask the user to open Spotify on it and look whether it is online."
}

// waitLocal: The assistant's own speaker plays within 3 s.
func (s *Service) waitLocal() bool {
	for i := 0; i < 30; i++ {
		if s.LocalPlaying() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (s *Service) musicSpeakers(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	action := stringOr(args, "action", "list")
	devices, errText := s.loadDevices(sess)
	if errText != "" {
		return errText
	}
	if action == "list" {
		text := "Speakers Spotify sees" + sess.suffix() + ":"
		for _, d := range devices {
			text += "\n- " + d.name + " (" + d.typ
			if d.active {
				text += ", playing"
			}
			if d.volume >= 0 {
				text += ", volume " + strconv.Itoa(d.volume) + "%"
			}
			if d.restricted {
				text += ", not controllable"
			}
			text += ")"
		}
		if len(devices) == 0 {
			text += " none."
		}
		return text + notVisible
	}
	wanted := s.localName(tools.String(args, "speaker"))
	if wanted != "" {
		devices = s.withLocal(sess, devices, wanted)
	}
	dev, errText := findDevice(devices, wanted)
	if dev == nil {
		return errText
	}
	switch action {
	case "move":
		local := s.LocalPlaying != nil && s.isLocal(dev.name)
		var before map[string]any
		if local {
			before = s.snapshot(sess) // what to start anew if the move leaves the Echo silent
		}
		errText := s.do(sess, "PUT", "/me/player", marshal(map[string]any{"device_ids": []string{dev.id}, "play": true}))
		if strings.Contains(errText, "404") {
			return "Error: nothing to move; start something with play_music."
		}
		if errText != "" {
			return errText
		}
		if local && !s.waitLocal() {
			// librespot took a playback without album or playlist (the liked songs as a list)
			// and stopped at once ("context is not available"), Spotify reported it moved
			// (18.10.2026): the same song from the same second, the queue after it
			if before == nil {
				return "Error: moved to " + dev.name + ", but it stays silent and nothing to start again is known."
			}
			if errText := s.do(sess, "PUT", "/me/player/play?device_id="+dev.id, marshal(before)); errText != "" {
				return errText
			}
			if !s.waitLocal() {
				return "Error: moved to " + dev.name + " and started anew, but it stays silent."
			}
			log.Printf("[Spotify] moved to %s: silent, started anew from the same second", dev.name)
		}
		return "Playback moved to " + dev.name
	case "volume":
		level, ok := tools.Int(args, "level")
		if !ok {
			return "Error: level missing"
		}
		if !dev.volumeOk {
			return "Error: " + dev.name + " does not let Spotify set its volume."
		}
		level = clamp(level, 0, 100)
		errText := s.do(sess, "PUT", "/me/player/volume?volume_percent="+strconv.Itoa(level)+"&device_id="+dev.id, nil)
		s.forgetDuck(dev.id)
		if errText != "" {
			return errText
		}
		return "Volume " + strconv.Itoa(level) + "% on " + dev.name
	}
	return "Error: action is list, move or volume"
}
