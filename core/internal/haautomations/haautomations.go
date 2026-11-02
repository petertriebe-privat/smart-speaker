// Package haautomations: automations in Home Assistant, created by the assistant (tools
// create_home_automation, list_home_automations, delete_home_automation, find_entities), as
// devices/atoms3r/src/services/ha_automations.cpp. Device control on a schedule or on sensors
// belongs there: HA runs it locally, also when the assistant or the internet is down. Uses HA's config
// API (/api/config/automation/config/<id>, as the HA UI does; needs an admin token). The assistant only
// touches automations whose id starts with idPrefix() ("jarvis_" by default), never the user's. To make the assistant speak or
// act, an automation fires the events <slug>_announce / <slug>_task / <slug>_conversation (internal/haevents).
package haautomations

import (
	"echo-assistant/internal/prompts"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"echo-assistant/internal/homeassistant"
	"echo-assistant/internal/tools"
)

const (
	entityHits = 40
)

// Service holds the four automation tools.
type Service struct {
	ha *homeassistant.HA
}

// New creates the service on a Home Assistant connection.
func New(ha *homeassistant.HA) *Service { return &Service{ha: ha} }

type state struct {
	EntityID   string `json:"entity_id"`
	State      string `json:"state"`
	Attributes struct {
		ID            string `json:"id"`
		FriendlyName  string `json:"friendly_name"`
		LastTriggered string `json:"last_triggered"`
	} `json:"attributes"`
}

// states: the states of one domain through HA's template (small, #85); "" = all of /api/states
// (find_entities without a domain, now and then).
func (s *Service) states(domain string) ([]state, string) {
	status, body, err := 0, []byte(nil), error(nil)
	if domain != "" {
		status, body, err = s.ha.DomainStates(domain)
	} else {
		status, body, err = s.ha.Rest("GET", "/api/states", nil)
	}
	if err != nil || status != 200 {
		return nil, homeassistant.Failure(status, body, err)
	}
	var list []state
	json.Unmarshal(body, &list)
	return list, ""
}

func (s *Service) create(args map[string]any) string {
	alias := tools.String(args, "alias")
	if alias == "" {
		return "Error: give an alias"
	}
	raw, _ := args["config"].(string)
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
		return "Error: config is not a JSON object"
	}
	_, hasTriggers := config["triggers"].([]any)
	_, hasActions := config["actions"].([]any)
	if !hasTriggers || !hasActions {
		return "Error: config needs triggers and actions (lists)"
	}
	id := fmt.Sprintf("%s%d", idPrefix(), time.Now().Unix())
	description, _ := args["description"].(string)
	config["id"] = id
	config["alias"] = prompts.Name() + ": " + alias
	config["description"] = description + " (created by the assistant)"
	if _, ok := config["mode"].(string); !ok {
		config["mode"] = "single"
	}
	status, body, err := s.ha.Rest("POST", "/api/config/automation/config/"+id, config)
	payload, _ := json.Marshal(config)
	log.Printf("[Automations] %s: HTTP %d %s", id, status, payload)
	if err != nil || status != 200 {
		return homeassistant.Failure(status, body, err) + ". Check the config and try again."
	}
	return "Created: \"" + prompts.Name() + ": " + alias + "\" (" + id + "). Home Assistant runs it by itself from now on."
}

func (s *Service) list() string {
	states, errText := s.states("automation")
	if errText != "" {
		return errText
	}
	var text strings.Builder
	for _, e := range states {
		if !strings.HasPrefix(e.EntityID, "automation.") || !strings.HasPrefix(e.Attributes.ID, idPrefix()) {
			continue
		}
		active := "aus"
		if e.State == "on" {
			active = "aktiv"
		}
		fmt.Fprintf(&text, "- %s [%s], %s", e.Attributes.FriendlyName, e.Attributes.ID, active)
		if last := e.Attributes.LastTriggered; last != "" {
			if len(last) > 16 {
				last = last[:16]
			}
			text.WriteString(", last " + last + "\n")
		} else {
			text.WriteString(", never triggered\n")
		}
	}
	if text.Len() == 0 {
		return "No automations by the assistant in Home Assistant."
	}
	return text.String()
}

func (s *Service) remove(id string) string {
	if !strings.HasPrefix(id, idPrefix()) {
		return "Error: The assistant deletes only its own automations (" + idPrefix() + "...)"
	}
	status, body, err := s.ha.Rest("DELETE", "/api/config/automation/config/"+id, nil)
	if err != nil || status != 200 {
		return homeassistant.Failure(status, body, err)
	}
	return "Automation " + id + " deleted."
}

// findEntities gives entity_ids for automations: GetLiveContext gives names, HA's config needs ids.
func (s *Service) findEntities(query, domain string) string {
	states, errText := s.states(domain)
	if errText != "" {
		return errText
	}
	wanted := strings.ToLower(query)
	var text strings.Builder
	hits := 0
	for _, e := range states {
		if domain != "" && !strings.HasPrefix(e.EntityID, domain+".") {
			continue
		}
		name := e.Attributes.FriendlyName
		if wanted != "" && !strings.Contains(strings.ToLower(e.EntityID+" "+name), wanted) {
			continue
		}
		if hits++; hits > entityHits {
			text.WriteString("… (mehr Treffer, genauer suchen)\n")
			break
		}
		fmt.Fprintf(&text, "%s = %s (%s)\n", e.EntityID, name, e.State)
	}
	if text.Len() == 0 {
		return "No matching entities."
	}
	return text.String()
}

