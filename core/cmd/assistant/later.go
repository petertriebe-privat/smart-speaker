package main

import (
	"log"
	"strings"
	"time"

	"echo-assistant/internal/agent"
	"echo-assistant/internal/live"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Notifications on the phone and research that comes back by itself (#75).
//
//   - notify_phone: a card as a notification on the user's phone (the phone app, also on the car's
//     screen); a tap opens it in the assistant's console. From any the assistant: the phones take it over MQTT
//     (screen.Notify), their screen on or off.
//   - research_later: a long research runs in the background instead of a silence in the
//     conversation. The assistant says it will come back and ends the conversation (the music plays on);
//     when it is done he starts a conversation himself where it was asked (the music pauses) and
//     tells the result, and the phones get it as a notification.

func (d *device) addLaterTools() {
	d.registry.Add(tools.Tool{
		Name: "notify_phone",
		Description: "Sends the user a notification to their phone (the assistant's app; in the car on its screen too): a " +
			"title, a short text for the notification itself, and the card a tap opens, with its kind and blocks " +
			"as for show_on_screen. For what the user wants to have on the phone („schick mir das aufs Handy“). " +
			"Longer texts, files and links go better by chat (message)." + prompts.Text("SCREEN_TEMPLATES"),
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"title": {"type": "string"}, "text": {"type": "string", "description": "one to three sentences"},
                           "kind": {"type": "string", "description": "the card's kind as for show_on_screen (info, list, weather, calendar, route, search …)"},
                           "blocks": {"type": "array", "items": {"type": "object"}, "description": "optional: the card's blocks as for show_on_screen"}},
            "required": ["title", "text"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, ctx tools.Context) string {
			card := map[string]any{"kind": "info", "title": tools.String(args, "title"), "text": tools.String(args, "text")}
			if k := tools.String(args, "kind"); k != "" {
				card["kind"] = k
			}
			if b, ok := args["blocks"].([]any); ok && len(b) > 0 {
				card["blocks"] = b
			}
			if n := d.svc.screens.Notify(card); n == 0 {
				return "Error: no phone with the assistant's app is online"
			}
			return "Sent to the phone"
		},
	})
	d.registry.Add(tools.Tool{
		Name: "research_later",
		Description: "Runs a research or task that takes longer (several searches, comparing sources, a summary) " +
			"in the background instead of keeping the user waiting: tell them in one sentence that you start and " +
			"will come back, then end the conversation (their music plays on). When it is done the assistant starts a " +
			"conversation by itself and tells the result, and it also goes to the phone as a notification. " +
			"Give the complete task in German with everything the user said that matters.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"task": {"type": "string", "description": "the complete task in German"},
                           "title": {"type": "string", "description": "a short title for the notification"}},
            "required": ["task"]}`,
		Channels: tools.Voice | tools.Console,
		Run: func(args map[string]any, ctx tools.Context) string {
			task := strings.TrimSpace(tools.String(args, "task"))
			if task == "" {
				return "Error: the task is missing"
			}
			if d.svc.agent == nil {
				return "Error: no backend"
			}
			title := tools.String(args, "title")
			if title == "" {
				title = "Recherche fertig"
			}
			go d.researchLater(task, title, ctx)
			return "Started in the background. Tell the user in one sentence that you will come back with the result, " +
				"then end the conversation."
		},
	})
}

func (d *device) researchLater(task, title string, ctx tools.Context) {
	started := time.Now()
	out := d.svc.agent.Run(task, agent.Options{Instructions: thinkInstructions(), Person: ctx.Person, Origin: ctx.Origin,
		Effort: "high", Exclude: []string{"think_harder", "research_later"}})
	log.Printf("[Later] %.80q: %d s", task, int(time.Since(started).Seconds()))
	failed := strings.HasPrefix(out, "Error")
	if !failed {
		d.svc.screens.Notify(map[string]any{"kind": "search", "title": title, "text": out})
	}
	reason := "Die Recherche, um die der Nutzer vorhin gebeten hat, ist fertig. Aufgabe: " + task +
		"\nErgebnis:\n" + out + "\nMelde dich mit dem Ergebnis, kurz und in deinen Worten; das Ergebnis liegt auch als " +
		"Benachrichtigung auf dem Handy."
	if failed {
		reason = "Die Recherche, um die der Nutzer vorhin gebeten hat, ist leider gescheitert (" + out + "). Sag es ihm kurz."
	}
	d.requestConversation(live.Request{Reason: reason, Person: ctx.Person})
}
