package spotify

import (
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/tools"
)

// --- music_playlist ---------------------------------------------------------------------

func (s *Service) addToPlaylist(sess session, pl playlist, tracks []any) string {
	uris, found, missed := s.resolveTracks(sess, tracks)
	if len(uris) == 0 {
		text := "Error: no song found"
		if missed != "" {
			text += ": " + missed
		}
		return text
	}
	existing, errText := s.playlistItems(sess, pl.id, 4)
	if errText != "" {
		return errText
	}
	var fresh, skipped []string
	for _, uri := range uris {
		there := false
		for _, row := range existing {
			if row.uri == uri {
				there = true
			}
		}
		if there {
			skipped = append(skipped, uri)
		} else {
			fresh = append(fresh, uri)
		}
	}
	if len(fresh) == 0 {
		return "Already in '" + pl.name + "': " + found
	}
	if errText := s.do(sess, "POST", "/playlists/"+pl.id+"/items", marshal(map[string]any{"uris": fresh})); errText != "" {
		return errText
	}
	text := "Added to '" + pl.name + "': " + found
	if len(skipped) > 0 {
		text += ". Some were already in it"
	}
	return text + notFound(missed) + sess.suffix()
}

func (s *Service) musicPlaylist(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	action := stringOr(args, "action", "list")
	tracks, _ := args["tracks"].([]any)

	if action == "create" {
		name := tools.String(args, "name")
		if name == "" {
			return "Error: name missing"
		}
		description, _ := args["description"].(string)
		body := map[string]any{"name": name, "description": description, "public": tools.Bool(args, "public", false)}
		made, _, errText := s.callJSON(sess, "POST", "/me/playlists", marshal(body))
		if errText != "" {
			return errText
		}
		pl := playlist{id: str(made, "id"), name: name}
		text := "Created playlist '" + name + "'" + sess.suffix()
		if link := str(made, "external_urls", "spotify"); link != "" {
			text += ". Link: " + link
		}
		if len(tracks) > 0 {
			text += ". " + s.addToPlaylist(sess, pl, tracks)
		}
		return text
	}

	all, errText := s.loadPlaylists(sess)
	if errText != "" {
		return errText
	}
	if action == "list" {
		var own, followed []string
		for _, p := range all {
			entry := p.name
			if p.tracks >= 0 {
				entry += " (" + strconv.Itoa(p.tracks) + ")"
			}
			if p.own {
				own = append(own, entry)
			} else {
				followed = append(followed, entry+" by "+p.owner)
			}
		}
		return "Own playlists" + sess.suffix() + ": " + joinOrNone(own) +
			"\nFollowed (play only, songs not readable): " + joinOrNone(followed)
	}
	pl, errText := findPlaylist(all, tools.String(args, "playlist"), true)
	if pl == nil {
		return errText
	}

	switch action {
	case "show":
		items, errText := s.playlistItems(sess, pl.id, 2)
		if errText != "" {
			return errText
		}
		text := "'" + pl.name + "'"
		if pl.tracks >= 0 {
			text += " (" + strconv.Itoa(pl.tracks) + " songs)"
		}
		text += ":"
		for i, row := range items {
			text += "\n" + strconv.Itoa(i+1) + ". " + row.line
		}
		if len(items) < pl.tracks {
			text += "\n..."
		}
		return text
	case "add":
		return s.addToPlaylist(sess, *pl, tracks)
	case "remove":
		return s.removeFromPlaylist(sess, *pl, tracks)
	case "rename":
		body := map[string]any{}
		if name := tools.String(args, "name"); name != "" {
			body["name"] = name
		}
		if description, ok := args["description"].(string); ok {
			body["description"] = description
		}
		if public, ok := args["public"].(bool); ok {
			body["public"] = public
		}
		if len(body) == 0 {
			return "Error: nothing to change"
		}
		if errText := s.do(sess, "PUT", "/playlists/"+pl.id, marshal(body)); errText != "" {
			return errText
		}
		return "Changed playlist '" + pl.name + "'"
	case "delete":
		if errText := s.do(sess, "DELETE", "/me/library?uris="+urlEncode(pl.uri), nil); errText != "" {
			return errText
		}
		return "Deleted playlist '" + pl.name + "' (removed from the library)" + sess.suffix()
	}
	return "Error: unknown action"
}

