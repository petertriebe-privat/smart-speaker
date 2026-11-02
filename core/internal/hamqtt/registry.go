package hamqtt

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/coder/websocket"

	"echo-assistant/internal/netx"
)

// Placement: the device's name and area as the owner set them in HA's UI ("" = not set there).
type Placement struct {
	Name, Area string
}

// WatchRegistry follows this device in HA's device registry over HA's WebSocket API (admin token):
// once at the start and after every change of the device or of an area, onChange gets the name
// the owner gave it and its area's name. Reconnects; start as a goroutine.
func WatchRegistry(haURL, token, deviceID string, onChange func(Placement)) {
	url := strings.TrimRight(haURL, "/")
	url = strings.Replace(strings.Replace(url, "https://", "wss://", 1), "http://", "ws://", 1) + "/api/websocket"
	for {
		err := watch(url, token, deviceID, onChange)
		log.Printf("[HA device] registry: %v, again in 30 s", err)
		time.Sleep(30 * time.Second)
	}
}

type registryDevice struct {
	ID          string      `json:"id"`
	Identifiers [][2]string `json:"identifiers"`
	NameByUser  string      `json:"name_by_user"`
	AreaID      string      `json:"area_id"`
}

type registryArea struct {
	AreaID string `json:"area_id"`
	Name   string `json:"name"`
}

// placementOf finds deviceID (an MQTT identifier) in the lists; ok false while HA does not know
// it yet.
func placementOf(devices []registryDevice, areas []registryArea, deviceID string) (Placement, string, bool) {
	for _, d := range devices {
		for _, id := range d.Identifiers {
			if id[0] == "mqtt" && id[1] == deviceID {
				p := Placement{Name: strings.TrimSpace(d.NameByUser)}
				for _, a := range areas {
					if a.AreaID == d.AreaID {
						p.Area = strings.TrimSpace(a.Name)
					}
				}
				return p, d.ID, true
			}
		}
	}
	return Placement{}, "", false
}

func watch(url, token, deviceID string, onChange func(Placement)) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dial, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dial, url, &websocket.DialOptions{HTTPClient: netx.Client(0)})
	dialCancel()
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(8 << 20) // the device registry of a large home
	nextID := 0
	// The auth message carries no id: HA refuses it with any extra key (27.10.2026: the watcher
	// never logged in and a name set in HA was not taken over).
	send := func(v map[string]any) (int, error) {
		if v["type"] != "auth" {
			nextID++
			v["id"] = nextID
		}
		data, _ := json.Marshal(v)
		c, cc := context.WithTimeout(ctx, 10*time.Second)
		defer cc()
		return nextID, conn.Write(c, websocket.MessageText, data)
	}
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
	var devices []registryDevice
	var areas []registryArea
	devicesID, areasID := -1, -1
	haID := ""
	last, known := Placement{}, false
	refresh := func() {
		devicesID, _ = send(map[string]any{"type": "config/device_registry/list"})
		areasID, _ = send(map[string]any{"type": "config/area_registry/list"})
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg struct {
			Type    string          `json:"type"`
			ID      int             `json:"id"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   struct{ Message string }
			Event   struct {
				EventType string `json:"event_type"`
				Data      struct {
					DeviceID string `json:"device_id"`
				} `json:"data"`
			} `json:"event"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "auth_required":
			send(map[string]any{"type": "auth", "access_token": token})
		case "auth_invalid":
			return errors.New("login refused")
		case "auth_ok":
			send(map[string]any{"type": "subscribe_events", "event_type": "device_registry_updated"})
			send(map[string]any{"type": "subscribe_events", "event_type": "area_registry_updated"})
			refresh()
		case "event":
			if msg.Event.EventType == "area_registry_updated" || msg.Event.Data.DeviceID == haID || haID == "" {
				refresh()
			}
		case "result":
			if !msg.Success {
				log.Printf("[HA device] registry: %s", msg.Error.Message)
				continue
			}
			switch msg.ID {
			case devicesID:
				devices = nil
				json.Unmarshal(msg.Result, &devices)
			case areasID:
				areas = nil
				json.Unmarshal(msg.Result, &areas)
			default:
				continue
			}
			if devices == nil || areas == nil {
				continue
			}
			p, id, ok := placementOf(devices, areas, deviceID)
			devices, areas = nil, nil
			if !ok {
				continue
			}
			haID = id
			if !known || p != last {
				last, known = p, true
				onChange(p)
			}
		}
	}
}
