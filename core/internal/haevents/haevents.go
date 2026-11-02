// Package haevents: events from Home Assistant to the assistant, as devices/atoms3r/src/services/
// ha_events.cpp: a permanent WebSocket to HA's API (/api/websocket, admin token) subscribed to
// three event types (<slug>: prompts.Slug(), jarvis by default) any HA automation can fire with the action `event:`:
//
//	<slug>_announce     {text}                         the assistant says the text
//	<slug>_task         {title, instruction, deliver}  the assistant carries out the instruction
//	<slug>_conversation {text, person?, insist?}       the assistant starts a conversation at the speaker
//
// <slug>_announce and <slug>_conversation take an optional room: the assistant speaker there.
//
// Only on the primary device: both the assistant would answer the same event.
package haevents

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/coder/websocket"

	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
)

// The event types, prefixed with the slug of the assistant's name (jarvis_announce by default).
func types() []string {
	p := prompts.Slug()
	return []string{p + "_announce", p + "_task", p + "_conversation"}
}

type Events struct {
	URL, Token string
	// RunNow: tasks.Scheduler.RunNow (kind "reminder" for announcements, "prompt" for tasks).
	RunNow func(kind, title, text, deliver, person string)
	// Conversation: start one at the speaker (person "" = whoever is there).
	Conversation func(text, person string, insist bool) string
	ByName       func(name string) string
	// SpeakIn and ConversationIn: the same on the Jarvis speaker in room (its room or name, "all"
	// for announcements), when the event names one (#65); nil = the room is ignored.
	SpeakIn        func(room, text string) string
	ConversationIn func(room, text, person string, insist bool) string
}

// Run keeps the connection, reconnecting after HA_EVENTS_RETRY_MS. Start as a goroutine.
func (e *Events) Run() {
	url := strings.TrimRight(e.URL, "/")
	switch {
	case strings.HasPrefix(url, "https://"):
		url = "wss://" + url[len("https://"):]
	case strings.HasPrefix(url, "http://"):
		url = "ws://" + url[len("http://"):]
	}
	url += "/api/websocket"
	retry := time.Duration(prompts.Int("HA_EVENTS_RETRY_MS", 10000)) * time.Millisecond
	for {
		if err := e.session(url); err != nil {
			log.Printf("[HA events] %v, reconnecting", err)
		}
		time.Sleep(retry)
	}
}

func (e *Events) session(url string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dial, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dial, url, &websocket.DialOptions{HTTPClient: netx.Client(0)})
	dialCancel()
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)
	send := func(v any) error {
		data, _ := json.Marshal(v)
		c, cc := context.WithTimeout(ctx, 10*time.Second)
		defer cc()
		return conn.Write(c, websocket.MessageText, data)
	}
	// A dead link is noticed and reconnected.
	go func() {
		for ctx.Err() == nil {
			time.Sleep(30 * time.Second)
			c, cc := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(c)
			cc()
			if err != nil {
				cancel()
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg struct {
			Type    string                   `json:"type"`
			ID      int                      `json:"id"`
			Success bool                     `json:"success"`
			Message string                   `json:"message"`
			Error   struct{ Message string } `json:"error"`
			Event   struct {
				EventType string          `json:"event_type"`
				Data      json.RawMessage `json:"data"`
			} `json:"event"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "auth_required":
			send(map[string]any{"type": "auth", "access_token": e.Token})
		case "auth_ok":
			for i, t := range types() {
				send(map[string]any{"id": i + 1, "type": "subscribe_events", "event_type": t})
			}
		case "auth_invalid":
			log.Printf("[HA events] login refused: %s", msg.Message)
		case "result":
			if !msg.Success {
				log.Printf("[HA events] subscription refused: %s", msg.Error.Message)
			} else if msg.ID == len(types()) {
				log.Printf("[HA events] connected (%s)", strings.Join(types(), ", "))
			}
		case "event":
			e.onEvent(msg.Event.EventType, msg.Event.Data)
		}
	}
}

func (e *Events) onEvent(kind string, raw json.RawMessage) {
	log.Printf("[HA event] %s %s", kind, raw)
	var data struct {
		Text        string `json:"text"`
		Title       string `json:"title"`
		Instruction string `json:"instruction"`
		Deliver     string `json:"deliver"`
		Person      string `json:"person"`
		Insist      bool   `json:"insist"`
		Room        string `json:"room"`
	}
	json.Unmarshal(raw, &data)
	switch strings.TrimPrefix(kind, prompts.Slug()+"_") {
	case "announce":
		if data.Room != "" && e.SpeakIn != nil {
			log.Printf("[HA event] %s", e.SpeakIn(data.Room, data.Text))
			return
		}
		e.RunNow("reminder", "Home Assistant", data.Text, "speak", "")
	case "task":
		title, deliver := data.Title, data.Deliver
		if title == "" {
			title = "Home Assistant"
		}
		if deliver == "" {
			deliver = "speak"
		}
		e.RunNow("prompt", title, data.Instruction, deliver, "")
	case "conversation":
		// Whoever is in the room unless the automation names a person of the household.
		if data.Room != "" && e.ConversationIn != nil {
			log.Printf("[HA event] %s", e.ConversationIn(data.Room, data.Text, e.ByName(data.Person), data.Insist))
			return
		}
		log.Printf("[HA event] %s", e.Conversation(data.Text, e.ByName(data.Person), data.Insist))
	}
}
