// Package screen: The assistant's cards on the TV (#108). The app in devices/android runs on the Fire TV
// sticks and shows what show_on_screen publishes over MQTT: a card in the corner above any app
// (a film keeps playing) or the whole screen. Topics under jarvis/screen/<room id>:
//
//	status  retained, from the app: {"state": online|offline, "display": on|off, "room", "name"}
//	show    a card: {"id", "mode": overlay|fullscreen, "title", "text", "items", "image", "seconds"}
//	clear   hides what is shown
//	ack     the app's answer: the card's id once it is on the screen
//
// A screen that is offline or dark is woken over ADB (the fire_tv package) and its app started.
package screen

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"echo-assistant/internal/mqtt"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

const (
	prefix = "assistant/screen/"
	// StartCommand: the shell command that (re)starts the app's service on the stick; the service
	// directly, not an activity, so a film in front is not interrupted.
	StartCommand = "am start-foreground-service -n dev.smartspeaker.screen/.ScreenService"
	maxItems     = 30
	maxSeconds   = 3600
)

// How long to wait: a woken stick until its app reports online with the display on (Wi-Fi and
// the MQTT connection come back after sleep), and the app's ack of a card.
var (
	startTimeout = 45 * time.Second // a TV that was off: power on, the app and its status
	ackTimeout   = 5 * time.Second
	poll         = 200 * time.Millisecond
)

type status struct {
	State   string `json:"state"`
	Display string `json:"display"`
	Room    string `json:"room"`
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "phone": the phone app (#75), which takes notifications (Notify)
}

func (s status) ready() bool { return s.State == "online" && s.Display == "on" }

type publisher interface {
	Publish(topic string, payload []byte, retain bool) error
}

type Service struct {
	// Start wakes the stick in a room and runs command there (fire_tv over ADB): "" or the error.
	Start func(room, command string) string
	// Room: this speaker's room, the default when neither the call nor the conversation names one.
	Room string
	// Device: this the assistant's name, for what it leaves in a room (recent.go).
	Device string

	pub     publisher
	mu      sync.Mutex
	screens map[string]status      // by room id
	acks    map[string]chan string // card id -> how it was shown ("fullscreen paused")
	recent  map[string]recent      // "<room id>/shown|talk" (recent.go)
}

// New connects to the broker (MQTT_URL) and follows the screens' status; nil without a URL.
func New(url, user, password, clientID string) *Service {
	if url == "" {
		return nil
	}
	s := newService(nil)
	c, err := mqtt.New(mqtt.Options{URL: url, User: user, Password: password, ClientID: clientID,
		OnConnect: func(c *mqtt.Client) {
			if err := c.Subscribe(prefix+"+/status", prefix+"+/ack", prefix+"+/shown", prefix+"+/talk"); err != nil {
				log.Printf("[Screen] subscribe: %v", err)
			}
		},
		OnMessage: func(m mqtt.Message) { s.receive(m.Topic, m.Payload) }})
	if err != nil {
		log.Printf("[Screen] off: %v", err)
		return nil
	}
	s.pub = c
	go c.Run(30 * time.Second)
	return s
}

func newService(pub publisher) *Service {
	return &Service{pub: pub, screens: map[string]status{}, acks: map[string]chan string{}, recent: map[string]recent{}}
}

func (s *Service) receive(topic string, payload []byte) {
	id, kind, ok := strings.Cut(strings.TrimPrefix(topic, prefix), "/")
	if !ok || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "status":
		var st status
		if len(payload) == 0 || json.Unmarshal(payload, &st) != nil {
			delete(s.screens, id) // retained status removed: the screen is gone
			return
		}
		st.Room = cmp.Or(st.Room, id)
		s.screens[id] = st
	case "shown", "talk":
		s.keep(id, kind, payload)
	case "ack": // "<card id> <mode>[ paused]" (older apps: the id alone)
		cardID, how, _ := strings.Cut(string(payload), " ")
		if ch, ok := s.acks[cardID]; ok {
			ch <- how
			delete(s.acks, cardID)
		}
	}
}

