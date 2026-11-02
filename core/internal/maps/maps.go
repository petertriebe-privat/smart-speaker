// Package maps: Google Maps Platform with an API key (GOOGLE_MAPS_API_KEY), as devices/atoms3r/src/
// services/maps.cpp. Tool find_place (Places API (New) Text Search), travel_time (Routes API),
// get_weather (Weather, Pollen and Air Quality APIs). Requests are counted per API and calendar
// month (Quota) before they are sent; at the monthly limit the tool refuses.
package maps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"echo-assistant/internal/config"
	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

const (
	placesHost     = "places.googleapis.com"
	routesHost     = "routes.googleapis.com"
	geocodingHost  = "maps.googleapis.com"
	weatherHost    = "weather.googleapis.com"
	pollenHost     = "pollen.googleapis.com"
	airQualityHost = "airquality.googleapis.com"
	requestTimeout = 20 * time.Second
)

// HomeRest is the part of Home Assistant's client used here: its REST API gives zone.home.
type HomeRest interface {
	Rest(method, path string, payload any) (int, []byte, error)
}

// Service is the Google Maps tools.
type Service struct {
	key    string
	ha     HomeRest
	quota  *Quota
	client *http.Client

	homeMu            sync.Mutex
	homeKnown         bool
	homeLat, homeLong float64
}

// New reads GOOGLE_MAPS_API_KEY. ha supplies home's coordinates (zone.home) and may be nil;
// quota is shared with the other users of the key.
func New(secrets config.Secrets, ha HomeRest, quota *Quota) *Service {
	return &Service{key: secrets.String("GOOGLE_MAPS_API_KEY"), ha: ha, quota: quota,
		client: netx.Client(requestTimeout)}
}

// Configured: a key is set.
func (s *Service) Configured() bool { return s.key != "" }

// Count counts one request of api in this month's quota, for other users of the same key.
func (s *Service) Count(api string, limit int) error { return s.quota.Count(api, limit) }

// --- Home (Home Assistant's zone.home), fetched once --------------------------------------

func (s *Service) home() (lat, long float64, ok bool) {
	s.homeMu.Lock()
	defer s.homeMu.Unlock()
	if !s.homeKnown && s.ha != nil {
		status, body, err := s.ha.Rest("GET", "/api/states/zone.home", nil)
		var zone struct {
			Attributes struct {
				Latitude  *float64 `json:"latitude"`
				Longitude *float64 `json:"longitude"`
			} `json:"attributes"`
		}
		if err == nil && status == 200 && json.Unmarshal(body, &zone) == nil &&
			zone.Attributes.Latitude != nil && zone.Attributes.Longitude != nil {
			s.homeLat, s.homeLong = *zone.Attributes.Latitude, *zone.Attributes.Longitude
			s.homeKnown = true
		} else {
			log.Printf("[Maps] home coordinates not available from Home Assistant")
		}
	}
	return s.homeLat, s.homeLong, s.homeKnown
}

