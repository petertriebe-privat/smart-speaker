// Package homeassistant: Home Assistant as MCP server ("Model Context Protocol Server"
// integration) in the LAN, as devices/atoms3r/src/services/home_assistant.cpp. The assistant loads HA's
// tools, offers them to the backend model as function tools and forwards calls to HA. The
// server is stateless: every request is a single JSON-RPC POST to /api/mcp.
package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/mcp"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

type HA struct {
	url, token string
	client     *mcp.Client
	rest       *http.Client

	mu    sync.Mutex
	tools []mcp.Tool
}

func New(url, token string) *HA {
	url = strings.TrimRight(url, "/")
	c := mcp.NewClient(url+"/api/mcp", token, 8*time.Second)
	c.Stateless = true
	return &HA{url: url, token: token, client: c, rest: netx.Client(8 * time.Second)}
}

func (h *HA) Configured() bool { return h.url != "" && h.token != "" }

// Run loads the tools, retrying every HA_RETRY_MS until it works.
func (h *HA) Run() {
	retry := time.Duration(prompts.Int("HA_RETRY_MS", 30000)) * time.Millisecond
	for {
		if err := h.load(); err == nil {
			return
		} else {
			log.Printf("[Home Assistant] tools not loaded: %v", err)
		}
		time.Sleep(retry)
	}
}

func (h *HA) load() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list, err := h.client.ListTools(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.tools = list
	h.mu.Unlock()
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = t.Name
	}
	log.Printf("[Home Assistant] tools: %s", strings.Join(names, " "))
	return nil
}

func (h *HA) Loaded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tools) > 0
}

// Provider for the tool registry.
func (h *HA) Provider() tools.Provider {
	return tools.Provider{
		Name: "home_assistant",
		Describe: func(tools.Context) []any {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make([]any, 0, len(h.tools))
			for _, t := range h.tools {
				// date and time are in every instruction: the backend asked HA first anyway, a
				// whole round for nothing ("Was steht morgen im Kalender?", ctl eval 18.10.2026)
				if strings.HasSuffix(t.Name, "GetDateTime") {
					continue
				}
				out = append(out, tools.Function(t.Name, t.Description, t.InputSchema))
			}
			return out
		},
		Has: func(name string) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, t := range h.tools {
				if t.Name == name {
					return true
				}
			}
			return false
		},
		Call: func(name string, args map[string]any, _ tools.Context) string { return h.CallTool(name, args) },
	}
}

