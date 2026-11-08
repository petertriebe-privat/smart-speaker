package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"echo-assistant/internal/assistant"
	"echo-assistant/internal/live"
	"echo-assistant/internal/tools"
)

// The shared state of several Echos (phase 4 of devices/echo/docs/18-several-echos.md, #32): the
// primary device owns memory, tasks and people. A secondary one asks it over the peer link:
//   - the tools that change that state run there (sharedWrite); when it is not reachable they
//     fail, so no second state grows here. The ones that only read (sharedRead) then run on the
//     local copy (restored from the backup at start);
//   - a finished conversation goes there for the memory and the agenda;
//   - the instructions' context (notes, rules, profile, tasks and agenda) comes from there, the
//     local copy when it does not answer in time.

var (
	sharedWrite = []string{"remember", "forget", "save_rule", "delete_rule", "save_skill", "delete_skill",
		"update_profile", "dream", "create_task", "delete_task", "pause_task", "run_task", "add_person",
		"update_person", "remove_person", "send_welcome", "set_humor", "speaker_access", "mail_rule", "update_affair",
		"pause_hints", "affair_trust", "answer_suggestion", "confirm_rule", "log_behaviour"}
	sharedRead = []string{"recall", "list_activities", "open_conversation", "open_topic", "use_skill",
		"list_tasks", "list_people", "list_affairs"}
)

// contextTimeout: the instructions wait this long for the primary's context.
const contextTimeout = 3 * time.Second

type sharedTool struct {
	ID        string        `json:"id,omitempty"` // a replayed journal entry (the M5, #66): run once
	Name      string        `json:"name"`
	Arguments string        `json:"arguments"`
	Channel   tools.Channel `json:"channel"`
	Person    string        `json:"person"`
	Origin    string        `json:"origin"`
	Room      string        `json:"room,omitempty"` // the asking speaker's room (tools.Context.Room)
}

type sharedConversation struct {
	ID         string    `json:"id,omitempty"` // a replayed journal entry (the M5, #66): taken once
	Text       string    `json:"text"`         // with the note on the kind of conversation
	Started    time.Time `json:"started"`
	Person     string    `json:"person"`
	Channel    string    `json:"channel"`
	Conv       string    `json:"conv"`
	Transcript string    `json:"transcript"`
	Stranger   bool      `json:"stranger"`
	Room       string    `json:"room,omitempty"` // the Echo's room where it was held ("" = the M5, or unknown)
}

type sharedContext struct {
	Fn         string `json:"fn"` // notes, rules, profile, planned, tasks
	Person     string `json:"person"`
	Field      string `json:"field,omitempty"`
	WithIDs    bool   `json:"with_ids,omitempty"`
	Stranger   bool   `json:"stranger,omitempty"`
	Phone      bool   `json:"phone,omitempty"`
	Voice      bool   `json:"voice,omitempty"`
	Conv       string `json:"conv,omitempty"`       // closing
	Transcript string `json:"transcript,omitempty"` // closing
}

// agendaClosing: what the assistant still mentions before hanging up; the primary keeps what was said
// (agenda_said.json), so a secondary asks it and nothing is said twice.
func (d *device) agendaClosing(conv, transcript string) string {
	local := func() string { return d.svc.agenda.Closing(conv, transcript) }
	if d.peers == nil || d.cfg.Settings.Primary {
		return local()
	}
	return d.remoteText(sharedContext{Fn: "closing", Conv: conv, Transcript: transcript}, local)
}

type sharedText struct {
	Text string `json:"text"`
}

var errNotPrimary = errors.New("not the primary device")

// setupShared: the primary answers the others; a secondary asks the primary.
func (d *device) setupShared() {
	if d.peers == nil {
		return
	}
	d.peers.Handle("/tool", d.serveTool)
	d.peers.Handle("/conversation", d.serveConversation)
	d.peers.Handle("/context", d.serveContext)
	d.peers.Handle("/state", d.serveState)              // failover.go
	d.peers.Handle("/handover", d.serveHandover)        // failover.go
	d.peers.Handle("/google_login", d.serveGoogleLogin) // failover.go, for a member M5
	if d.cfg.Settings.Primary {
		return
	}
	log.Print("[Shared] memory, tasks and people are the primary device's")
	d.registry.SetRemote(d.remoteTool, sharedWrite, sharedRead)
	notes, rules, profile, tasksCtx := assistant.Notes, assistant.Rules, assistant.Profile, d.svc.agent.Tasks
	assistant.Notes = func(withIDs bool, person string) string {
		return d.remoteText(sharedContext{Fn: "notes", WithIDs: withIDs, Person: person}, func() string { return notes(withIDs, person) })
	}
	assistant.Rules = func(person string, withIDs bool) string {
		return d.remoteText(sharedContext{Fn: "rules", WithIDs: withIDs, Person: person}, func() string { return rules(person, withIDs) })
	}
	assistant.Profile = func(field, person string) string {
		return d.remoteText(sharedContext{Fn: "profile", Field: field, Person: person}, func() string { return profile(field, person) })
	}
	assistant.Planned = func(req live.Request, voice bool) string {
		q := sharedContext{Fn: "planned", Person: req.Person, Stranger: req.Stranger, Phone: req.Phone, Voice: voice}
		return d.remoteText(q, func() string { return d.planned(req.Person, req.Stranger, req.Phone, voice) })
	}
	d.svc.agent.Notes, d.svc.agent.Rules = assistant.Notes, assistant.Rules
	d.svc.agent.Tasks = func() string {
		return d.remoteText(sharedContext{Fn: "tasks"}, tasksCtx)
	}
}