func (s *Service) screen(id string) (status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.screens[id]
	return st, ok
}

// Live shows a card without waiting for an answer (The assistant's state and words, a hint) on the TV in
// room, if one is there, online and on; "" = every such TV. It never wakes a TV.
func (s *Service) Live(room string, card map[string]any) {
	if s == nil || s.pub == nil {
		return
	}
	payload, _ := json.Marshal(card)
	s.mu.Lock()
	var ids []string
	for id, st := range s.screens {
		if st.ready() && (room == "" || id == mqtt.TopicID(room)) {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.pub.Publish(prefix+id+"/show", payload, false)
	}
}

// Notify sends a card as a notification to every phone that is online, its screen on or off
// (the phone app shows it as an Android notification, in the car too; a tap opens the card).
// Returns how many phones got it.
func (s *Service) Notify(card map[string]any) int {
	if s == nil || s.pub == nil {
		return 0
	}
	card["push"] = true
	payload, _ := json.Marshal(card)
	s.mu.Lock()
	var ids []string
	for id, st := range s.screens {
		if st.Kind == "phone" && st.State == "online" {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.pub.Publish(prefix+id+"/show", payload, false)
	}
	return len(ids)
}

// On: the TV in room is there, online and on (the backend shows what it talks about there).
func (s *Service) On(room string) bool {
	if s == nil || room == "" {
		return false
	}
	st, ok := s.screen(mqtt.TopicID(room))
	return ok && st.ready()
}

// toAll: room "all" – every TV that is on (none is woken), without waiting for answers.
func (s *Service) toAll(args map[string]any, mode string) string {
	var payload []byte
	if mode == "clear" {
		payload = nil
	} else {
		c, errText := cardOf(args, mode)
		if errText != "" {
			return errText
		}
		payload, _ = json.Marshal(c)
	}
	s.mu.Lock()
	var rooms, ids []string
	for id, st := range s.screens {
		if st.ready() {
			topic := prefix + id + "/show"
			if mode == "clear" {
				topic = prefix + id + "/clear"
			}
			s.pub.Publish(topic, payload, false)
			rooms = append(rooms, st.Room)
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	shown := ""
	if mode != "clear" {
		c, _ := cardOf(args, mode)
		shown = summary(c)
	}
	for _, id := range ids {
		s.publishRecent(id, "shown", shown)
	}
	if len(rooms) == 0 {
		return "No TV is on right now (none was woken)"
	}
	slices.Sort(rooms)
	return "Shown on the TVs that are on: " + strings.Join(rooms, ", ")
}

// rooms: the rooms with a screen, sorted.
func (s *Service) rooms() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []string
	for _, st := range s.screens {
		list = append(list, st.Room)
	}
	slices.Sort(list)
	return list
}

// find: the screen in room; with no room at all the only screen there is. Error text otherwise.
func (s *Service) find(room string) (string, status, string) {
	rooms := s.rooms()
	if len(rooms) == 0 {
		return "", status{}, "Error: no TV screen known (the assistant app on a Fire TV has never reported)"
	}
	if room == "" {
		if len(rooms) == 1 {
			room = rooms[0]
		} else {
			return "", status{}, "Error: which room? Screens: " + strings.Join(rooms, ", ")
		}
	}
	id := mqtt.TopicID(room)
	if st, ok := s.screen(id); ok {
		return id, st, ""
	}
	return "", status{}, "Error: no TV screen in " + room + ". Screens: " + strings.Join(rooms, ", ")
}

// wake: the screen is offline or dark: wake its stick, start the app, wait until it is ready.
func (s *Service) wake(id string, st status) string {
	if s.Start == nil {
		return "Error: the screen in " + st.Room + " is " + describe(st) + " and cannot be woken here"
	}
	// A TV that was off answers Home Assistant's ADB late: the command timed out after 8 s while
	// it woke all the same (29.10.2026, "not reachable" with the TV on). Its status decides.
	started := make(chan string, 1)
	go func() { started <- s.Start(st.Room, StartCommand) }()
	startErr := ""
	for end := time.Now().Add(startTimeout); time.Now().Before(end); time.Sleep(poll) {
		select {
		case startErr = <-started:
		default:
		}
		if now, _ := s.screen(id); now.ready() {
			return ""
		}
	}
	now, _ := s.screen(id)
	if startErr != "" {
		return "Error: the screen in " + st.Room + " is still " + describe(now) + " (waking it: " + startErr + ")"
	}
	return "Error: the screen in " + st.Room + " was woken but is still " + describe(now)
}

func describe(st status) string {
	if st.State != "online" {
		return "offline"
	}
	return "dark (TV off)"
}

func (s *Service) run(args map[string]any, ctx tools.Context) string {
	if s.pub == nil {
		return "Error: no MQTT connection"
	}
	mode := cmp.Or(tools.String(args, "mode"), "auto")
	if mode != "auto" && mode != "overlay" && mode != "fullscreen" && mode != "clear" {
		return "Error: mode is auto, overlay, fullscreen or clear"
	}
	if strings.EqualFold(tools.String(args, "room"), "all") {
		return s.toAll(args, mode)
	}
	id, st, errText := s.find(cmp.Or(tools.String(args, "room"), ctx.Room, s.Room))
	if errText != "" {
		return errText
	}
	if mode == "clear" {
		if err := s.pub.Publish(prefix+id+"/clear", nil, false); err != nil {
			return "Error: " + err.Error()
		}
		s.publishRecent(id, "shown", "")
		return "Screen in " + st.Room + " cleared"
	}
	card, errText := cardOf(args, mode)
	if errText != "" {
		return errText
	}
	pictureNote := ""
	if total, dropped := checkPictures(&card); dropped > 0 {
		if dropped == total && card.Text == "" && len(card.Items) == 0 && len(card.Blocks) == 0 && card.URL == "" {
			return fmt.Sprintf("Error: none of the %d pictures loads (the sites refuse showing them elsewhere); "+
				"nothing was shown. Search again for other pictures, or say so.", total)
		}
		pictureNote = fmt.Sprintf("; %d of %d pictures did not load and were left out", dropped, total)
	}
	if !st.ready() {
		if errText := s.wake(id, st); errText != "" {
			return errText
		}
	}
	ack := make(chan string, 1)
	s.mu.Lock()
	s.acks[card.ID] = ack
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.acks, card.ID)
		s.mu.Unlock()
	}()
	payload, _ := json.Marshal(card)
	if err := s.pub.Publish(prefix+id+"/show", payload, false); err != nil {
		return "Error: " + err.Error()
	}
	select {
	case how := <-ack:
		s.publishRecent(id, "shown", summary(card))
		return "Shown on the TV in " + st.Room + " (" + describeShown(cmp.Or(how, mode)) + pictureNote + ")"
	case <-time.After(ackTimeout):
		return "Error: the TV in " + st.Room + " did not confirm the card (app not running?)"
	}
}

// checkPictures leaves out the card's pictures that do not load (netx.Pictures): the TV showed
// the frames of a gallery for a moment and then its heading alone, and Jarvis said "here you see
// the pictures" (29.10.2026). Returns how many there were and how many were left out.
func checkPictures(c *card) (total, dropped int) {
	type ref struct {
		block, item int // block -1: the card's image; item -1: an image block's url
		link        string
	}
	var refs []ref
	if c.Image != "" {
		refs = append(refs, ref{-1, -1, c.Image})
	}
	for b, block := range c.Blocks {
		switch block["type"] {
		case "image":
			if link, _ := block["url"].(string); link != "" {
				refs = append(refs, ref{b, -1, link})
			}
		case "gallery":
			items, _ := block["items"].([]any)
			for i, it := range items {
				link, _ := it.(string)
				if m, ok := it.(map[string]any); ok {
					link, _ = m["url"].(string)
				}
				refs = append(refs, ref{b, i, link})
			}
		}
	}
	if len(refs) == 0 {
		return 0, 0
	}
	links := make([]string, len(refs))
	for i, r := range refs {
		links[i] = r.link
	}
	ok := netx.Pictures(links)
	bad := map[int]map[int]bool{} // block -> items left out
	for i, r := range refs {
		if ok[i] {
			continue
		}
		dropped++
		if r.block < 0 {
			c.Image = ""
			continue
		}
		if bad[r.block] == nil {
			bad[r.block] = map[int]bool{}
		}
		bad[r.block][r.item] = true
	}
	var blocks []map[string]any
	for b, block := range c.Blocks {
		gone := bad[b]
		switch {
		case gone == nil:
			blocks = append(blocks, block)
		case gone[-1]: // an image block whose picture does not load
		default:
			items, _ := block["items"].([]any)
			var kept []any
			for i, it := range items {
				if !gone[i] {
					kept = append(kept, it)
				}
			}
			if len(kept) > 0 {
				block["items"] = kept
				blocks = append(blocks, block)
			}
		}
	}
	c.Blocks = blocks
	return len(refs), dropped
}

// describeShown turns the app's "fullscreen paused" into words for the model.
func describeShown(how string) string {
	mode, paused, _ := strings.Cut(how, " ")
	text := map[string]string{"overlay": "small in the corner", "fullscreen": "full screen", "web": "full screen"}[mode]
	if text == "" {
		text = mode
	}
	if paused == "paused" {
		text += ", the film is paused until the card closes"
	}
	return text
}

// card: the app's format (devices/android/docs/design.md). Blocks pass through as the model wrote
// them; the app draws what it knows and skips the rest.
type card struct {
	ID       string           `json:"id"`
	Mode     string           `json:"mode"` // auto: the app chooses (a hint stays small while a film plays)
	Kind     string           `json:"kind,omitempty"`
	Title    string           `json:"title,omitempty"`
	Subtitle string           `json:"subtitle,omitempty"`
	Text     string           `json:"text,omitempty"`
	Items    []string         `json:"items,omitempty"`
	Image    string           `json:"image,omitempty"`
	URL      string           `json:"url,omitempty"` // a web page (mode web) or a video file
	Blocks   []map[string]any `json:"blocks,omitempty"`
	Seconds  int              `json:"seconds,omitempty"`
}

var kinds = map[string]bool{"info": true, "list": true, "recipe": true, "timer": true, "weather": true,
	"calendar": true, "message": true, "alert": true, "camera": true, "music": true, "home": true,
	"energy": true, "route": true, "package": true, "call": true, "search": true, "choice": true,
	"web": true, "video": true}

const maxBlocks = 8

var cards atomic.Int64 // card ids: unique within this run, the time keeps them apart across runs

// cardOf builds the card from the tool's arguments; error text when there is nothing to show.
func cardOf(args map[string]any, mode string) (card, string) {
	c := card{Mode: mode, Title: strings.TrimSpace(tools.String(args, "title")),
		Subtitle: strings.TrimSpace(tools.String(args, "subtitle")),
		Text:     strings.TrimSpace(tools.String(args, "text"))}
	if kind := tools.String(args, "kind"); kinds[kind] {
		c.Kind = kind
	}
	if list, ok := args["blocks"].([]any); ok {
		for _, b := range list {
			if block, ok := b.(map[string]any); ok && len(c.Blocks) < maxBlocks {
				if _, typed := block["type"].(string); typed {
					c.Blocks = append(c.Blocks, block)
				}
			}
		}
	}
	if link := strings.TrimSpace(tools.String(args, "url")); link != "" {
		if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
			return card{}, "Error: url must be an http(s) link"
		}
		switch {
		case c.Kind == "video":
			c.Blocks = append(c.Blocks, map[string]any{"type": "video", "url": link, "wide": true})
		default: // a web page in a frame of the HUD
			c.Kind, c.URL = "web", link
			if mode != "clear" {
				c.Mode = "web"
			}
		}
	}
	if list, ok := args["items"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" && len(c.Items) < maxItems {
				c.Items = append(c.Items, strings.TrimSpace(text))
			}
		}
	}
	if image := strings.TrimSpace(tools.String(args, "image_url")); image != "" {
		if !strings.HasPrefix(image, "https://") && !strings.HasPrefix(image, "http://") {
			return card{}, "Error: image_url must be an http(s) link to an image"
		}
		c.Image = image
	}
	if c.Title == "" && c.Text == "" && len(c.Items) == 0 && c.Image == "" && c.URL == "" && len(c.Blocks) == 0 {
		return card{}, "Error: nothing to show (title, text, items, image_url, url or blocks)"
	}
	if seconds, ok := tools.Int(args, "seconds"); ok && seconds > 0 {
		c.Seconds = min(seconds, maxSeconds)
	}
	c.ID = fmt.Sprintf("%d-%d", time.Now().UnixMilli(), cards.Add(1))
	return c, ""
}

