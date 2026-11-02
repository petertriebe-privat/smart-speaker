package spotify

import (
	"strconv"
	"strings"
)

// searchFirst returns the first result of a search for type (Spotify returns null for items it
// hides); nil with the error text if there is none.
func (s *Service) searchFirst(sess session, query, typ string) (any, string) {
	doc, errText := s.getJSON(sess, "/search?q="+urlEncode(query)+"&type="+typ+"&limit=5")
	if errText != "" {
		return nil, errText
	}
	for _, item := range list(doc, typ+"s", "items") {
		if item != nil {
			return item, ""
		}
	}
	return nil, "Error: nothing found on Spotify for " + typ + " \"" + query + "\"."
}

// current gives the track or episode playing now (uri, line); the error text if nothing plays.
func (s *Service) current(sess session) (uri, line, errText string) {
	doc, errText := s.getJSON(sess, "/me/player/currently-playing?additional_types=episode")
	if errText != "" {
		return "", "", errText
	}
	item := get(doc, "item")
	if item == nil {
		return "", "", "Error: nothing is playing right now."
	}
	return str(item, "uri"), describe(item), ""
}

// resolveTracks turns song names ("Artist - Title", "current" = the song playing, or Spotify
// URIs) into track URIs; found describes them, misses are listed.
func (s *Service) resolveTracks(sess session, wanted []any) (uris []string, found, missed string) {
	var foundParts, missedParts []string
	for count, entry := range wanted {
		if count+1 > s.listTracks {
			missedParts = append(missedParts, "(more than "+strconv.Itoa(s.listTracks)+" songs)")
			break
		}
		name, _ := entry.(string)
		var uri, line string
		switch {
		case lower(name) == "current":
			if u, l, errText := s.current(sess); errText == "" {
				uri, line = u, l
			}
		case strings.HasPrefix(name, "spotify:"):
			uri, line = name, name
		default:
			if item, errText := s.searchFirst(sess, name, "track"); errText == "" {
				uri, line = str(item, "uri"), describe(item)
			}
		}
		if uri == "" {
			missedParts = append(missedParts, name)
			continue
		}
		uris = append(uris, uri)
		foundParts = append(foundParts, line)
	}
	return uris, strings.Join(foundParts, "; "), strings.Join(missedParts, ", ")
}

type playlist struct {
	id, uri, name, owner string
	own                  bool
	tracks               int // -1 unknown
}

func (s *Service) loadPlaylists(sess session) ([]playlist, string) {
	me, errText := s.userID(sess)
	if me == "" {
		return nil, errText
	}
	var out []playlist
	for page := 0; page < s.playlistPages; page++ {
		doc, errText := s.getJSON(sess, "/me/playlists?limit=50&offset="+strconv.Itoa(page*50))
		if errText != "" {
			return nil, errText
		}
		for _, p := range list(doc, "items") {
			if p == nil {
				continue
			}
			pl := playlist{
				id: str(p, "id"), uri: str(p, "uri"), name: str(p, "name"), owner: str(p, "owner", "display_name"),
				own: str(p, "owner", "id") == me || flag(p, "collaborative"),
			}
			if isNull(p, "items", "total") {
				pl.tracks = intOr(p, -1, "tracks", "total")
			} else {
				pl.tracks = intOr(p, -1, "items", "total")
			}
			out = append(out, pl)
		}
		if isNull(doc, "next") {
			break
		}
	}
	return out, ""
}

func playlistNames(all []playlist, ownOnly bool) string {
	var names []string
	for _, p := range all {
		if ownOnly && !p.own {
			continue
		}
		if len(names) == 40 {
			names = append(names, "...")
			break
		}
		names = append(names, p.name)
	}
	return strings.Join(names, ", ")
}

func kindOf(ownOnly bool) string {
	if ownOnly {
		return "Own"
	}
	return "Known"
}

// findPlaylist finds a playlist by name: exact (ignoring case), else the only one containing it.
func findPlaylist(all []playlist, wanted string, ownOnly bool) (*playlist, string) {
	key := lower(wanted)
	if key == "" {
		return nil, "Error: which playlist? " + kindOf(ownOnly) + " playlists: " + playlistNames(all, ownOnly)
	}
	var hit *playlist
	hits := 0
	for i := range all {
		p := &all[i]
		if ownOnly && !p.own {
			continue
		}
		if lower(p.name) == key {
			return p, ""
		}
		if strings.Contains(lower(p.name), key) || wordsIn(lower(p.name), key, false) {
			hit = p
			hits++
		}
	}
	if hits == 1 {
		return hit, ""
	}
	text := "Error: no playlist \""
	if hits > 0 {
		text = "Error: several playlists match \""
	}
	text += wanted + "\". " + kindOf(ownOnly) + " playlists: " + playlistNames(all, ownOnly)
	if hits == 0 && ownOnly {
		for _, p := range all {
			if !p.own && strings.Contains(lower(p.name), key) {
				text += ". '" + p.name + "' belongs to " + p.owner + ": Spotify lets only its owner change it or read its songs."
			}
		}
	}
	return nil, text
}

type playlistItem struct {
	uri, line, name, artists string // name and artists lower case
}

// playlistItems reads up to pages x 50 items of an own playlist.
func (s *Service) playlistItems(sess session, id string, pages int) ([]playlistItem, string) {
	var all []playlistItem
	for page := 0; page < pages; page++ {
		doc, errText := s.getJSON(sess, "/playlists/"+id+"/items?limit=50&additional_types=episode&offset="+strconv.Itoa(page*50))
		if errText != "" {
			return nil, errText
		}
		for _, entry := range list(doc, "items") {
			item := get(entry, "item")
			if item == nil {
				item = get(entry, "track")
			}
			if item == nil {
				continue
			}
			all = append(all, playlistItem{
				uri: str(item, "uri"), line: describe(item),
				name: lower(str(item, "name")), artists: lower(names(list(item, "artists"))),
			})
		}
		if isNull(doc, "next") {
			break
		}
	}
	return all, ""
}
