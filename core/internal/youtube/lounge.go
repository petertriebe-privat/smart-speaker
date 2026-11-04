package youtube

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

// Playing: the YouTube app's screen over the Lounge API.
// The app ignores a video in the DIAL launch (v=...) nowadays: it starts and waits for a sender.
// Like a phone casting, the assistant takes the app's screenId (DIAL additionalData), trades it for a
// lounge token, opens a session and sends the video as the playlist.

const (
	loungeHost  = "https://www.youtube.com"
	appStartMax = 20 * time.Second // a cold YouTube app takes a while to show its screen id
)

// screenID: the screen id of the running app, launching it over DIAL when needed. "" with an
// error text.
func (s *Service) screenID(t tv) (string, string) {
	body, running := s.appInfo(t)
	if id := between(body, "<screenId>", "</screenId>"); running && id != "" {
		return id, ""
	}
	req, err := http.NewRequest("POST", t.appURL+"YouTube", strings.NewReader(""))
	if err != nil {
		return "", "Error: bad DIAL address"
	}
	req.Header.Set("Content-Type", "text/plain; charset=\"utf-8\"")
	req.Header.Set("Origin", "https://www.youtube.com")
	start := &http.Client{Transport: s.lan.Transport, Timeout: 8 * time.Second}
	resp, err := start.Do(req)
	status := 0
	if err == nil {
		status = resp.StatusCode
		resp.Body.Close()
	}
	if err != nil || (status != 201 && status != 200) {
		reason := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			reason = err.Error()
		}
		return "", "Error: \"" + t.name + "\" did not start YouTube (" + reason + ")"
	}
	for began := time.Now(); time.Since(began) < appStartMax; time.Sleep(time.Second) {
		body, running = s.appInfo(t)
		if id := between(body, "<screenId>", "</screenId>"); running && id != "" {
			return id, ""
		}
	}
	return "", "Error: YouTube started on \"" + t.name + "\" but did not get ready"
}

// loungePost: a form POST to the Lounge API. Returns the HTTP status and the answer; a kept
// connection the server closed meanwhile is retried once.
func (s *Service) loungePost(path, form string) (int, string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest("POST", loungeHost+path, strings.NewReader(form))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Origin", "https://www.youtube.com")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return resp.StatusCode, string(data), err
	}
	return 0, "", lastErr
}

// lounge: a remote's session with the app's screen: joins whatever it is playing.
type lounge struct {
	s           *Service
	common, sid string
	gsession    string
	rid, ofs    int
}

func randomHex() string {
	var b [4]byte
	rand.Read(b[:])
	return fmt.Sprintf("%08x", binary.BigEndian.Uint32(b[:]))
}

// open trades the screen id for a lounge token and binds a session. "" or an error text.
func (l *lounge) open(screen string) string {
	l.rid = 1
	status, body, err := l.s.loungePost("/api/lounge/pairing/get_lounge_token_batch", "screen_ids="+urlEncode(screen))
	var tokens struct {
		Screens []struct {
			LoungeToken string `json:"loungeToken"`
		} `json:"screens"`
	}
	if status != 200 || err != nil || json.Unmarshal([]byte(body), &tokens) != nil {
		return fmt.Sprintf("Error: YouTube gave no lounge token (HTTP %d)", status)
	}
	if len(tokens.Screens) == 0 || tokens.Screens[0].LoungeToken == "" {
		return "Error: YouTube gave no lounge token"
	}
	device := randomHex() + "-assistant-" + randomHex()
	l.common = "device=REMOTE_CONTROL&id=" + device +
		"&name=" + prompts.Name() + "&app=youtube-desktop&mdx-version=3&VER=8&v=2&t=1&CVER=1&theme=cl&loungeIdToken=" +
		urlEncode(tokens.Screens[0].LoungeToken)
	status, session, err := l.s.loungePost(fmt.Sprintf("/api/lounge/bc/bind?%s&RID=%d&zx=assistant", l.common, l.rid), "count=0")
	l.rid++
	l.sid = between(session, "[\"c\",\"", "\"")
	l.gsession = between(session, "[\"S\",\"", "\"")
	if status != 200 || err != nil || l.sid == "" || l.gsession == "" {
		return fmt.Sprintf("Error: no session with the TV's YouTube app (HTTP %d)", status)
	}
	return ""
}

// send: one command with its fields ("req0_" is added), e.g. seekTo, "newTime=120".
func (l *lounge) send(command, fields string) string {
	form := fmt.Sprintf("count=1&ofs=%d&req0__sc=%s", l.ofs, command)
	l.ofs++
	if fields != "" {
		for _, field := range strings.Split(fields, "&") {
			form += "&req0_" + field
		}
	}
	path := fmt.Sprintf("/api/lounge/bc/bind?%s&RID=%d&SID=%s&gsessionid=%s&zx=assistant",
		l.common, l.rid, urlEncode(l.sid), urlEncode(l.gsession))
	l.rid++
	status, _, err := l.s.loungePost(path, form)
	if status == 200 && err == nil {
		return ""
	}
	return fmt.Sprintf("Error: the TV did not take %s (HTTP %d)", command, status)
}