// KmFromHome: straight-line distance in km from home, < 0 if home is unknown (haversine).
func (s *Service) KmFromHome(latitude, longitude float64) float64 {
	hLat, hLong, ok := s.home()
	if !ok {
		return -1
	}
	const radians = math.Pi / 180
	dLat, dLon := (latitude-hLat)*radians, (longitude-hLong)*radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(hLat*radians)*math.Cos(latitude*radians)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 6371.0 * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// --- HTTP -----------------------------------------------------------------------------

// call: a POST with a JSON body, or a GET (body nil). fieldMask "": the API has none.
func (s *Service) call(host, path, fieldMask string, body []byte) (int, []byte, error) {
	method := "GET"
	var reader io.Reader
	if len(body) > 0 {
		method = "POST"
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "https://"+host+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Goog-Api-Key", s.key)
	if fieldMask != "" {
		req.Header.Set("X-Goog-FieldMask", fieldMask)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

func isTimeout(err error) bool {
	var n net.Error
	return errors.As(err, &n) && n.Timeout()
}

// errorText: the model's text for a failed request.
func errorText(status int, data []byte, err error) string {
	if err != nil {
		if isTimeout(err) {
			return "Error: Google Maps does not answer"
		}
		return "Error: Google Maps not reachable (" + err.Error() + ")"
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(data, &parsed)
	msg := parsed.Error.Message
	if msg != "" {
		msg = ": " + textutil.Prefix(msg, 200)
	}
	return fmt.Sprintf("Error: HTTP %d%s", status, msg)
}

func parse(data []byte, out any) string {
	if err := json.Unmarshal(data, out); err != nil {
		return "Error: Google's answer is not readable (" + err.Error() + ")"
	}
	return ""
}

// request counts one request of api, sends it and parses the answer into out. "" on success,
// else the error text.
func (s *Service) request(api string, limit int, host, path, fieldMask string, body []byte, out any) string {
	if err := s.quota.Count(api, limit); err != nil {
		return err.Error()
	}
	status, data, err := s.call(host, path, fieldMask, body)
	if err != nil || status != 200 {
		return errorText(status, data, err)
	}
	return parse(data, out)
}

// --- Address -------------------------------------------------------------------------

// Address: the street address of a position (Google Geocoding API, counted as "geocoding"). ""
// with an error text if there is none or the API refused.
func (s *Service) Address(latitude, longitude float64) (string, string) {
	if !s.Configured() {
		return "", "Error: no Google Maps key"
	}
	if err := s.quota.Count("geocoding", prompts.Int("GEOCODING_MONTHLY_LIMIT", 9000)); err != nil {
		return "", err.Error()
	}
	// The Geocoding web service takes the key as a parameter.
	path := fmt.Sprintf("/maps/api/geocode/json?language=de&latlng=%.6f,%.6f&key=%s", latitude, longitude, s.key)
	status, data, err := s.call(geocodingHost, path, "", nil)
	var parsed struct {
		Status       string `json:"status"`
		ErrorMessage string `json:"error_message"`
		Results      []struct {
			FormattedAddress string `json:"formatted_address"`
		} `json:"results"`
	}
	if err != nil || status != 200 || parse(data, &parsed) != "" {
		return "", errorText(status, data, err)
	}
	if parsed.Status != "OK" {
		msg := "Error: Geocoding " + parsed.Status + " " + parsed.ErrorMessage
		if parsed.Status == "REQUEST_DENIED" {
			msg += " (enable the Geocoding API and allow it for the key)"
		}
		return "", msg
	}
	if len(parsed.Results) == 0 {
		return "", ""
	}
	return parsed.Results[0].FormattedAddress, ""
}

// --- Tools ----------------------------------------------------------------------------

// AddTools registers find_place, travel_time, get_weather and the console tool maps_usage.
func (s *Service) AddTools(r *tools.Registry) {
	if !s.Configured() {
		return
	}
	r.Add(tools.Tool{
		Name: "find_place",
		Description: "Searches Google Maps for businesses and places (restaurants, shops, doctors, craftsmen, authorities), " +
			"preferring those near home: address, phone number, opening hours, whether open now, rating, website. Use it " +
			"instead of the web search for such questions and to get the number before phone_call to a business. " +
			"Limited to 1000 searches a month: one precise query (what and where, e.g. 'Italienisches Restaurant " +
			"Altstadt' or 'Zahnarzt Dr. Müller Hamburg'), not several tries.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"query": {"type": "string", "description": "what and, if not near home, where"},
                           "open_now": {"type": "boolean", "description": "only places open right now"}},
            "required": ["query"]}`,
		Run: s.findPlace,
	})
	r.Add(tools.Tool{
		Name: "travel_time",
		Description: "Travel time between two places with Google Maps: by car with the traffic expected, by public transport " +
			"with the connections (times, lines, stops), by bicycle or on foot. For 'wie lange brauche ich nach …', " +
			"'wann muss ich los, um um 9 in … zu sein', 'wann fährt der nächste Bus nach …'. Places as address or name " +
			"with town; '' or 'home' is the user's home. Limited to 1000 requests a month.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"destination": {"type": "string", "description": "'' = home"},
                           "origin": {"type": "string", "description": "'' = home"},
                           "mode": {"type": "string", "enum": ["drive", "transit", "bicycle", "walk"]},
                           "date": {"type": "string", "description": "YYYY-MM-DD, '' = today"},
                           "time": {"type": "string", "description": "HH:MM, '' = now"},
                           "time_is": {"type": "string", "enum": ["departure", "arrival"]}},
            "required": ["destination", "mode"]}`,
		Run: s.travelTime,
	})
	r.Add(tools.Tool{
		Name: "get_weather",
		Description: "Weather from Google for home or another place. include picks the parts: now (current conditions), hours " +
			"(hourly forecast), days (daily forecast with sunrise and sunset), alerts (official weather warnings), " +
			"pollen (pollen forecast, up to 5 days), air (air quality now). Ask only for what the question needs; a " +
			"morning overview: now, days, alerts, pollen. Use it instead of the web search for weather.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"include": {"type": "array",
                                       "items": {"type": "string", "enum": ["now", "hours", "days", "alerts", "pollen", "air"]}},
                           "place": {"type": "string", "description": "town or address, '' = home"},
                           "hours": {"type": "integer", "description": "hours ahead, 1-24, default 12"},
                           "days": {"type": "integer", "description": "days ahead incl. today, 1-10, default 3"}},
            "required": ["include"]}`,
		Run: s.getWeather,
	})
	// Only for tests over the console: the month's counted requests.
	r.Add(tools.Tool{
		Name:        "maps_usage",
		Description: "Requests to Google Maps counted this month (quota file).",
		Parameters:  `{"type": "object", "additionalProperties": false, "properties": {}, "required": []}`,
		Channels:    tools.Console,
		Run:         func(map[string]any, tools.Context) string { return s.quota.Usage() },
	})
}
