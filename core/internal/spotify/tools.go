package spotify

import (
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// accountProperty is the same "account" argument of every tool.
const accountProperty = `"account": {"type": "string", "description": "whose Spotify: a person's name, '' = the person talking (else the admin's)"}`

// AddTools registers play_music, music_control, music_speakers, music_playlist, music_library
// and music_insights (nothing if Spotify is not configured). All are slow (TLS to Spotify, a
// few hundred ms per request); every call already runs in its own goroutine. The tools pick
// the account themselves (the person talking, else the admin's), so none is Private.
func (s *Service) AddTools(r *tools.Registry) {
	if !s.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "play_music",
		Description: "Plays music, podcasts or audiobooks from Spotify on a speaker at home or a phone (Spotify Connect: Echos, " +
			"phones with the Spotify app open, ...). type: track, album, artist, playlist (search), my_playlist (an own or " +
			"followed playlist by name), liked (liked songs), top (the user's most played songs, period), show (podcast), " +
			"episode, audiobook, or tracks: a list you put together yourself ('Artist - Title', up to 15) for 'something " +
			"like X', a mood or an occasion; Spotify has no recommendations, use your own music knowledge (music_insights " +
			"shows the user's taste). speaker: name or room, '' = where music plays now; if nothing plays anywhere ask " +
			"where. queue: add songs to the queue instead of playing now. volume: set before it starts (e.g. quiet for " +
			"waking up). Spotify's own mixes (Mix der Woche, Daily Mix, Release Radar) and radio stations are not " +
			"reachable here: use alexa on an Echo for those.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"what": {"type": "string", "description": "search words or playlist name, e.g. 'Daft Punk Get Lucky'; '' for liked/top/tracks"},
                           "type": {"type": "string", "enum": ["track", "album", "artist", "playlist", "my_playlist", "liked", "top", "show", "episode", "audiobook", "tracks"]},
                           "tracks": {"type": "array", "items": {"type": "string"}, "description": "type tracks: 'Artist - Title' each"},
                           "speaker": {"type": "string", "description": "e.g. 'Echo Wohnzimmer', 'Küche', 'Handy'; '' = the assistant's own speaker (the default; a room only when the user names one)"},
                           "shuffle": {"type": "boolean"},
                           "queue": {"type": "boolean"},
                           "volume": {"type": "integer", "minimum": 0, "maximum": 100},
                           "period": {"type": "string", "enum": ["4_weeks", "6_months", "all_time"], "description": "type top"},
                           ` + accountProperty + `},
            "required": ["what", "type", "speaker"]}`,
		Channels: tools.Everywhere,
		Run:      s.playMusic,
	})
	r.Add(tools.Tool{
		Name: "music_control",
		Description: "Controls Spotify playback: status (what plays where, progress, volume, a link to share), pause, resume, next, " +
			"previous, seek (seconds relative, e.g. -30), volume (level or step), shuffle (on), repeat (mode), queue (what " +
			"comes next). speaker: only to control a specific device, '' = the one playing. While Spotify plays, " +
			"pause, skip and volume go here, not to alexa. A sleep timer is create_task with the instruction to pause.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["status", "pause", "resume", "next", "previous", "seek", "volume", "shuffle", "repeat", "queue"]},
                           "seconds": {"type": "integer"},
                           "level": {"type": "integer", "minimum": 0, "maximum": 100},
                           "step": {"type": "integer", "minimum": -100, "maximum": 100},
                           "on": {"type": "boolean"},
                           "mode": {"type": "string", "enum": ["off", "track", "context"]},
                           "speaker": {"type": "string"},
                           ` + accountProperty + `},
            "required": ["action"]}`,
		Channels: tools.Everywhere,
		Run:      s.musicControl,
	})
	r.Add(tools.Tool{
		Name: "music_speakers",
		Description: "Spotify's speakers: list (all devices Spotify sees, which one plays, volumes), move (the music continues on " +
			"another speaker, also an Echo: 'mach die Musik in der Küche an', 'spiel auf dem Echo im Arbeitszimmer weiter', " +
			"'hol die Musik zu dir zurück' = speaker '" + prompts.Name() + "'; not alexa), volume (of one speaker).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["list", "move", "volume"]},
                           "speaker": {"type": "string"},
                           "level": {"type": "integer", "minimum": 0, "maximum": 100},
                           ` + accountProperty + `},
            "required": ["action"]}`,
		Channels: tools.Everywhere,
		Run:      s.musicSpeakers,
	})
	r.Add(tools.Tool{
		Name: "music_playlist",
		Description: "Manages the user's Spotify playlists: list (own and followed), show (songs of an own playlist), create (name, " +
			"optional description and tracks), add / remove tracks ('Artist - Title' or 'current' = the song playing now), " +
			"rename (new name and/or description), delete (after the user confirmed). Only own playlists can be changed " +
			"or read; followed ones can be played.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["list", "show", "create", "add", "remove", "rename", "delete"]},
                           "playlist": {"type": "string", "description": "name of an existing playlist"},
                           "name": {"type": "string", "description": "create: its name; rename: the new name"},
                           "description": {"type": "string"},
                           "public": {"type": "boolean", "description": "default private"},
                           "tracks": {"type": "array", "items": {"type": "string"}, "description": "'Artist - Title' or 'current'"},
                           ` + accountProperty + `},
            "required": ["action"]}`,
		Channels: tools.Everywhere,
		Run:      s.musicPlaylist,
	})
	r.Add(tools.Tool{
		Name: "music_library",
		Description: "The user's Spotify library: like / unlike (save or remove a song, album, podcast, episode, audiobook, follow " +
			"an artist or playlist; what 'current' = the song playing now), check (is it saved?), list (saved items of a " +
			"type, newest first).",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["like", "unlike", "check", "list"]},
                           "type": {"type": "string", "enum": ["track", "album", "artist", "playlist", "show", "episode", "audiobook"]},
                           "what": {"type": "string", "description": "search words or 'current'"},
                           ` + accountProperty + `},
            "required": ["action", "type"]}`,
		Channels: tools.Everywhere,
		Run:      s.musicLibrary,
	})
	r.Add(tools.Tool{
		Name: "music_insights",
		Description: "What the user listens to and catalog facts: top_tracks / top_artists (period), recent (what played lately, " +
			"'what was that song earlier?'), artist_albums (an artist's albums and singles with years), album_tracks, " +
			"show_episodes (a podcast's newest episodes, whether heard). Spotify gives no genres, popularity or similar " +
			"artists any more: use your own knowledge for those.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"action": {"type": "string", "enum": ["top_tracks", "top_artists", "recent", "artist_albums", "album_tracks", "show_episodes"]},
                           "period": {"type": "string", "enum": ["4_weeks", "6_months", "all_time"]},
                           "what": {"type": "string", "description": "artist, album or podcast name"},
                           ` + accountProperty + `},
            "required": ["action"]}`,
		Channels: tools.Everywhere,
		Run:      s.musicInsights,
	})
}
