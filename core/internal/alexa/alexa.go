// Package alexa: the Amazon Echo speakers through Home Assistant's official Alexa Devices
// integration (alexa_devices), as devices/atoms3r/src/services/alexa.cpp. Volume and mute come from
// HA's MCP tools (HassSetVolume, ...); this adds what MCP does not offer: a text command to Alexa
// (music, pause, next track, timers on the Echo) and making an Echo speak or announce.
// Tool: alexa (slow: HA passes it on to Amazon's cloud).
package alexa

import (
	"encoding/json"
	"strings"

	"echo-assistant/internal/homeassistant"
	"echo-assistant/internal/tools"
)

// The available Echos (media players of alexa_devices) with room, device id and their notify
// entities for speaking and announcing; the entity ids end in the German entity names.
const echosTemplate = "{% set ns = namespace(l=[]) %}" +
	"{% for e in integration_entities('alexa_devices') | select('match', 'media_player[.]')" +
	" | reject('is_state', 'unavailable') %}" +
	"{% set d = device_id(e) %}" +
	"{% set n = device_entities(d) | select('match', 'notify[.]') | list %}" +
	"{% set ns.l = ns.l + [{'name': state_attr(e, 'friendly_name'), 'area': area_name(e) or '', 'device': d," +
	" 'speak': n | select('search', '_sprechen$') | first | default('')," +
	" 'announce': n | select('search', '_durchsagen$') | first | default('')}] %}" +
	"{% endfor %}{{ {'echos': ns.l} | tojson }}"

// Service is the alexa tool.
type Service struct {
	ha *homeassistant.HA
}

// New creates the service on a Home Assistant connection.
func New(ha *homeassistant.HA) *Service { return &Service{ha: ha} }

type echo struct {
	Name     string `json:"name"`
	Area     string `json:"area"`
	Device   string `json:"device"`
	Speak    string `json:"speak"`
	Announce string `json:"announce"`
}

func lower(text string) string { return strings.ToLower(strings.TrimSpace(text)) }

func names(echos []echo) string {
	parts := make([]string, len(echos))
	for i, e := range echos {
		parts[i] = e.Name
		if e.Area != "" {
			parts[i] += " (" + e.Area + ")"
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Service) loadEchos() ([]echo, string) {
	status, body, err := s.ha.Rest("POST", "/api/template", map[string]any{"template": echosTemplate})
	if err != nil || status != 200 {
		return nil, homeassistant.Failure(status, body, err)
	}
	var response struct {
		Echos []echo `json:"echos"`
	}
	json.Unmarshal(body, &response)
	if len(response.Echos) == 0 {
		return nil, "Error: Home Assistant reports no reachable Echos (integration Alexa Devices)."
	}
	return response.Echos, ""
}

// find returns the Echo meant by wanted: its exact name, else the only one in that room or with
// the name part (an "Echo ..." before speaker groups and Fire TVs). Error text otherwise.
func find(echos []echo, wanted string) (*echo, string) {
	key := lower(wanted)
	for i := range echos {
		if lower(echos[i].Name) == key {
			return &echos[i], ""
		}
	}
	var hits, echoHits []echo
	for _, e := range echos {
		if key == "" || (lower(e.Area) != key && !strings.Contains(lower(e.Name), key)) {
			continue
		}
		hits = append(hits, e)
		if strings.HasPrefix(lower(e.Name), "echo") {
			echoHits = append(echoHits, e)
		}
	}
	if len(hits) == 1 {
		return &hits[0], ""
	}
	if len(echoHits) == 1 {
		return &echoHits[0], ""
	}
	switch {
	case key == "":
		return nil, "Error: device missing. Reachable devices: " + names(echos)
	case len(hits) == 0:
		return nil, "Error: no device \"" + wanted + "\". Reachable devices: " + names(echos)
	}
	return nil, "Error: several devices match \"" + wanted + "\": " + names(hits) + ". Name one."
}

// call runs a HA service; "" on success, else the error text.
func (s *Service) call(service string, data map[string]any) string {
	status, body, err := s.ha.Rest("POST", "/api/services/"+service, data)
	if err == nil && status == 200 {
		return ""
	}
	return homeassistant.Failure(status, body, err)
}

func (s *Service) run(args map[string]any, _ tools.Context) string {
	action, text := tools.String(args, "action"), tools.String(args, "text")
	if text == "" {
		return "Error: text missing"
	}
	echos, errText := s.loadEchos()
	if errText != "" {
		return errText
	}
	e, errText := find(echos, tools.String(args, "device"))
	if e == nil {
		return errText
	}
	if action == "command" {
		if errText := s.call("alexa_devices/send_text_command",
			map[string]any{"device_id": e.Device, "text_command": text}); errText != "" {
			return errText
		}
		return "An " + e.Name + " geschickt: " + text
	}
	if action != "speak" && action != "announce" {
		return "Error: action is command, speak or announce"
	}
	entity := e.Announce
	if action == "speak" {
		entity = e.Speak
	}
	if entity == "" {
		extra := ""
		if action == "speak" {
			extra = " (announce only)"
		}
		return "Error: " + e.Name + " cannot do that" + extra
	}
	if errText := s.call("notify/send_message",
		map[string]any{"entity_id": entity, "message": text}); errText != "" {
		return errText
	}
	if action == "speak" {
		return e.Name + " says: " + text
	}
	return e.Name + " announces: " + text
}

// AddTools registers the alexa tool (the M5's addTools(); the caller skips it without HA).
func (s *Service) AddTools(r *tools.Registry) {
	if !s.ha.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "alexa",
		Description: "Controls the Amazon Echo speakers (Alexa) at home through Home Assistant. action command sends text to " +
			"the Echo as if the user had said it to Alexa: radio ('spiel Radio Bob'), Spotify's own mixes (Mix der " +
			"Woche, Daily Mix), a timer or alarm on an Alexa Echo the user names (one ringing here is set_timer). Not for other music, podcasts or controlling what " +
			"plays (the Spotify tools), not for lights or other smart home devices (Home Assistant tools), not for " +
			"the volume (Home Assistant) and not for Fire TVs (fire_tv). action speak makes the " +
			"Echo say text, announce plays it as an announcement with a chime. device: the Echo's name or its room; " +
			"an unclear device returns the list.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"device": {"type": "string", "description": "e.g. 'Echo Wohnzimmer' or 'Wohnzimmer'"},
                           "action": {"type": "string", "enum": ["command", "speak", "announce"]},
                           "text": {"type": "string", "description": "German: the Alexa command without 'Alexa', or the sentence to say"}},
            "required": ["device", "action", "text"]}`,
		Channels: tools.Everywhere,
		Run:      s.run,
	})
}
