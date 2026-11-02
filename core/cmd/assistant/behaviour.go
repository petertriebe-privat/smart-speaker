package main

import (
	"encoding/json"
	"sort"
	"strings"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// The behaviour log (internal/memory/behaviour.go): what people did, for the dream's habits.
// Quick commands (endQuick), the tools in BEHAVIOUR_TOOLS that ran for someone, and arrivals and
// departures (the primary's presence). A secondary Echo sends its entries to the primary
// (log_behaviour is a shared tool).

const behaviourTools = "play_music,music_control,play_video,control_video,set_timer,alexa,HassTurnOn,HassTurnOff," +
	"HassLightSet,HassClimateSetTemperature,HassSetPosition,HassMediaPause,HassMediaUnpause,HassMediaNext"

func (d *device) setupBehaviour() {
	s := &d.svc
	s.memory.AddBehaviourTool(d.registry)
	watched := map[string]bool{}
	for _, name := range strings.Split(prompts.TextOr("BEHAVIOUR_TOOLS", behaviourTools), ",") {
		watched[strings.TrimSpace(name)] = true
	}
	d.registry.Observe = func(name, arguments string, ctx tools.Context, _ string) {
		// Console calls are the device's own (quick commands log themselves) or tests.
		if !watched[name] || ctx.Channel == tools.Console || ctx.Channel == tools.Stranger {
			return
		}
		room := ""
		if ctx.Origin == "Lautsprecher" {
			room = d.cfg.Settings.Room
		}
		what := name
		if args := compactArgs(arguments); args != "" {
			what += " " + args
		}
		if ctx.Origin != "" && ctx.Origin != "Lautsprecher" {
			what += " (" + ctx.Origin + ")"
		}
		d.logBehaviour(ctx.Person, "Tool", what, room)
	}
}

// presenceChanged: the primary's presence saw someone come home or leave.
func (d *device) presenceChanged(person, state string) {
	what := "geht weg"
	if state == "home" {
		what = "kommt nach Hause"
	}
	d.svc.memory.LogBehaviour(person, "Anwesenheit", what, "")
}

// logBehaviour notes an entry, on the primary (log_behaviour is shared); never blocks the caller.
func (d *device) logBehaviour(person, kind, what, room string) {
	args, _ := json.Marshal(map[string]string{"person": person, "kind": kind, "what": what, "room": room})
	go d.registry.Call("log_behaviour", string(args), tools.Context{Channel: tools.Console, Origin: "Konsole"})
}

// compactArgs: a tool's arguments as short "key=value" pairs, empty values left out.
func compactArgs(arguments string) string {
	var args map[string]any
	if json.Unmarshal([]byte(arguments), &args) != nil {
		return ""
	}
	var parts []string
	for _, key := range sortedKeys(args) {
		v := args[key]
		switch x := v.(type) {
		case string:
			if x == "" {
				continue
			}
			parts = append(parts, key+"="+x)
		case bool:
			if x {
				parts = append(parts, key)
			}
		case nil:
		default:
			raw, _ := json.Marshal(x)
			parts = append(parts, key+"="+string(raw))
		}
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
