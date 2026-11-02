package hamqtt

import (
	"encoding/json"
	"testing"
)

func TestPlacement(t *testing.T) {
	var devices []registryDevice
	var areas []registryArea
	json.Unmarshal([]byte(`[{"id":"a1","identifiers":[["mqtt","other"]],"name_by_user":"X","area_id":"kitchen"},
		{"id":"b2","identifiers":[["mqtt","echo1"]],"name_by_user":" Jarvis Küche ","area_id":"kitchen"},
		{"id":"c3","identifiers":[["hue","echo1"]],"area_id":"bath"}]`), &devices)
	json.Unmarshal([]byte(`[{"area_id":"kitchen","name":"Küche"},{"area_id":"bath","name":"Bad"}]`), &areas)
	p, id, ok := placementOf(devices, areas, "echo1")
	if !ok || id != "b2" || p != (Placement{Name: "Jarvis Küche", Area: "Küche"}) {
		t.Fatalf("%+v %s %v", p, id, ok)
	}
	if _, _, ok := placementOf(devices, areas, "echo9"); ok {
		t.Fatal("an unknown device was found")
	}
	devices = nil
	json.Unmarshal([]byte(`[{"id":"b2","identifiers":[["mqtt","echo1"]],"name_by_user":null,"area_id":null}]`), &devices)
	if p, _, ok := placementOf(devices, areas, "echo1"); !ok || p != (Placement{}) {
		t.Fatalf("not set in HA: %+v", p)
	}
}

func TestDiscovery(t *testing.T) {
	d := &Device{ID: "echo1", Name: "Jarvis", Model: "Echo", Version: "abc", SuggestedArea: "Küche"}
	cfg := d.discovery(Entity{Component: "number", Key: "volume", Config: map[string]any{"name": "Lautstärke", "min": 10},
		State: func() string { return "40" }, Command: func(string) {}})
	if cfg["unique_id"] != "echo1_volume" || cfg["state_topic"] != "jarvis/echo1/volume" ||
		cfg["command_topic"] != "jarvis/echo1/volume/set" || cfg["availability_topic"] != "jarvis/echo1/status" ||
		cfg["min"] != 10 || cfg["device"].(map[string]any)["suggested_area"] != "Küche" {
		t.Fatalf("%v", cfg)
	}
	ev := d.discovery(Entity{Component: "event", Key: "events"})
	if ev["state_topic"] != "jarvis/echo1/events" || ev["command_topic"] != nil {
		t.Fatalf("event: %v", ev)
	}
}