// CallTool forwards a call. HA's schemas mark nothing as required, so the backend model fills
// every slot with an empty value ("floor": "", "device_class": [], "temperature": 0); HA rejects
// those as "invalid slot info": pass only the slots that carry a value.
func (h *HA) CallTool(name string, args map[string]any) string {
	cleaned := map[string]any{}
	for k, v := range args {
		switch x := v.(type) {
		case nil:
			continue
		case string:
			if x == "" {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		case map[string]any:
			if len(x) == 0 {
				continue
			}
		case float64:
			if k == "temperature" && x == 0 {
				continue
			}
		}
		cleaned[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	text, err := h.client.CallTool(ctx, name, cleaned)
	if err != nil {
		return "Error: " + err.Error()
	}
	return checkBroadcast(name, text)
}

// checkBroadcast: HA's broadcast answers "action_done" also when no satellite took it
// (data.success empty). The model then said "done" and nothing was heard (16.10.2026).
func checkBroadcast(name, text string) string {
	if !strings.HasSuffix(name, "HassBroadcast") {
		return text
	}
	var r struct {
		Data struct {
			Success []any `json:"success"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(text), &r) == nil && len(r.Data.Success) == 0 {
		return "Error: no Home Assistant speaker received the broadcast, nothing was heard. For the speaker at home " +
			"(the one the assistant runs on) use speak."
	}
	return text
}

// AssistResult: what HA's own sentence matching did with a text.
type AssistResult struct {
	Type   string // action_done, query_answer or error
	Code   string // for an error: no_intent_match, no_valid_targets, failed_to_handle, ...
	Speech string // HA's answer in its language ("Eingeschaltet")
	Done   int    // targets that did it
	Failed int    // targets that failed
}

// Assist hands a sentence to HA's built-in conversation agent ("Licht in der Küche aus"): HA's
// own sentence matching in the LAN, which knows the house's areas, names and aliases, so no
// household data is needed here. The built-in agent and not the default one, which may be an
// LLM. It acts at once when it matches: give it only what is meant as a command.
func (h *HA) Assist(ctx context.Context, text string) (AssistResult, error) {
	status, body, err := h.restCtx(ctx, "POST", "/api/conversation/process",
		map[string]any{"text": text, "agent_id": "conversation.home_assistant"})
	if err != nil {
		return AssistResult{}, err
	}
	if status != http.StatusOK {
		return AssistResult{}, fmt.Errorf("HTTP %d", status)
	}
	var r struct {
		Response struct {
			ResponseType string `json:"response_type"`
			Speech       struct {
				Plain struct {
					Speech string `json:"speech"`
				} `json:"plain"`
			} `json:"speech"`
			Data struct {
				Code    string `json:"code"`
				Success []any  `json:"success"`
				Failed  []any  `json:"failed"`
			} `json:"data"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return AssistResult{}, err
	}
	return AssistResult{Type: r.Response.ResponseType, Code: r.Response.Data.Code, Speech: r.Response.Speech.Plain.Speech,
		Done: len(r.Response.Data.Success), Failed: len(r.Response.Data.Failed)}, nil
}

// House: what the initiative looks at (#82, step 9), by device class, nothing named in code.
type House struct {
	Open    []string `json:"open"`    // binary sensors of windows, doors, garage doors, openings that are on
	Lights  []string `json:"lights"`  // lights that are on
	Weather string   `json:"weather"` // the first weather entity's state ("rainy", "pouring", ...), "" = none
}

// houseTemplate: HA works it out itself, so only a small answer comes back (not /api/states).
const houseTemplate = `{% set open = states.binary_sensor | selectattr('state', 'eq', 'on')
  | selectattr('attributes.device_class', 'defined')
  | selectattr('attributes.device_class', 'in', ['window', 'door', 'garage_door', 'opening'])
  | map(attribute='name') | list %}
{% set lights = states.light | selectattr('state', 'eq', 'on') | map(attribute='name') | list %}
{% set weather = states.weather | map(attribute='state') | first | default('') %}
{{ {'open': open, 'lights': lights, 'weather': weather} | tojson }}`

// HouseState: the open windows and doors, the lights on and the weather, from one template.
func (h *HA) HouseState() (House, error) {
	status, data, err := h.Rest("POST", "/api/template", map[string]string{"template": houseTemplate})
	if err != nil {
		return House{}, err
	}
	if status != 200 {
		return House{}, fmt.Errorf("HTTP %d", status)
	}
	var house House
	if err := json.Unmarshal(bytes.TrimSpace(data), &house); err != nil {
		return House{}, fmt.Errorf("template answer: %v", err)
	}
	return house, nil
}

// DomainStates: the states of one domain ("person", "automation") as /api/states gives them
// (entity_id, state, last_changed, last_updated, attributes friendly_name, id, last_triggered,
// latitude, longitude, gps_accuracy), filtered by HA through the template API: /api/states had
// grown to ~165 KB, fetched every minute by every device (#85; the M5's network died of it).
// Returns the status and the JSON array.
func (h *HA) DomainStates(domain string) (int, []byte, error) {
	for _, c := range domain {
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return 0, nil, fmt.Errorf("bad domain %q", domain)
		}
	}
	tpl := "{% set ns = namespace(first=true) %}[{% for s in states." + domain + " %}{% set a = namespace(d={}) %}" +
		"{% for k in ['friendly_name', 'id', 'last_triggered', 'latitude', 'longitude', 'gps_accuracy'] %}" +
		"{% set v = s.attributes.get(k) %}{% if v is not none %}" +
		"{% set a.d = dict(a.d, **{k: (v if v is number or v is string or v is boolean else v | string)}) %}" +
		"{% endif %}{% endfor %}{{ '' if ns.first else ',' }}{% set ns.first = false %}" +
		"{{ {'entity_id': s.entity_id, 'state': s.state, 'last_changed': s.last_changed.isoformat(), " +
		"'last_updated': s.last_updated.isoformat(), 'attributes': a.d} | tojson }}{% endfor %}]"
	return h.Rest("POST", "/api/template", map[string]string{"template": tpl})
}

// Rest: one request to HA's REST API (/api/...) for what MCP does not offer (automations,
// service calls, templates). Returns the status and the body.
func (h *HA) Rest(method, path string, payload any) (int, []byte, error) {
	return h.restCtx(context.Background(), method, path, payload)
}

func (h *HA) restCtx(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.rest.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

// Failure: error text for the model from a REST status and body.
func Failure(status int, body []byte, err error) string {
	if err != nil {
		return "Error: Home Assistant not reachable (" + err.Error() + ")"
	}
	var m struct {
		Message string `json:"message"`
	}
	json.Unmarshal(body, &m)
	if m.Message != "" {
		return fmt.Sprintf("Error from Home Assistant (HTTP %d): %s", status, m.Message)
	}
	return fmt.Sprintf("Error from Home Assistant (HTTP %d)", status)
}