// AddTools registers the four tools (nothing without Home Assistant, as on the M5).
func (s *Service) AddTools(r *tools.Registry) {
	if !s.ha.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "create_home_automation",
		Description: "Creates a permanent automation in Home Assistant, after the user confirmed it: recurring device " +
			"control at a time, at sunrise/sunset or when a sensor or device changes. HA runs it locally from " +
			"then on; a time trigger fires every day. Never for one-off or delayed actions ('in a minute', " +
			"'tomorrow once'): use create_task. config is the automation as JSON in Home Assistant's format " +
			"with triggers, optional conditions, actions and mode, e.g. " +
			"{\"triggers\":[{\"trigger\":\"time\",\"at\":\"19:00:00\"}],\"conditions\":[]," +
			"\"actions\":[{\"action\":\"light.turn_on\",\"target\":{\"entity_id\":\"light.x\"}}]," +
			"\"mode\":\"single\"}. Sunset: {\"trigger\":\"sun\",\"event\":\"sunset\",\"offset\":\"-00:15:00\"}; " +
			"state: {\"trigger\":\"state\",\"entity_id\":\"...\",\"to\":\"off\",\"for\":\"00:05:00\"}; weekdays: " +
			"condition {\"condition\":\"time\",\"weekday\":[\"mon\",\"tue\"]}. Get entity_ids with find_entities " +
			"first. To make " + prompts.Name() + " speak: action {\"event\":\"" + prompts.Slug() + "_announce\",\"event_data\":{\"text\":\"...\"}}; " +
			"to make him think and act (search, mail, summarize): {\"event\":\"" + prompts.Slug() + "_task\"," +
			"\"event_data\":{\"title\":\"...\",\"instruction\":\"...\",\"deliver\":\"speak\"}} " +
			"(deliver: speak, email, both, telegram, none); to make him talk WITH whoever is there (greet them " +
			"and ask, wait for the answer): {\"event\":\"" + prompts.Slug() + "_conversation\",\"event_data\":{\"text\":\"...\"}} " +
			"(text: the occasion and what to say or ask, in German; optional person: a household name; " +
			"insist: true keeps at it until someone answers). " + prompts.Slug() + "_announce and " + prompts.Slug() + "_conversation take an " +
			"optional room (a assistant speaker's room) to speak there. The assistant speakers' night mode (their " +
			"night volume, the ring dimmed) is switch." + prompts.Slug() + "_<room>_nachtmodus: one automation with a trigger " +
			"id 'on' and one 'off' and a choose on the trigger. To change when it runs (\"Nachtmodus zwischen 22 " +
			"und 7 Uhr\", \"ab Sonnenuntergang\"), delete the existing night mode automation first " +
			"(list_home_automations), so two never contradict each other.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {
              "alias": {"type": "string", "description": "short German name"},
              "description": {"type": "string", "description": "what it does, in German"},
              "config": {"type": "string", "description": "the automation as a JSON object (string)"}},
            "required": ["alias", "description", "config"]}`,
		Channels: tools.Everywhere,
		Run:      func(args map[string]any, _ tools.Context) string { return s.create(args) },
	})
	r.Add(tools.Tool{
		Name:        "list_home_automations",
		Description: "Lists the automations the assistant created in Home Assistant (name, id, active, last run).",
		Parameters:  `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:    tools.Everywhere,
		Run:         func(map[string]any, tools.Context) string { return s.list() },
	})
	r.Add(tools.Tool{
		Name:        "delete_home_automation",
		Description: "Deletes an automation the assistant created (id " + idPrefix() + "...).",
		Parameters:  `{"type": "object", "additionalProperties": false, "properties": {"id": {"type": "string"}}, "required": ["id"]}`,
		Channels:    tools.Everywhere,
		Run:         func(args map[string]any, _ tools.Context) string { return s.remove(tools.String(args, "id")) },
	})
	r.Add(tools.Tool{
		Name: "find_entities",
		Description: "Finds Home Assistant entity_ids (light.*, switch.*, sensor.*, binary_sensor.*, ...) by name, for " +
			"create_home_automation.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {
              "query": {"type": "string", "description": "part of the name or id, e.g. 'wohnzimmer'; '' for all of a domain"},
              "domain": {"type": "string", "description": "optional: light, switch, sensor, binary_sensor, ...; '' for all"}},
            "required": ["query"]}`,
		Channels: tools.Everywhere,
		Run: func(args map[string]any, _ tools.Context) string {
			return s.findEntities(tools.String(args, "query"), tools.String(args, "domain"))
		},
	})
}

// idPrefix: the ids of the automations the assistant made (jarvis_ by default).
func idPrefix() string { return prompts.Slug() + "_" }
