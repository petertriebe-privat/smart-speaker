package main

import (
	"log"
	"time"

	"echo-assistant/internal/hamqtt"
	"echo-assistant/internal/mqtt"
	"echo-assistant/internal/people"
	"echo-assistant/internal/prompts"
)

// The household's assistant in Home Assistant (#65): one device beside the speakers, published by the
// primary (one id, so a new primary takes the same device over). Per person: unasked hints paused
// for today ("Heute nicht, Jarvis" as a switch) and the room of their last conversation at a
// speaker while it is recent, for HA's automations. People added later appear after a restart.

// haAssistantID: the household's assistant as one HA device (jarvis_household by default).
func haAssistantID() string { return prompts.Slug() + "_household" }

func (d *device) setupHAAssistant(url, user, password string) {
	if !d.cfg.Settings.Primary || d.svc.people == nil || d.svc.affairs == nil {
		return
	}
	j := &hamqtt.Device{ID: haAssistantID(), Name: prompts.Name(), Model: "Assistant", Version: version}
	for _, p := range d.svc.people.All() {
		j.Entities = append(j.Entities, d.personEntities(p)...)
	}
	if len(j.Entities) == 0 {
		return
	}
	if err := j.Start(url, user, password); err != nil {
		log.Printf("[HA device] "+prompts.Name()+": %v", err)
		return
	}
	d.haAssistant = j
}

func (d *device) personEntities(p people.Person) []hamqtt.Entity {
	id, name := p.ID, p.Name
	key := mqtt.TopicID(id)
	if key == "" {
		return nil
	}
	book := d.svc.affairs
	return []hamqtt.Entity{
		{Component: "switch", Key: "hints_paused_" + key, Config: map[string]any{
			"name": "Hinweise pausiert " + name, "icon": "mdi:bell-sleep"},
			State: func() string { return hamqtt.OnOff(book.Quiet(id) != "") },
			Command: func(payload string) {
				until := "off"
				if payload == "ON" {
					until = "" // today
				}
				if out, err := book.SetQuiet(id, until); err != nil {
					log.Printf("[HA device] hints %s: %v", id, err)
				} else {
					log.Printf("[HA device] hints %s: %s", id, out)
				}
			}},
		{Component: "sensor", Key: "room_" + key, Config: map[string]any{
			"name": "Raum " + name, "icon": "mdi:map-marker-radius"},
			State: func() string { return d.recentRoom(id) }},
	}
}

// recentRoom: the room of the person's last conversation at a speaker while it is fresh
// (INITIATIVE_ROOM_FRESH_MIN, as the initiative's whereabouts), "None" (unknown) otherwise.
func (d *device) recentRoom(person string) string {
	d.lastRoomMu.Lock()
	seen := d.lastRoom[person]
	d.lastRoomMu.Unlock()
	if seen.room == "" || time.Since(seen.at) > time.Duration(prompts.Int("INITIATIVE_ROOM_FRESH_MIN", 30))*time.Minute {
		return "None"
	}
	return seen.room
}