// remote: a session kept per screen: opening one takes a token and a bind (two HTTPS requests,
// seconds during a conversation), a further command on it one request.
type remote struct {
	screen string
	lounge *lounge
	usedAt time.Time
}

// command: one command to the screen over its kept session; a missing, old or failing one is
// opened anew. "" or an error text.
func (s *Service) command(screen, name, fields string) string {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	var r *remote
	for _, candidate := range s.remotes {
		if candidate.screen == screen {
			r = candidate
		}
	}
	if r == nil {
		if len(s.remotes) >= 8 { // a few TVs; screens change after app updates
			s.remotes = s.remotes[1:]
		}
		r = &remote{screen: screen}
		s.remotes = append(s.remotes, r)
	}
	session := time.Duration(prompts.Int("LOUNGE_SESSION_MS", 600000)) * time.Millisecond
	for attempt := 0; attempt < 2; attempt++ {
		fresh := attempt > 0 || r.usedAt.IsZero() || time.Since(r.usedAt) > session
		if fresh {
			r.lounge = &lounge{s: s}
			if err := r.lounge.open(screen); err != "" {
				r.usedAt = time.Time{}
				return err
			}
		}
		err := r.lounge.send(name, fields)
		if err == "" {
			r.usedAt = time.Now()
			return ""
		}
		r.usedAt = time.Time{}
		if fresh {
			return err // a new session failed too
		}
	}
	return "Error: no session with the TV's YouTube app"
}

// pendingSeek: the seek after play_video, done a few seconds later: play_video answers at once
// instead of keeping the assistant silent meanwhile. The latest play_video wins.
type pendingSeek struct {
	generation int
	timer      *time.Timer
}

func (s *Service) seekLater(screen string, seconds int, generation int) {
	s.remoteMu.Lock()
	current := s.pending.generation == generation
	s.remoteMu.Unlock()
	if !current {
		return
	}
	err := s.command(screen, "seekTo", fmt.Sprintf("newTime=%d", seconds))
	if err == "" {
		err = "done"
	}
	log.Printf("[YouTube] seek to %d s: %s", seconds, err)
}

// sendToScreen plays video; the app resumes where the TV's account left the video, so a seek to
// start follows once it is playing (seekLater).
func (s *Service) sendToScreen(screen, video string, start int) string {
	id := urlEncode(video)
	err := s.command(screen, "setPlaylist", fmt.Sprintf("videoId=%s&currentTime=%d&currentIndex=-1&audioOnly=false&videoIds=%s"+
		"&listId=&prioritizeMobileSenderPlaybackStateOnConnection=true", id, start, id))
	if err != "" {
		return err
	}
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	if s.pending.timer != nil {
		s.pending.timer.Stop()
	}
	s.pending.generation++
	generation := s.pending.generation
	delay := time.Duration(prompts.Int("YOUTUBE_SEEK_DELAY_MS", 5000)) * time.Millisecond
	s.pending.timer = time.AfterFunc(delay, func() { s.seekLater(screen, start, generation) })
	return ""
}

// --- Tools ----------------------------------------------------------------------------

// setLastTV remembers the TV reached last, also across restarts.
func (s *Service) setLastTV(name string) {
	s.tvMu.Lock()
	s.lastTV = name
	s.tvMu.Unlock()
	os.WriteFile(filepath.Join(store.Dir, "youtube_tv.txt"), []byte(name), 0o600)
}

func (s *Service) getLastTV() string {
	s.tvMu.Lock()
	defer s.tvMu.Unlock()
	if s.lastTV == "" {
		data, _ := os.ReadFile(filepath.Join(store.Dir, "youtube_tv.txt"))
		s.lastTV = strings.TrimSpace(string(data))
	}
	return s.lastTV
}

// setLastVideo remembers the video the assistant started last, with the time (across restarts).
func (s *Service) setLastVideo(id string) {
	os.WriteFile(filepath.Join(store.Dir, "youtube_video.txt"), []byte(id+" "+time.Now().Format(time.RFC3339)), 0o600)
}