// AddTools registers show_on_screen.
func (s *Service) AddTools(r *tools.Registry) {
	r.Add(tools.Tool{
		Name: "show_on_screen",
		Description: "Shows something on a TV at home (the assistant app on a Fire TV): a card with facts, a list, " +
			"steps, a table, a chart, the weather, a picture; a web page; a video. Whenever the user says " +
			"'zeig mir …' or wants to see something on the TV, or their rules say so. " +
			"mode auto (normally): the TV chooses – the whole screen when nothing plays; while a film plays " +
			"short things stay small in the corner and long ones take the screen and pause the film until " +
			"they close. overlay or fullscreen only when the user asks for it ('im Vollbild', 'nur klein'); " +
			"clear: hide what is shown. url: a web page shown in a frame (kind web), or with kind video a " +
			"video file (mp4, webm, HLS; YouTube goes through play_video). Write in German, short lines, no " +
			"Markdown. Still give a short spoken answer; the card holds the details. A sleeping TV is " +
			"switched on. room: the TV's room, '' = where the user is, 'all' = every TV that is on." +
			prompts.Text("SCREEN_TEMPLATES"),
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"room": {"type": "string", "description": "e.g. 'Wohnzimmer'; '' = the user's room; 'all'"},
                           "mode": {"type": "string", "enum": ["auto", "overlay", "fullscreen", "clear"]},
                           "kind": {"type": "string", "enum": ["info", "list", "recipe", "timer", "weather", "calendar", "message", "alert", "camera", "music", "home", "energy", "route", "package", "search", "choice", "web", "video"], "description": "label and icon of the card"},
                           "title": {"type": "string", "description": "a short heading"},
                           "subtitle": {"type": "string", "description": "one short line under it; '' for none"},
                           "text": {"type": "string", "description": "a few sentences; '' for none"},
                           "items": {"type": "array", "items": {"type": "string"}, "description": "list lines; [] for none"},
                           "image_url": {"type": "string", "description": "an https link to an image; '' for none"},
                           "url": {"type": "string", "description": "a web page, or a video file with kind video; '' for none"},
                           "blocks": {"type": "array", "items": {"type": "object"}, "description": "richer content, [] for none; each {type, label?, wide?, ...}: stats {items:[{label,value,unit,trend}]}; kv {items:[{label,value}]}; table {columns:[], rows:[[]], highlight: row}; chart {kind: line|bar, labels:[], series:[{name, values:[]}], unit}; steps {items:[], current: index}; timeline {items:[{time,title,note,state: past|now}]}; forecast {now:{temp,condition,text,high,low}, items:[{time,condition,temp,low,rain}]} (condition as Home Assistant: sunny, partlycloudy, cloudy, rainy, pouring, snowy, lightning, fog, clear-night); choices {items:[]} (the user answers with the number); timer {label, seconds}; progress {label, value 0..1, text}; media {title, artist, cover}; image {url, caption}; gallery {items:[{url, caption}]} (up to 9 pictures as a grid)"},
                           "seconds": {"type": "integer", "description": "how long it stays; 0 = default (corner 20 s; full screen and web pages until closed or the next card)"}},
            "required": ["room", "mode", "kind", "title", "subtitle", "text", "items", "image_url", "url", "blocks", "seconds"]}`,
		Channels: tools.Everywhere,
		Run:      s.run,
	})
}
