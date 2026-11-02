// Package hamqtt: this speaker as a Home Assistant device over MQTT discovery (#65). Entities
// are declared with a state and/or a command; the device publishes their discovery configs
// (retained), its availability (online, the last will offline), every state change and fires
// events. Name and room come back from HA's device registry (registry.go).
package hamqtt

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/mqtt"
)

// Entity: one HA entity of the device.
type Entity struct {
	Component string         // sensor, binary_sensor, number, switch, button, event, notify
	Key       string         // object id within the device ([a-z0-9_])
	Config    map[string]any // name, icon, device_class, entity_category, options, min, max, ...
	State     func() string  // nil = no state (button, notify, event)
	Command   func(payload string)
}

// Device: the speaker in HA.
type Device struct {
	ID                                  string // stable: the node id in every topic and unique id
	Name, Model, Version, SuggestedArea string
	Entities                            []Entity
	Retired                             []string // "<component>/<id>/<key>" given up: removed from HA
	client                              *mqtt.Client
	mu                                  sync.Mutex
	published                           map[string]string
}

const discoveryPrefix = "homeassistant"

func (d *Device) base() string         { return "assistant/" + d.ID }
func (d *Device) availability() string { return d.base() + "/status" }
func (d *Device) stateTopic(key string) string {
	return d.base() + "/" + key
}
func (d *Device) commandTopic(key string) string { return d.base() + "/" + key + "/set" }

// Start connects to the broker and keeps the device up to date. Returns at once.
func (d *Device) Start(url, user, password string) error {
	c, err := mqtt.New(mqtt.Options{URL: url, User: user, Password: password, ClientID: "assistant_" + d.ID,
		WillTopic: d.availability(), WillPayload: "offline",
		OnConnect: d.onConnect, OnMessage: d.onMessage})
	if err != nil {
		return err
	}
	d.client = c
	go c.Run(15 * time.Second)
	go func() {
		for range time.Tick(2 * time.Second) {
			d.publishStates(false)
		}
	}()
	return nil
}

func (d *Device) onConnect(c *mqtt.Client) {
	d.mu.Lock()
	d.published = map[string]string{}
	d.mu.Unlock()
	var topics []string
	for _, e := range d.Entities {
		if e.Command != nil {
			topics = append(topics, d.commandTopic(e.Key))
		}
	}
	// HA's birth message: after an HA restart the discovery goes out again.
	topics = append(topics, discoveryPrefix+"/status")
	c.Subscribe(topics...)
	d.announce()
}

// announce: every discovery config, online, every state.
func (d *Device) announce() {
	for _, e := range d.Entities {
		data, _ := json.Marshal(d.discovery(e))
		d.client.Publish(discoveryPrefix+"/"+e.Component+"/"+d.ID+"/"+e.Key+"/config", data, true)
	}
	for _, r := range d.Retired { // an empty config removes the entity from HA
		d.client.Publish(discoveryPrefix+"/"+r+"/config", nil, true)
	}
	d.client.Publish(d.availability(), []byte("online"), true)
	d.publishStates(true)
	log.Printf("[HA device] %d entities announced as %q", len(d.Entities), d.Name)
}

func (d *Device) discovery(e Entity) map[string]any {
	cfg := map[string]any{
		"unique_id":          d.ID + "_" + e.Key,
		"availability_topic": d.availability(),
		"has_entity_name":    true,
		"origin":             map[string]any{"name": "smart-speaker", "sw": d.Version},
		"device": map[string]any{"identifiers": []string{d.ID}, "name": d.Name, "model": d.Model,
			"manufacturer": "smart-speaker", "sw_version": d.Version, "suggested_area": d.SuggestedArea},
	}
	for k, v := range e.Config {
		cfg[k] = v
	}
	if e.State != nil || e.Component == "event" {
		cfg["state_topic"] = d.stateTopic(e.Key)
	}
	if e.Command != nil {
		cfg["command_topic"] = d.commandTopic(e.Key)
	}
	return cfg
}

// publishStates sends the states that changed (all with force); retained, so HA has them
// after its own restart too.
func (d *Device) publishStates(force bool) {
	if d.client == nil || !d.client.Connected() {
		return
	}
	for _, e := range d.Entities {
		if e.State == nil {
			continue
		}
		v := e.State()
		d.mu.Lock()
		old, seen := d.published[e.Key]
		d.mu.Unlock()
		if seen && old == v && !force {
			continue
		}
		if d.client.Publish(d.stateTopic(e.Key), []byte(v), true) == nil {
			d.mu.Lock()
			d.published[e.Key] = v
			d.mu.Unlock()
		}
	}
}

// Changed: a state changed now (sooner than the next 2 s round).
func (d *Device) Changed() { go d.publishStates(false) }

// Fire: an event entity's event (event_type one of its event_types), not retained.
func (d *Device) Fire(key, eventType string, attrs map[string]any) {
	if d == nil || d.client == nil {
		return
	}
	payload := map[string]any{"event_type": eventType}
	for k, v := range attrs {
		payload[k] = v
	}
	data, _ := json.Marshal(payload)
	if err := d.client.Publish(d.stateTopic(key), data, false); err == nil {
		log.Printf("[HA device] event %s %s", key, eventType)
	}
}

func (d *Device) onMessage(m mqtt.Message) {
	if m.Topic == discoveryPrefix+"/status" {
		if string(m.Payload) == "online" {
			go d.announce()
		}
		return
	}
	for _, e := range d.Entities {
		if e.Command != nil && m.Topic == d.commandTopic(e.Key) {
			payload := strings.TrimSpace(string(m.Payload))
			log.Printf("[HA device] %s: %.80q", e.Key, payload)
			go func() {
				e.Command(payload)
				// HA shows what it sent until the state answers, even an unchanged one (a text
				// field kept "1 Minute" instead of its empty state)
				d.mu.Lock()
				delete(d.published, e.Key)
				d.mu.Unlock()
				d.publishStates(false)
			}()
			return
		}
	}
}

// OnOff: a bool as HA's binary_sensor / switch payload.
func OnOff(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}