// lastVideoLink: the video Jarvis started in the last 12 hours, "" if none (the user may have
// cast another one from the phone since; older is too likely a different one).
func (s *Service) lastVideoLink() string {
	data, _ := os.ReadFile(filepath.Join(store.Dir, "youtube_video.txt"))
	id, at, _ := strings.Cut(strings.TrimSpace(string(data)), " ")
	when, err := time.Parse(time.RFC3339, at)
	if id == "" || err != nil || time.Since(when) > 12*time.Hour {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + id
}

// watching: a video plays now; the conversation ends once the assistant is quiet - left open it took the
// people in the video for the user and talked with them (18.10.2026).
func watching(ctx tools.Context) {
	if ctx.Conversation != nil && ctx.Channel == tools.Voice {
		ctx.Conversation.EndSoon()
	}
}

func (s *Service) playVideo(args map[string]any, ctx tools.Context) string {
	id, wanted := tools.String(args, "video_id"), tools.String(args, "tv")
	start, _ := tools.Int(args, "start_seconds")
	start = max(0, start)
	if id == "" {
		return "Error: video_id missing (from find_video)"
	}
	if s.sticks != nil {
		link := "https://www.youtube.com/watch?v=" + id
		if start > 0 {
			link += fmt.Sprintf("&t=%ds", start)
		}
		if ok, name := s.sticks.YouTube(wanted, "open", link); ok {
			s.setLastTV(name)
			s.setLastVideo(id)
			watching(ctx)
			return "Playing on \"" + name + "\" (switched on if it slept; the picture may take a few seconds)."
		}
	}
	started := time.Now()
	var errText string
	for attempt := 0; attempt < 2; attempt++ {
		var t tv
		if t, errText = s.findTV(wanted, attempt > 0); errText != "" { // not found: search the LAN again
			if attempt == 0 && !strings.Contains(errText, "several") {
				continue
			}
			return errText
		}
		var screen string
		if screen, errText = s.screenID(t); screen == "" {
			if attempt == 0 && strings.Contains(errText, "did not start") {
				continue // address changed: search again
			}
			return errText
		}
		errText = s.sendToScreen(screen, id, start)
		result := errText
		if result == "" {
			result = "sent"
		}
		log.Printf("[YouTube] play on %s: %s after %d ms", t.name, result, time.Since(started).Milliseconds())
		if errText != "" {
			return errText
		}
		from := ""
		if start != 0 {
			from = fmt.Sprintf(" from second %d", start)
		}
		s.setLastTV(t.name)
		s.setLastVideo(id)
		return "Playing on \"" + t.name + "\"" + from + "."
	}
	return errText
}

// controlVideo: pause, play, seek or stop what the YouTube app on a TV is playing (a remote
// joins it).
// runningTV: the one TV whose YouTube app runs (the TVs asked side by side).
func (s *Service) runningTV() (tv, string) {
	list := s.knownTVs(false)
	running := make([]bool, len(list))
	var wg sync.WaitGroup
	for i, t := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, running[i] = s.appInfo(t)
		}()
	}
	wg.Wait()
	var on []tv
	for i, t := range list {
		if running[i] {
			on = append(on, t)
		}
	}
	switch len(on) {
	case 1:
		return on[0], ""
	case 0:
		// the TV last controlled: its error says what the app reports
		last := s.getLastTV()
		if t, errText := matchTV(list, last); last != "" && errText == "" {
			return t, ""
		}
		return tv{}, "Error: YouTube runs on no TV; TVs: " + tvNames(list)
	}
	return tv{}, "Error: YouTube runs on several TVs, say which: " + tvNames(on)
}

func (s *Service) controlVideo(args map[string]any, ctx tools.Context) string {
	wanted, action := tools.String(args, "tv"), tools.String(args, "action")
	name := map[string]string{"pause": "pause", "play": "play", "seek": "seekTo", "stop": "stopVideo"}[action]
	if name == "" {
		return "Error: action is pause, play, seek or stop"
	}
	started := time.Now()
	var t tv
	var errText string
	if wanted == "" { // no TV named ("pause the video"): the one YouTube runs on, else the last one
		if t, errText = s.runningTV(); errText != "" {
			return errText
		}
		wanted = t.name
	}
	if s.sticks != nil && action != "seek" { // a Fire TV: its remote keys over ADB, woken first
		link := ""
		if action == "play" {
			link = s.lastVideoLink() // started anew if the stick lost it in standby
		}
		if ok, stick := s.sticks.YouTube(wanted, action, link); ok {
			if strings.HasPrefix(stick, "Error") {
				return stick
			}
			s.setLastTV(stick)
			if action != "play" {
				return "Done on \"" + stick + "\": " + action + "."
			}
			watching(ctx)
			return "Done on \"" + stick + "\": play (switched on if it slept)."
		}
	}
	if t.name == "" {
		if t, errText = s.findTV(wanted, false); errText != "" {
			t, errText = s.findTV(wanted, true)
		}
	}
	if errText != "" {
		return errText
	}
	body, running := s.appInfo(t)
	screen := between(body, "<screenId>", "</screenId>")
	if !running || screen == "" {
		state := between(body, "<state>", "</state>")
		if state == "" {
			state = "no answer"
		}
		return "Error: YouTube is not running on \"" + t.name + "\" (app state: " + state + "). Start the video again with play_video."
	}
	fields := ""
	if action == "seek" {
		seconds, _ := tools.Int(args, "seconds")
		fields = fmt.Sprintf("newTime=%d", max(0, seconds))
	}
	errText = s.command(screen, name, fields)
	result := errText
	if result == "" {
		result = "sent"
	}
	log.Printf("[YouTube] %s on %s: %s after %d ms", name, t.name, result, time.Since(started).Milliseconds())
	if errText != "" {
		return errText
	}
	s.setLastTV(t.name)
	if action == "play" {
		watching(ctx)
	}
	return "Done on \"" + t.name + "\": " + action + "."
}
