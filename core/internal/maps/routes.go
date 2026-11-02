package maps

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Pro SKU with TRAFFIC_AWARE; the steps only for public transport (connections).
const (
	routeFields   = "routes.duration,routes.staticDuration,routes.distanceMeters,routes.description,routes.warnings"
	transitFields = "routes.duration,routes.distanceMeters,routes.warnings," +
		"routes.legs.steps.travelMode,routes.legs.steps.staticDuration,routes.legs.steps.transitDetails"
)

type textValue struct {
	Text string `json:"text"`
}

type step struct {
	TravelMode     string `json:"travelMode"`
	StaticDuration string `json:"staticDuration"`
	TransitDetails struct {
		TransitLine struct {
			NameShort string `json:"nameShort"`
			Name      string `json:"name"`
			Vehicle   struct {
				Name textValue `json:"name"`
			} `json:"vehicle"`
		} `json:"transitLine"`
		Headsign    string `json:"headsign"`
		StopDetails struct {
			DepartureStop struct {
				Name string `json:"name"`
			} `json:"departureStop"`
			ArrivalStop struct {
				Name string `json:"name"`
			} `json:"arrivalStop"`
		} `json:"stopDetails"`
		LocalizedValues struct {
			DepartureTime struct {
				Time textValue `json:"time"`
			} `json:"departureTime"`
			ArrivalTime struct {
				Time textValue `json:"time"`
			} `json:"arrivalTime"`
		} `json:"localizedValues"`
	} `json:"transitDetails"`
}

type route struct {
	Duration       string   `json:"duration"`
	StaticDuration string   `json:"staticDuration"`
	DistanceMeters float64  `json:"distanceMeters"`
	Description    string   `json:"description"`
	Warnings       []string `json:"warnings"`
	Legs           []struct {
		Steps []step `json:"steps"`
	} `json:"legs"`
}

// seconds reads a duration like "1234s".
func seconds(duration string) int {
	n, err := strconv.ParseFloat(strings.TrimSuffix(duration, "s"), 64)
	if err != nil {
		return 0
	}
	return int(n)
}

func minutesText(seconds int) string {
	minutes := (seconds + 30) / 60
	if minutes < 60 {
		return fmt.Sprintf("%d min", minutes)
	}
	return fmt.Sprintf("%d h %d min", minutes/60, minutes%60)
}

func clockText(t time.Time) string { return t.Local().Format("15:04") }

// parseLocal reads "YYYY-MM-DD" (” = today) and "HH:MM" as local time.
func parseLocal(date, clock string, today time.Time) (time.Time, bool) {
	if date == "" {
		date = today.Format("2006-01-02")
	}
	var year, month, day, hour, minute int
	if n, _ := fmt.Sscanf(date, "%4d-%2d-%2d", &year, &month, &day); n != 3 {
		return time.Time{}, false
	}
	if n, _ := fmt.Sscanf(clock, "%2d:%2d", &hour, &minute); n != 2 || year > 2037 || hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.Local), true
}

func utcStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func isHome(place string) bool {
	return place == "" || strings.EqualFold(place, "home") || strings.EqualFold(place, "zuhause") ||
		strings.EqualFold(place, "daheim")
}

// waypoint: an address or place name, or home (Home Assistant's coordinates). "" error text on success.
func (s *Service) waypoint(place string) (map[string]any, string) {
	if !isHome(place) {
		return map[string]any{"address": place}, ""
	}
	lat, long, ok := s.home()
	if !ok {
		return nil, "Error: home is unknown (Home Assistant's zone.home): give the address"
	}
	return map[string]any{"location": map[string]any{"latLng": map[string]any{"latitude": lat, "longitude": long}}}, ""
}

// connections: public transport, the rides with times, lines and stops, walks in between.
func connections(steps []step) string {
	var text strings.Builder
	walk := 0
	flush := func() {
		if walk >= 60 {
			text.WriteString("  walk " + minutesText(walk) + "\n")
		}
	}
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	for _, st := range steps {
		if st.TravelMode != "TRANSIT" {
			walk += seconds(st.StaticDuration)
			continue
		}
		flush()
		walk = 0
		d := st.TransitDetails
		line := d.TransitLine
		name := or(line.NameShort, line.Name)
		text.WriteString("  " + or(d.LocalizedValues.DepartureTime.Time.Text, "?") + " " + line.Vehicle.Name.Text + " " + name)
		if d.Headsign != "" {
			text.WriteString(" towards " + d.Headsign)
		}
		text.WriteString(" from " + or(d.StopDetails.DepartureStop.Name, "?") + " to " +
			or(d.StopDetails.ArrivalStop.Name, "?") + ", arrives " +
			or(d.LocalizedValues.ArrivalTime.Time.Text, "?") + "\n")
	}
	flush()
	return text.String()
}

