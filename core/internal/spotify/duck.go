package spotify

import (
	"log"
	"strconv"
)

// duckedDevice: a device turned down while the assistant talks, with the volume to put back.
type duckedDevice struct {
	refresh, device  string
	volume, original int
}

// Duck tells the service whether the assistant is talking at the speaker at home (a voice
// conversation, not a phone call, Telegram or WhatsApp: then nobody needs to hear the assistant over
// the music at home). The caller invokes it with true when a conversation opens and with false
// when it ends (the M5 calls spotify::setConversation every loop; repeated calls with the same
// value do nothing, so calling it often is fine).
//
// Music playing in the assistant's room (SPOTIFY_ROOM_SPEAKERS; without that list on any speaker at
// home, not on phones or computers), any account, is turned down to SPOTIFY_DUCK_VOLUME while
// the conversation lasts and back afterwards, unless someone changed the volume meanwhile.
// The work runs in a background goroutine, in order, and never delays the caller. Without a
// configured account or with SPOTIFY_DUCK_VOLUME 0 it does nothing.
func (s *Service) Duck(on bool) {
	if !s.Configured() || s.duckVolume <= 0 {
		return
	}
	s.duckMu.Lock()
	if on == s.open {
		s.duckMu.Unlock()
		return
	}
	s.open = on
	s.duckMu.Unlock()
	s.duckOnce.Do(func() {
		s.duckQueue = make(chan bool, 4)
		go func() {
			for open := range s.duckQueue {
				if open {
					s.duck()
				} else {
					s.restore()
				}
			}
		}()
	})
	select {
	case s.duckQueue <- on:
	default:
		log.Printf("[Spotify] worker busy, volume left as it is")
	}
}

// forgetDuck: someone set a volume himself, so it is not put back after the conversation.
func (s *Service) forgetDuck(device string) {
	s.duckMu.Lock()
	defer s.duckMu.Unlock()
	kept := s.ducked[:0]
	for _, d := range s.ducked {
		if device != "" && d.device != device {
			kept = append(kept, d)
		}
	}
	s.ducked = kept
}

func (s *Service) duck() {
	for _, sess := range s.allSessions() {
		state, errText := s.getJSON(sess, "/me/player")
		if errText != "" || !flag(state, "is_playing") {
			continue
		}
		name, typ, id := str(state, "device", "name"), str(state, "device", "type"), str(state, "device", "id")
		volume := intOr(state, -1, "device", "volume_percent")
		if !s.inRoom(name, typ) { // music elsewhere does not disturb
			continue
		}
		if s.LocalSpeaker != "" && lower(name) == lower(s.LocalSpeaker) { // ducked on the device
			continue
		}
		if !flag(state, "device", "supports_volume") || id == "" || volume <= s.duckVolume {
			continue
		}
		errText = s.do(sess, "PUT", "/me/player/volume?volume_percent="+strconv.Itoa(s.duckVolume)+"&device_id="+id, nil)
		if name == "" {
			name = "?"
		}
		log.Printf("[Spotify] %s turned down from %d%%%s", name, volume, errText)
		if errText == "" {
			s.duckMu.Lock()
			s.ducked = append(s.ducked, duckedDevice{sess.refresh, id, s.duckVolume, volume})
			s.duckMu.Unlock()
		}
	}
}

func (s *Service) restore() {
	s.duckMu.Lock()
	pending := s.ducked
	s.ducked = nil
	s.duckMu.Unlock()
	for _, d := range pending {
		sess := session{refresh: d.refresh}
		state, errText := s.getJSON(sess, "/me/player")
		if errText != "" {
			continue
		}
		// Only where the music still plays as the assistant left it.
		if str(state, "device", "id") != d.device || intOr(state, -1, "device", "volume_percent") != d.volume {
			continue
		}
		errText = s.do(sess, "PUT", "/me/player/volume?volume_percent="+strconv.Itoa(d.original)+"&device_id="+d.device, nil)
		log.Printf("[Spotify] volume back to %d%%%s", d.original, errText)
	}
}
