package parceltrack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/store"
)

// fakeHA answers the template and the seventeentrack actions like Home Assistant.
type fakeHA struct {
	entry    string
	packages []pkg
	added    []string
	archived []string
	addFails bool
}

func (f *fakeHA) rest(method, path string, payload any) (int, []byte, error) {
	data, _ := json.Marshal(payload)
	var args map[string]any
	json.Unmarshal(data, &args)
	switch {
	case path == "/api/template":
		return 200, []byte(f.entry), nil
	case strings.HasSuffix(path, "/add_package"):
		if f.addFails {
			return 500, []byte(`{"message":"login failed"}`), nil
		}
		f.added = append(f.added, args["package_tracking_number"].(string)+"="+args["package_friendly_name"].(string))
		return 200, []byte("[]"), nil
	case strings.HasSuffix(path, "/archive_package"):
		f.archived = append(f.archived, args["package_tracking_number"].(string))
		return 200, []byte("[]"), nil
	case strings.HasSuffix(path, "/get_packages?return_response"):
		body, _ := json.Marshal(map[string]any{"changed_states": []any{},
			"service_response": map[string]any{"packages": f.packages}})
		return 200, body, nil
	}
	return 404, nil, nil
}

type told struct{ person, text string }

func setup(t *testing.T, ha *fakeHA, shipments []Shipment) (*Tracker, *[]told, map[string]string) {
	store.Dir = t.TempDir()
	tr := New()
	tr.Rest = ha.rest
	tr.Shipments = func() []Shipment { return shipments }
	var messages []told
	tr.Tell = func(person, text string) bool { messages = append(messages, told{person, text}); return true }
	updates := map[string]string{}
	tr.Update = func(number, status string, delivered bool) { updates[number] = status }
	return tr, &messages, updates
}

func TestNumber(t *testing.T) {
	for in, want := range map[string]string{
		"00340434 1234 5678 9012": "00340434123456789012",
		"h1234567890123456789":    "H1234567890123456789",
		"JJD-0001-2345-6789":      "JJD000123456789",
		"1234":                    "",
		"abc; drop":               "",
		"":                        "",
	} {
		if got := Number(in); got != want {
			t.Errorf("Number(%q) = %q, want %q", in, got, want)
		}
	}
	if Slug("Ready to be picked up") != "ready_to_be_picked_up" || Slug("In Transit") != "in_transit" {
		t.Error("Slug")
	}
}

func TestFollowAndTell(t *testing.T) {
	ha := &fakeHA{entry: "abc123"}
	tr, messages, updates := setup(t, ha, []Shipment{
		{Person: "anna", Number: "00340434123456789012", Carrier: "DHL", Sender: "Shop"},
		{Person: "ben", Number: "H1234567890123456789", Carrier: "Hermes", Sender: "Versand", Delivered: true},
		{Person: "ben", Number: "kein", Carrier: "DPD"},
	})
	tr.Step() // taken up and added; 17TRACK does not know it yet
	if len(ha.added) != 1 || ha.added[0] != "00340434123456789012=Shop" {
		t.Fatalf("added %v", ha.added)
	}
	ha.packages = []pkg{{TrackingNumber: "00340434123456789012", Status: "In Transit", InfoText: "Im Startpaketzentrum",
		Timestamp: "2026-11-06T10:00:00+00:00"}}
	tr.Step() // the first state: no news, but the mail watch gets it
	if len(*messages) != 0 || updates["00340434123456789012"] != "unterwegs: Im Startpaketzentrum" {
		t.Fatalf("first state: %v %v", *messages, updates)
	}
	tr.Step() // unchanged
	if len(*messages) != 0 {
		t.Fatalf("unchanged told: %v", *messages)
	}
	ha.packages[0].InfoText, ha.packages[0].Timestamp = "In Zustellung", "2026-11-07T08:00:00+00:00"
	tr.Step()
	if len(*messages) != 1 || (*messages)[0].person != "anna" ||
		(*messages)[0].text != "📦 Paket von Shop (DHL): unterwegs – In Zustellung" {
		t.Fatalf("change: %v", *messages)
	}
	ha.packages[0].Status, ha.packages[0].InfoText, ha.packages[0].Location = "Delivered", "Zugestellt", "Berlin"
	tr.Step()
	if len(*messages) != 2 || !strings.Contains((*messages)[1].text, "zugestellt") || len(ha.archived) != 1 {
		t.Fatalf("delivered: %v, archived %v", *messages, ha.archived)
	}
	tr.Step() // done: not added again, not told again
	if len(ha.added) != 1 || len(*messages) != 2 {
		t.Fatalf("after done: added %v, told %v", ha.added, *messages)
	}
}

func TestRulesDecide(t *testing.T) {
	ha := &fakeHA{entry: "abc123"}
	tr, messages, _ := setup(t, ha, []Shipment{{Person: "anna", Number: "00340434123456789012", Carrier: "DHL"}})
	tr.Rules = func(string) string { return "- Bei Paketen nur melden, wenn sie zugestellt sind." }
	asked := 0
	tr.Ask = func(input string, choices []string, unit string) (string, error) {
		asked++
		if strings.Contains(input, `"delivered"`) {
			return "tell", nil
		}
		return "skip", nil
	}
	tr.Step()
	ha.packages = []pkg{{TrackingNumber: "00340434123456789012", Status: "In Transit", InfoText: "a"}}
	tr.Step()
	ha.packages[0].InfoText = "b"
	tr.Step()
	ha.packages[0].InfoText = "c"
	tr.Step()
	if len(*messages) != 0 || asked != 1 {
		t.Fatalf("in transit: told %v, asked %d", *messages, asked)
	}
	ha.packages[0].Status = "Delivered"
	tr.Step()
	if len(*messages) != 1 || asked != 2 {
		t.Fatalf("delivered: told %v, asked %d", *messages, asked)
	}
}

func TestNotSetUpAndAddFailing(t *testing.T) {
	ha := &fakeHA{}
	tr, _, _ := setup(t, ha, []Shipment{{Person: "anna", Number: "00340434123456789012"}})
	tr.Step()
	if len(ha.added) != 0 {
		t.Fatal("added without the integration")
	}
	ha.entry, ha.addFails = "abc123", true
	tr.Step()
	p := tr.find("00340434123456789012")
	if p.Added || p.Tries != 1 || p.NextTry <= time.Now().Unix() {
		t.Fatalf("after a failure: %+v", p)
	}
	tr.Step() // not before NextTry
	if p.Tries != 1 {
		t.Fatalf("tried again at once: %+v", p)
	}
}

func TestOldParcelsEnd(t *testing.T) {
	ha := &fakeHA{entry: "abc123"}
	tr, _, _ := setup(t, ha, []Shipment{{Person: "anna", Number: "00340434123456789012"}})
	tr.Step()
	now := time.Now()
	tr.Now = func() time.Time { return now.Add(50 * 24 * time.Hour) }
	tr.Step()
	if !tr.find("00340434123456789012").Done || len(ha.archived) != 1 {
		t.Fatalf("old parcel: archived %v", ha.archived)
	}
}