func (s *Service) travelTime(args map[string]any, _ tools.Context) string {
	if !s.quota.synced() {
		return "Error: the clock is not set yet."
	}
	destination, origin := tools.String(args, "destination"), tools.String(args, "origin")
	if isHome(destination) && isHome(origin) {
		return "Error: origin and destination are both home"
	}
	mode := tools.String(args, "mode")
	if mode == "" {
		mode = "drive"
	}
	travelMode := map[string]string{"transit": "TRANSIT", "bicycle": "BICYCLE", "walk": "WALK", "drive": "DRIVE"}[mode]
	if travelMode == "" {
		return "Error: mode is drive, transit, bicycle or walk"
	}

	// When: now, or leaving / arriving at date + time.
	clock := tools.String(args, "time")
	arriving := tools.String(args, "time_is") == "arrival"
	now := time.Now()
	when := now
	if clock != "" {
		var ok bool
		if when, ok = parseLocal(tools.String(args, "date"), clock, now); !ok {
			return "Error: date as YYYY-MM-DD, time as HH:MM"
		}
		if when.Before(now.Add(-60 * time.Second)) {
			return "Error: this time is in the past"
		}
	}

	from, msg := s.waypoint(origin)
	if msg != "" {
		return msg
	}
	to, msg := s.waypoint(destination)
	if msg != "" {
		return msg
	}
	request := map[string]any{"origin": from, "destination": to, "travelMode": travelMode,
		"languageCode": "de", "regionCode": "DE", "units": "METRIC"}
	transit, drive := travelMode == "TRANSIT", travelMode == "DRIVE"
	if drive {
		request["routingPreference"] = "TRAFFIC_AWARE"
	}
	// Arriving by car: the traffic around the arrival time stands in for the one on the way.
	if transit && arriving {
		request["arrivalTime"] = utcStamp(when)
	} else if (transit || drive) && when.After(now.Add(60*time.Second)) {
		request["departureTime"] = utcStamp(when)
	}

	body, _ := json.Marshal(request)
	fields := routeFields
	if transit {
		fields = transitFields
	}
	var parsed struct {
		Routes []route `json:"routes"`
	}
	if msg := s.request("routes", prompts.Int("ROUTES_MONTHLY_LIMIT", 1000), routesHost,
		"/directions/v2:computeRoutes", fields, body, &parsed); msg != "" {
		return msg
	}
	if len(parsed.Routes) == 0 {
		return "No route found (check the places, or try another mode)."
	}
	return formatRoute(parsed.Routes[0], origin, destination, mode, arriving, when, now)
}

func formatRoute(r route, origin, destination, mode string, arriving bool, when, now time.Time) string {
	transit, drive := mode == "transit", mode == "drive"
	duration := seconds(r.Duration)
	from, to := origin, destination
	if isHome(origin) {
		from = "home"
	}
	if isHome(destination) {
		to = "home"
	}
	by := "foot"
	switch {
	case transit:
		by = "public transport"
	case drive:
		by = "car"
	case mode == "bicycle":
		by = "bicycle"
	}
	text := "By " + by + " from " + from + " to " + to + ": " + minutesText(duration)
	if drive {
		without := seconds(r.StaticDuration)
		if without != 0 && without != duration {
			text += " with the traffic expected (" + minutesText(without) + " without)"
		} else {
			text += " (no traffic delay)"
		}
	}
	text += fmt.Sprintf(", %.1f km", r.DistanceMeters/1000)
	if r.Description != "" {
		text += ", via " + r.Description
	}
	text += ".\n"
	span := time.Duration(duration) * time.Second
	if arriving && !transit {
		leave := when.Add(-span)
		if !leave.Before(now) {
			text += "Leave by " + clockText(leave) + " to arrive at " + clockText(when) + ".\n"
		} else {
			text += "Too late: leaving now, arrival about " + clockText(now.Add(span)) + " instead of " +
				clockText(when) + ".\n"
		}
	} else if !transit {
		leaving := "now"
		if when.After(now.Add(60 * time.Second)) {
			leaving = "at " + clockText(when)
		}
		text += "Leaving " + leaving + ", arrival about " + clockText(when.Add(span)) + ".\n"
	}
	if transit && len(r.Legs) > 0 {
		text += connections(r.Legs[0].Steps)
	}
	for _, w := range r.Warnings {
		text += "Note: " + w + "\n"
	}
	return text
}