// remoteTool: a shared tool on the primary.
func (d *device) remoteTool(name, arguments string, ctx tools.Context) (string, error) {
	body, _ := json.Marshal(sharedTool{Name: name, Arguments: arguments, Channel: ctx.Channel, Person: ctx.Person,
		Origin: ctx.Origin, Room: ctx.Room})
	out, err := d.peers.CallPrimary(context.Background(), "/tool", body)
	if err != nil {
		log.Printf("[Shared] %s on the primary: %v", name, err)
		return "", err
	}
	var a sharedText
	if err := json.Unmarshal(out, &a); err != nil {
		return "", err
	}
	log.Printf("[Shared] %s ran on the primary", name)
	return a.Text, nil
}

// remoteText: context from the primary, else local().
func (d *device) remoteText(q sharedContext, local func() string) string {
	if d.peers.Leader() == nil {
		return local()
	}
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()
	body, _ := json.Marshal(q)
	out, err := d.peers.CallPrimary(ctx, "/context", body)
	var a sharedText
	if err == nil {
		err = json.Unmarshal(out, &a)
	}
	if err != nil {
		log.Printf("[Shared] context %s from the primary: %v (the local copy instead)", q.Fn, err)
		return local()
	}
	return a.Text
}

// shareConversation: on a secondary, a finished conversation goes to the primary; false = keep it
// here (this is the primary, or it is not reachable: better here than lost).
func (d *device) shareConversation(c sharedConversation) bool {
	if d.peers == nil || d.cfg.Settings.Primary || c.Transcript == "" {
		return false
	}
	body, _ := json.Marshal(c)
	if _, err := d.peers.CallPrimary(context.Background(), "/conversation", body); err != nil {
		log.Printf("[Shared] conversation to the primary: %v (kept here)", err)
		return false
	}
	log.Print("[Shared] conversation handed to the primary")
	return true
}

// --- The primary's side ------------------------------------------------------------------------

func (d *device) serveTool(body []byte) ([]byte, error) {
	if !d.cfg.Settings.Primary {
		return nil, errNotPrimary
	}
	var q sharedTool
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	if out, ok := replays.answered(q.ID); ok {
		log.Printf("[Shared] %s for another device: already done (%s)", q.Name, q.ID)
		return out, nil
	}
	log.Printf("[Shared] %s for another device", q.Name)
	out := d.registry.Call(q.Name, q.Arguments, tools.Context{Channel: q.Channel, Person: q.Person, Origin: q.Origin,
		Room: q.Room})
	answer, err := json.Marshal(sharedText{Text: out})
	if err == nil {
		replays.done(q.ID, answer)
	}
	return answer, err
}

// replays: the answers to the last journal entries a device replayed (ids), so an entry whose
// answer got lost runs once when it comes again.
var replays = &replayed{answers: map[string][]byte{}}

type replayed struct {
	mu      sync.Mutex
	answers map[string][]byte
	order   []string
}

const replaysKept = 200

func (r *replayed) answered(id string) ([]byte, bool) {
	if id == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out, ok := r.answers[id]
	return out, ok
}

func (r *replayed) done(id string, answer []byte) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.answers[id]; !ok {
		r.order = append(r.order, id)
	}
	r.answers[id] = answer
	for len(r.order) > replaysKept {
		delete(r.answers, r.order[0])
		r.order = r.order[1:]
	}
}

func (d *device) serveConversation(body []byte) ([]byte, error) {
	if !d.cfg.Settings.Primary {
		return nil, errNotPrimary
	}
	var c sharedConversation
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	if _, ok := replays.answered(c.ID); ok {
		log.Printf("[Shared] a conversation from another device: already taken (%s)", c.ID)
		return []byte("{}"), nil
	}
	log.Print("[Shared] a conversation from another device")
	replays.done(c.ID, []byte("{}"))
	go d.conversationOver(c)
	return []byte("{}"), nil
}

func (d *device) serveContext(body []byte) ([]byte, error) {
	if !d.cfg.Settings.Primary {
		return nil, errNotPrimary
	}
	var q sharedContext
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, err
	}
	m := d.svc.memory
	var text string
	switch q.Fn {
	case "notes":
		text = m.Context(q.WithIDs, q.Person)
	case "rules":
		text = m.Rules(q.Person, q.WithIDs)
	case "profile":
		text = m.ProfileValue(q.Field, q.Person)
	case "planned":
		text = d.planned(q.Person, q.Stranger, q.Phone, q.Voice)
	case "tasks":
		text = d.svc.tasks.Context()
	case "closing":
		if d.svc.agenda != nil {
			text = d.svc.agenda.Closing(q.Conv, q.Transcript)
		}
	default:
		return nil, errors.New("unknown context " + q.Fn)
	}
	return json.Marshal(sharedText{Text: text})
}
