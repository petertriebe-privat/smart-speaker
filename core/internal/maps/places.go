package maps

import (
	"encoding/json"
	"fmt"
	"strings"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/tools"
)

// Enterprise SKU (phone, opening hours, rating): what find_place is for.
const placeFields = "places.displayName,places.formattedAddress,places.internationalPhoneNumber,places.websiteUri," +
	"places.rating,places.userRatingCount,places.businessStatus,places.currentOpeningHours.openNow," +
	"places.regularOpeningHours.weekdayDescriptions"

type place struct {
	DisplayName struct {
		Text string `json:"text"`
	} `json:"displayName"`
	FormattedAddress         string   `json:"formattedAddress"`
	InternationalPhoneNumber string   `json:"internationalPhoneNumber"`
	WebsiteURI               string   `json:"websiteUri"`
	Rating                   *float64 `json:"rating"`
	UserRatingCount          int      `json:"userRatingCount"`
	BusinessStatus           string   `json:"businessStatus"`
	CurrentOpeningHours      struct {
		OpenNow *bool `json:"openNow"`
	} `json:"currentOpeningHours"`
	RegularOpeningHours struct {
		WeekdayDescriptions []string `json:"weekdayDescriptions"`
	} `json:"regularOpeningHours"`
}

func (s *Service) findPlace(args map[string]any, _ tools.Context) string {
	query := tools.String(args, "query")
	if query == "" {
		return "Error: query missing"
	}
	request := map[string]any{
		"textQuery":    query,
		"languageCode": "de",
		"regionCode":   "DE",
		"pageSize":     prompts.Int("PLACES_RESULTS", 5),
	}
	if tools.Bool(args, "open_now", false) {
		request["openNow"] = true
	}
	if lat, long, ok := s.home(); ok {
		request["locationBias"] = map[string]any{"circle": map[string]any{
			"center": map[string]any{"latitude": lat, "longitude": long},
			"radius": prompts.Int("PLACES_BIAS_METERS", 20000),
		}}
	}
	body, _ := json.Marshal(request)
	var parsed struct {
		Places []place `json:"places"`
	}
	if msg := s.request("places", prompts.Int("PLACES_MONTHLY_LIMIT", 1000), placesHost,
		"/v1/places:searchText", placeFields, body, &parsed); msg != "" {
		return msg
	}
	return formatPlaces(query, parsed.Places)
}

func formatPlaces(query string, places []place) string {
	if len(places) == 0 {
		return "No places found for \"" + query + "\"."
	}
	var text strings.Builder
	text.WriteString("Places for \"" + query + "\":\n")
	for _, p := range places {
		name := p.DisplayName.Text
		if name == "" {
			name = "(no name)"
		}
		text.WriteString("- " + name)
		if p.FormattedAddress != "" {
			text.WriteString(" | " + p.FormattedAddress)
		}
		if p.InternationalPhoneNumber != "" {
			text.WriteString(" | phone " + p.InternationalPhoneNumber)
		}
		if p.Rating != nil {
			fmt.Fprintf(&text, " | rating %.1f (%d)", *p.Rating, p.UserRatingCount)
		}
		if p.BusinessStatus != "" && p.BusinessStatus != "OPERATIONAL" {
			text.WriteString(" | " + p.BusinessStatus)
		}
		if p.CurrentOpeningHours.OpenNow != nil {
			if *p.CurrentOpeningHours.OpenNow {
				text.WriteString(" | open now")
			} else {
				text.WriteString(" | closed now")
			}
		}
		if hours := strings.Join(p.RegularOpeningHours.WeekdayDescriptions, "; "); hours != "" {
			text.WriteString(" | hours: " + hours)
		}
		if p.WebsiteURI != "" {
			text.WriteString(" | " + p.WebsiteURI)
		}
		text.WriteString("\n")
	}
	return text.String()
}
