package spotify

import (
	"echo-assistant/internal/prompts"
	"log"
	"slices"
	"strings"
	"time"
)

type device struct {
	id, name, typ                string
	active, restricted, volumeOk bool
	volume                       int // -1 unknown
}

func (s *Service) loadDevices(sess session) ([]device, string) {
	doc, errText := s.getJSON(sess, "/me/player/devices")
	if errText != "" {
		return nil, errText
	}
	var out []device
	for _, d := range list(doc, "devices") {
		dev := device{
			id: str(d, "id"), name: str(d, "name"), typ: str(d, "type"),
			active: flag(d, "is_active"), restricted: flag(d, "is_restricted"), volumeOk: flag(d, "supports_volume"),
			volume: intOr(d, -1, "volume_percent"),
		}
		if dev.id != "" {
			out = append(out, dev)
		}
	}
	return out, ""
}

func deviceNames(devices []device) string {
	var parts []string
	for _, d := range devices {
		parts = append(parts, d.name+" ("+d.typ+")")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

var phoneWords = []string{"handy", "smartphone", "phone", "telefon", "mobile"}

func isPhoneWord(word string) bool { return slices.Contains(phoneWords, word) }

// wordsIn: whether every word of key (3 letters or more, a German genitive s dropped) is in name.
// A number counts at any length and only as a whole word: "Jarvis 2" is not "Jarvis" (two Echos,
// 18.10.2026: music for the second one went to the first).
func wordsIn(name, key string, skipPhoneWords bool) bool {
	words := 0
	for _, word := range strings.Split(key, " ") {
		if isNumber(word) {
			if !slices.Contains(strings.Fields(name), word) {
				return false
			}
			continue
		}
		if len(word) < 3 || (skipPhoneWords && isPhoneWord(word)) {
			continue
		}
		words++
		if strings.Contains(name, word) {
			continue
		}
		if strings.HasSuffix(word, "s") && strings.Contains(name, word[:len(word)-1]) {
			continue
		}
		return false
	}
	return words > 0
}

func isNumber(word string) bool {
	return word != "" && strings.Trim(word, "0123456789") == ""
}

// ownDevice: The assistant's own speaker by its exact name; nil while Spotify does not list it. Never a
// similar name: that is another Echo ("Jarvis" and "Jarvis 2").
func (s *Service) ownDevice(devices []device) *device {
	for i := range devices {
		if s.LocalSpeaker != "" && lower(devices[i].name) == lower(s.LocalSpeaker) {
			return &devices[i]
		}
	}
	return nil
}

// findDevice returns the device meant by wanted: its exact name, else the only match (by name or
// words; "Handy" means the smartphones, an "Echo ..." wins over others). Nil with the error
// text otherwise.
func findDevice(devices []device, wanted string) (*device, string) {
	key := lower(wanted)
	for i := range devices {
		if lower(devices[i].name) == key {
			return &devices[i], ""
		}
	}
	phone := false
	for _, word := range phoneWords {
		if strings.Contains(key, word) {
			phone = true
		}
	}
	var hits, echoHits []*device
	for i := range devices {
		d := &devices[i]
		name := lower(d.name)
		var hit bool
		if phone {
			hit = d.typ == "Smartphone" && (wordsIn(name, key, true) || !wordsIn(key, key, true))
		} else {
			hit = (strings.Contains(name, key) || wordsIn(name, key, false)) && sameNumbers(name, key)
		}
		if !hit {
			continue
		}
		hits = append(hits, d)
		if strings.HasPrefix(name, "echo") {
			echoHits = append(echoHits, d)
		}
	}
	if len(hits) == 1 {
		return hits[0], ""
	}
	// "im Wohnzimmer" with Amazon's "Echo Wohnzimmer" and the assistant's own "Jarvis Wohnzimmer" in the
	// same room: Amazon's (a better speaker, the owner's wish 19.10.2026); "Jarvis …" or "hier"
	// for the assistant's own.
	if len(echoHits) == 1 {
		return echoHits[0], ""
	}
	if len(hits) == 0 {
		return nil, "Error: Spotify does not see a speaker \"" + wanted + "\". Visible: " + deviceNames(devices) + "." + notVisible
	}
	// The same name twice (an old Echo still known to the account, 18.10.2026): asking "which
	// one?" helps nobody. The playing one, else the one with a volume (a live device), else the first.
	if sameName(hits) {
		for _, d := range hits {
			if d.active {
				return d, ""
			}
		}
		for _, d := range hits {
			if d.volume >= 0 {
				return d, ""
			}
		}
		return hits[0], ""
	}
	several := make([]device, len(hits))
	for i, d := range hits {
		several[i] = *d
	}
	return nil, "Error: several speakers match \"" + wanted + "\": " + deviceNames(several) + ". Name one."
}

// sameNumbers: name and key have the same numbers as words. A number tells devices of one
// family apart: "Jarvis" is not "Jarvis 2", either way round.
func sameNumbers(name, key string) bool {
	numbers := func(text string) []string {
		var out []string
		for _, w := range strings.Fields(text) {
			if isNumber(w) {
				out = append(out, w)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(numbers(name), numbers(key))
}

func sameName(devices []*device) bool {
	for _, d := range devices[1:] {
		if lower(d.name) != lower(devices[0].name) {
			return false
		}
	}
	return true
}

// inRoom: whether music on this device can disturb a conversation at the speaker: one of
// SPOTIFY_ROOM_SPEAKERS, or without that list every speaker at home (not phones, tablets or
// computers: they may be anywhere, often with headphones).
func (s *Service) inRoom(name, typ string) bool {
	if len(s.roomSpeakers) == 0 {
		return typ != "Smartphone" && typ != "Tablet" && typ != "Computer"
	}
	return slices.Contains(s.roomSpeakers, lower(name))
}

// target picks the device to play on (play_music): the named one; without a name the assistant's own
// speaker (the user's wish: music without a place comes here, a room only when they name it),
// else the one playing, else the first in the assistant's room.
func (s *Service) target(devices []device, wanted string) (*device, string) {
	var dev *device
	var errText string
	if wanted != "" {
		dev, errText = findDevice(devices, wanted)
	} else {
		dev = s.ownDevice(devices) // nil while not logged in: the others
		for i := range devices {
			if dev == nil && devices[i].active {
				dev = &devices[i]
			}
		}
		if dev == nil && len(s.roomSpeakers) > 0 {
			dev, errText = findDevice(devices, s.roomSpeakers[0])
		}
		if dev == nil && errText == "" {
			errText = "Error: nothing is playing anywhere; ask the user on which speaker. Visible: " +
				deviceNames(devices) + "." + notVisible
		}
	}
	if dev != nil && dev.restricted {
		return nil, "Error: " + dev.name + " cannot be controlled through Spotify's Web API."
	}
	return dev, errText
}

// isLocal: wanted means the assistant's own speaker ("" counts when nothing else is named).
func (s *Service) isLocal(wanted string) bool {
	switch lower(wanted) {
	case "", "hier", "here":
		return s.LocalSpeaker != ""
	}
	if prompts.IsName(wanted) {
		return s.LocalSpeaker != ""
	}
	d, _ := findDevice([]device{{name: s.LocalSpeaker}}, wanted)
	return s.LocalSpeaker != "" && d != nil
}

// withLocal: when the assistant's own speaker is meant and Spotify does not list it, its player starts
// anew and the devices are loaded again until it shows (up to 10 s); else devices as they are.
// "Hol die Musik zu dir zurück" failed: after playback moved to another Echo, Spotify no longer
// listed it (18.10.2026).
func (s *Service) withLocal(sess session, devices []device, wanted string) []device {
	if s.LocalWake == nil || !s.isLocal(wanted) {
		return devices
	}
	if s.ownDevice(devices) != nil {
		return devices
	}
	log.Printf("[Spotify] %s not listed: its player starts anew", s.LocalSpeaker)
	s.LocalWake()
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		if again, errText := s.loadDevices(sess); errText == "" {
			if s.ownDevice(again) != nil {
				log.Printf("[Spotify] %s listed again after %d s", s.LocalSpeaker, i+1)
				return again
			}
		}
	}
	return devices
}

// localName: wanted, with "hier" and friends as the assistant's own speaker's name.
func (s *Service) localName(wanted string) string {
	if w := lower(wanted); (w == "hier" || w == "here" || prompts.IsName(w)) && s.LocalSpeaker != "" {
		return s.LocalSpeaker
	}
	return wanted
}

// deviceQuery gives "device_id=..." and the name for a named speaker; both "" for the active one.
func (s *Service) deviceQuery(sess session, speaker string) (query, name, errText string) {
	if speaker == "" {
		return "", "", ""
	}
	speaker = s.localName(speaker)
	devices, errText := s.loadDevices(sess)
	if errText != "" {
		return "", "", errText
	}
	devices = s.withLocal(sess, devices, speaker)
	dev, errText := findDevice(devices, speaker)
	if dev == nil {
		return "", "", errText
	}
	return "device_id=" + dev.id, dev.name, ""
}

func withQuery(path, query string) string {
	if query == "" {
		return path
	}
	if strings.Contains(path, "?") {
		return path + "&" + query
	}
	return path + "?" + query
}