func joinOrNone(parts []string) string {
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func (s *Service) removeFromPlaylist(sess session, pl playlist, tracks []any) string {
	items, errText := s.playlistItems(sess, pl.id, 10)
	if errText != "" {
		return errText
	}
	var removeItems []map[string]any
	var removed, missed []string
	for _, entry := range tracks {
		original, _ := entry.(string)
		wanted := lower(original)
		var uri string
		if wanted == "current" {
			var errText string
			if uri, _, errText = s.current(sess); errText != "" {
				return errText
			}
		}
		// "Artist - Title": the title must match, the artist too if given.
		var artist string
		if dash := strings.Index(wanted, " - "); dash > 0 {
			artist = wanted[:dash]
			wanted = wanted[dash+3:]
		}
		hit := false
		for _, row := range items {
			var match bool
			if uri != "" {
				match = uri == row.uri
			} else {
				match = strings.Contains(row.name, wanted) && (artist == "" || strings.Contains(row.artists, artist))
			}
			if !match {
				continue
			}
			removeItems = append(removeItems, map[string]any{"uri": row.uri})
			removed = append(removed, row.line)
			hit = true
			break
		}
		if !hit {
			missed = append(missed, original)
		}
	}
	if len(removeItems) == 0 {
		return "Error: not in '" + pl.name + "': " + strings.Join(missed, ", ")
	}
	if errText := s.do(sess, "DELETE", "/playlists/"+pl.id+"/items", marshal(map[string]any{"items": removeItems})); errText != "" {
		return errText
	}
	text := "Removed from '" + pl.name + "': " + strings.Join(removed, "; ")
	if len(missed) > 0 {
		text += ". Not in it: " + strings.Join(missed, ", ")
	}
	return text
}

// --- music_library ----------------------------------------------------------------------

func (s *Service) musicLibrary(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	action, typ, what := stringOr(args, "action", "list"), stringOr(args, "type", "track"), tools.String(args, "what")

	if action == "list" {
		path := "/me/" + typ + "s?limit=20"
		if typ == "artist" {
			path = "/me/following?type=artist&limit=20"
		}
		if typ == "playlist" {
			return "Error: playlists come from music_playlist list"
		}
		doc, errText := s.getJSON(sess, path)
		if errText != "" {
			if strings.Contains(errText, "scope") {
				errText += " (scripts/spotify_auth.py has to run once more for followed artists)"
			}
			return errText
		}
		page := doc
		if typ == "artist" {
			page = get(doc, "artists")
		}
		text := "Saved " + typ + "s" + sess.suffix() + " (" + strconv.Itoa(intOr(page, 0, "total")) + ", newest first):"
		for _, entry := range list(page, "items") {
			item := get(entry, typ)
			if item == nil {
				item = entry
			}
			text += "\n- " + describe(item)
		}
		return text
	}

	var uri, line string
	switch {
	case what == "" || lower(what) == "current":
		var errText string
		if uri, line, errText = s.current(sess); errText != "" {
			return errText
		}
	case typ == "playlist":
		all, errText := s.loadPlaylists(sess)
		if errText != "" {
			return errText
		}
		if pl, _ := findPlaylist(all, what, false); pl != nil {
			uri, line = pl.uri, "playlist '"+pl.name+"'"
			break
		}
		item, errText := s.searchFirst(sess, what, typ)
		if item == nil {
			return errText
		}
		uri, line = str(item, "uri"), describe(item)
	default:
		item, errText := s.searchFirst(sess, what, typ)
		if item == nil {
			return errText
		}
		uri, line = str(item, "uri"), describe(item)
	}

	if action == "check" {
		doc, errText := s.getJSON(sess, "/me/library/contains?uris="+urlEncode(uri))
		if errText != "" {
			return errText
		}
		in, _ := doc.([]any)
		if len(in) > 0 {
			if b, _ := in[0].(bool); b {
				return line + " is in the library" + sess.suffix()
			}
		}
		return line + " is not in the library" + sess.suffix()
	}
	if action != "like" && action != "unlike" {
		return "Error: action is like, unlike, check or list"
	}
	method := "DELETE"
	if action == "like" {
		method = "PUT"
	}
	errText = s.do(sess, method, "/me/library?uris="+urlEncode(uri), nil)
	if strings.Contains(errText, "scope") {
		errText += " (scripts/spotify_auth.py has to run once more for following artists)"
	}
	if errText != "" {
		return errText
	}
	if action == "like" {
		return "Saved to the library: " + line + sess.suffix()
	}
	return "Removed from the library: " + line + sess.suffix()
}

// --- music_insights ---------------------------------------------------------------------

func (s *Service) musicInsights(args map[string]any, ctx tools.Context) string {
	sess, errText := s.session(ctx.Person, tools.String(args, "account"))
	if errText != "" {
		return errText
	}
	action, what := tools.String(args, "action"), tools.String(args, "what")

	switch action {
	case "top_tracks", "top_artists":
		period := stringOr(args, "period", "6_months")
		kind, label := "top/tracks", "Top songs"
		if action == "top_artists" {
			kind, label = "top/artists", "Top artists"
		}
		doc, errText := s.getJSON(sess, "/me/"+kind+"?limit=20&time_range="+timeRange(period))
		if errText != "" {
			return errText
		}
		text := label + " (" + period + ")" + sess.suffix() + ":"
		rank := 0
		for _, item := range list(doc, "items") {
			rank++
			text += "\n" + strconv.Itoa(rank) + ". " + describe(item)
		}
		if rank == 0 {
			return text + " none yet"
		}
		return text
	case "recent":
		doc, errText := s.getJSON(sess, "/me/player/recently-played?limit=20")
		if errText != "" {
			return errText
		}
		text := "Recently played" + sess.suffix() + ":"
		now := time.Now()
		for _, entry := range list(doc, "items") {
			text += "\n- " + describe(get(entry, "track")) + " (" + ago(str(entry, "played_at"), now) + ")"
		}
		return text
	}
	if what == "" {
		return "Error: what missing (an artist, album or podcast)"
	}
	switch action {
	case "artist_albums":
		artist, errText := s.searchFirst(sess, what, "artist")
		if artist == nil {
			return errText
		}
		doc, errText := s.getJSON(sess, "/artists/"+str(artist, "id")+"/albums?include_groups=album,single&limit=10")
		if errText != "" {
			return errText
		}
		text := "Albums and singles of " + str(artist, "name") + " (" + strconv.Itoa(intOr(doc, 0, "total")) + ", the 10 newest):"
		for _, album := range list(doc, "items") {
			text += "\n- " + str(album, "name") + " (" + str(album, "album_type") + ", " + year(str(album, "release_date")) + ")"
		}
		return text
	case "album_tracks":
		album, errText := s.searchFirst(sess, what, "album")
		if album == nil {
			return errText
		}
		doc, errText := s.getJSON(sess, "/albums/"+str(album, "id")+"/tracks?limit=50")
		if errText != "" {
			return errText
		}
		text := describe(album) + " (" + year(str(album, "release_date")) + "):"
		for _, track := range list(doc, "items") {
			text += "\n" + strconv.Itoa(intOr(track, 0, "track_number")) + ". " + str(track, "name") +
				" (" + clock(int64(intOr(track, 0, "duration_ms"))) + ")"
		}
		return text
	case "show_episodes":
		show, errText := s.searchFirst(sess, what, "show")
		if show == nil {
			return errText
		}
		doc, errText := s.getJSON(sess, "/shows/"+str(show, "id")+"/episodes?limit=5")
		if errText != "" {
			return errText
		}
		text := "Newest episodes of " + describe(show) + ":"
		for _, episode := range list(doc, "items") {
			if episode == nil {
				continue
			}
			text += "\n- " + str(episode, "release_date") + ": " + str(episode, "name") + " (" +
				strconv.Itoa(intOr(episode, 0, "duration_ms")/60000) + " min"
			if flag(episode, "resume_point", "fully_played") {
				text += ", heard"
			}
			text += ")"
		}
		return text
	}
	return "Error: unknown action"
}
