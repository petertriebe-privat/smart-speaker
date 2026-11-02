package maps

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

var weekdays = [...]string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}

type temperature struct {
	Degrees float64 `json:"degrees"`
}

// conditionsDoc: the parts every forecast shares (condition, rain, wind, thunderstorm).
type conditionsDoc struct {
	WeatherCondition struct {
		Description textValue `json:"description"`
	} `json:"weatherCondition"`
	Precipitation struct {
		Probability struct {
			Percent float64 `json:"percent"`
		} `json:"probability"`
		Qpf struct {
			Quantity float64 `json:"quantity"`
		} `json:"qpf"`
	} `json:"precipitation"`
	Wind struct {
		Speed struct {
			Value float64 `json:"value"`
		} `json:"speed"`
		Gust struct {
			Value float64 `json:"value"`
		} `json:"gust"`
	} `json:"wind"`
	ThunderstormProbability float64 `json:"thunderstormProbability"`
}

type dateDoc struct {
	Year  *int `json:"year"`
	Month *int `json:"month"`
	Day   *int `json:"day"`
}

// fromUTC reads "2026-09-30T05:19:42.43Z"; the zero time if malformed.
func fromUTC(stamp string) time.Time {
	if len(stamp) < 19 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", stamp[:19], time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

// dayLabel: {"year", "month", "day"} -> "Mi 30.09."
func dayLabel(d dateDoc) string {
	or := func(v *int, def int) int {
		if v == nil {
			return def
		}
		return *v
	}
	noon := time.Date(or(d.Year, 1970), time.Month(or(d.Month, 1)), or(d.Day, 1), 12, 0, 0, 0, time.Local)
	return weekdays[noon.Weekday()] + noon.Format(" 02.01.")
}

func celsius(t temperature) string { return fmt.Sprintf("%.1f °C", t.Degrees) }

func conditions(c conditionsDoc) string {
	text := c.WeatherCondition.Description.Text
	if text == "" {
		text = "?"
	}
	text += fmt.Sprintf(", rain %d %%", int(c.Precipitation.Probability.Percent))
	if amount := c.Precipitation.Qpf.Quantity; amount >= 0.1 {
		text += fmt.Sprintf(" (%.1f mm)", amount)
	}
	text += fmt.Sprintf(", wind %d km/h", int(c.Wind.Speed.Value))
	if gusts := int(c.Wind.Gust.Value); gusts >= 40 {
		text += fmt.Sprintf(" (gusts %d)", gusts)
	}
	if thunder := int(c.ThunderstormProbability); thunder >= 20 {
		text += fmt.Sprintf(", thunderstorm %d %%", thunder)
	}
	return text
}

func constrain(v, lo, hi int) int { return max(lo, min(v, hi)) }

type spot struct {
	latitude, longitude float64
	name                string
}

// locate: home, or a place looked up with Places (one request of that quota). The second result
// is the error text.
func (s *Service) locate(place string) (spot, string) {
	if isHome(place) {
		lat, long, ok := s.home()
		if !ok {
			return spot{}, "Error: home is unknown (Home Assistant's zone.home): give a place"
		}
		return spot{lat, long, "home"}, ""
	}
	body, _ := json.Marshal(map[string]any{"textQuery": place, "languageCode": "de", "pageSize": 1})
	var parsed struct {
		Places []struct {
			Location struct {
				Latitude  *float64 `json:"latitude"`
				Longitude *float64 `json:"longitude"`
			} `json:"location"`
			FormattedAddress string `json:"formattedAddress"`
		} `json:"places"`
	}
	if msg := s.request("places", prompts.Int("PLACES_MONTHLY_LIMIT", 1000), placesHost,
		"/v1/places:searchText", "places.location,places.formattedAddress", body, &parsed); msg != "" {
		return spot{}, msg
	}
	if len(parsed.Places) == 0 || parsed.Places[0].Location.Latitude == nil {
		return spot{}, "Error: place not found: " + place
	}
	found := parsed.Places[0]
	name := found.FormattedAddress
	if name == "" {
		name = place
	}
	lon := 0.0
	if found.Location.Longitude != nil {
		lon = *found.Location.Longitude
	}
	return spot{*found.Location.Latitude, lon, name}, ""
}

func (s *Service) weatherNow(where string) string {
	var doc struct {
		conditionsDoc
		Temperature          temperature `json:"temperature"`
		FeelsLikeTemperature temperature `json:"feelsLikeTemperature"`
		RelativeHumidity     float64     `json:"relativeHumidity"`
		UVIndex              float64     `json:"uvIndex"`
		History              struct {
			Min temperature `json:"minTemperature"`
			Max temperature `json:"maxTemperature"`
		} `json:"currentConditionsHistory"`
	}
	if msg := s.request("weather", prompts.Int("WEATHER_MONTHLY_LIMIT", 9000), weatherHost,
		"/v1/currentConditions:lookup?"+where, "", nil, &doc); msg != "" {
		return "Now: " + msg + "\n"
	}
	return "Now: " + celsius(doc.Temperature) + " (feels like " + celsius(doc.FeelsLikeTemperature) + "), " +
		conditions(doc.conditionsDoc) + fmt.Sprintf(", humidity %d %%, UV %d", int(doc.RelativeHumidity), int(doc.UVIndex)) +
		"; last 24 h " + celsius(doc.History.Min) + " to " + celsius(doc.History.Max) + ".\n"
}

func (s *Service) weatherHours(where string, hours int) string {
	var doc struct {
		ForecastHours []struct {
			conditionsDoc
			DisplayDateTime struct {
				Hours int `json:"hours"`
			} `json:"displayDateTime"`
			Temperature temperature `json:"temperature"`
		} `json:"forecastHours"`
	}
	if msg := s.request("weather", prompts.Int("WEATHER_MONTHLY_LIMIT", 9000), weatherHost,
		fmt.Sprintf("/v1/forecast/hours:lookup?%s&hours=%d&pageSize=%d", where, hours, hours), "", nil, &doc); msg != "" {
		return "Hourly: " + msg + "\n"
	}
	text := "Hourly:\n"
	for _, h := range doc.ForecastHours {
		text += fmt.Sprintf("- %02d:00 %s, %s\n", h.DisplayDateTime.Hours, celsius(h.Temperature), conditions(h.conditionsDoc))
	}
	return text
}

func (s *Service) weatherDays(where string, days int) string {
	type part struct {
		conditionsDoc
		UVIndex float64 `json:"uvIndex"`
	}
	var doc struct {
		ForecastDays []struct {
			DisplayDate       dateDoc     `json:"displayDate"`
			MinTemperature    temperature `json:"minTemperature"`
			MaxTemperature    temperature `json:"maxTemperature"`
			DaytimeForecast   part        `json:"daytimeForecast"`
			NighttimeForecast part        `json:"nighttimeForecast"`
			SunEvents         struct {
				Sunrise string `json:"sunriseTime"`
				Sunset  string `json:"sunsetTime"`
			} `json:"sunEvents"`
		} `json:"forecastDays"`
	}
	if msg := s.request("weather", prompts.Int("WEATHER_MONTHLY_LIMIT", 9000), weatherHost,
		fmt.Sprintf("/v1/forecast/days:lookup?%s&days=%d&pageSize=%d", where, days, days), "", nil, &doc); msg != "" {
		return "Daily: " + msg + "\n"
	}
	text := "Daily:\n"
	for _, d := range doc.ForecastDays {
		text += fmt.Sprintf("- %s: %s to %s; day %s, UV %d; night %s", dayLabel(d.DisplayDate), celsius(d.MinTemperature),
			celsius(d.MaxTemperature), conditions(d.DaytimeForecast.conditionsDoc), int(d.DaytimeForecast.UVIndex),
			conditions(d.NighttimeForecast.conditionsDoc))
		rise, set := fromUTC(d.SunEvents.Sunrise), fromUTC(d.SunEvents.Sunset)
		if !rise.IsZero() && !set.IsZero() {
			text += "; sunrise " + clockText(rise) + ", sunset " + clockText(set)
		}
		text += "\n"
	}
	return text
}

func (s *Service) weatherAlerts(where string) string {
	var doc struct {
		WeatherAlerts []struct {
			AlertTitle     textValue `json:"alertTitle"`
			Severity       string    `json:"severity"`
			StartTime      string    `json:"startTime"`
			ExpirationTime string    `json:"expirationTime"`
			AreaName       string    `json:"areaName"`
			Description    string    `json:"description"`
			Instruction    any       `json:"instruction"` // a list of texts
		} `json:"weatherAlerts"`
	}
	if msg := s.request("weather", prompts.Int("WEATHER_MONTHLY_LIMIT", 9000), weatherHost,
		"/v1/publicAlerts:lookup?"+where, "", nil, &doc); msg != "" {
		return "Warnings: " + msg + "\n"
	}
	if len(doc.WeatherAlerts) == 0 {
		return "Official weather warnings: none.\n"
	}
	text := "Official weather warnings:\n"
	for _, a := range doc.WeatherAlerts {
		title := a.AlertTitle.Text
		if title == "" {
			title = "?"
		}
		text += "- " + title + " (" + a.Severity + ")"
		from, until := fromUTC(a.StartTime), fromUTC(a.ExpirationTime)
		if !from.IsZero() && !until.IsZero() {
			text += " " + from.Local().Format("02.01. 15:04") + " to " + until.Local().Format("02.01. 15:04")
		}
		if a.AreaName != "" {
			text += ", " + a.AreaName
		}
		if d := strings.ReplaceAll(a.Description, "\n", " "); d != "" {
			text += ": " + textutil.Prefix(d, 300)
		}
		if list, ok := a.Instruction.([]any); ok && len(list) > 0 {
			if first, ok := list[0].(string); ok && first != "" {
				text += " Advice: " + textutil.Prefix(first, 200)
			}
		}
		text += "\n"
	}
	return text
}

func (s *Service) pollen(where string, days int) string {
	var doc struct {
		DailyInfo []struct {
			Date        dateDoc `json:"date"`
			PollenTypes []struct {
				DisplayName string `json:"displayName"`
				InSeason    *bool  `json:"inSeason"`
				IndexInfo   *struct {
					Value    int    `json:"value"`
					Category string `json:"category"`
				} `json:"indexInfo"`
				HealthRecommendations []string `json:"healthRecommendations"`
			} `json:"pollenTypeInfo"`
			Plants []struct {
				DisplayName string `json:"displayName"`
				IndexInfo   struct {
					Value int `json:"value"`
				} `json:"indexInfo"`
			} `json:"plantInfo"`
		} `json:"dailyInfo"`
	}
	if msg := s.request("pollen", prompts.Int("POLLEN_MONTHLY_LIMIT", 4500), pollenHost,
		fmt.Sprintf("/v1/forecast:lookup?%s&days=%d&plantsDescription=false", where, min(days, 5)), "", nil, &doc); msg != "" {
		return "Pollen: " + msg + "\n"
	}
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	text := "Pollen (Universal Pollen Index 0-5):\n"
	for _, day := range doc.DailyInfo {
		text += "- " + dayLabel(day.Date) + ":"
		advice := ""
		highest := 0
		for _, t := range day.PollenTypes {
			if t.IndexInfo == nil {
				continue
			}
			text += fmt.Sprintf(" %s %d %s", or(t.DisplayName, "?"), t.IndexInfo.Value, t.IndexInfo.Category)
			if t.InSeason != nil && !*t.InSeason {
				text += " (out of season)"
			}
			text += ";"
			if t.IndexInfo.Value > highest {
				highest = t.IndexInfo.Value
				advice = ""
				if len(t.HealthRecommendations) > 0 {
					advice = t.HealthRecommendations[0]
				}
			}
		}
		var plants []string
		for _, p := range day.Plants {
			if p.IndexInfo.Value > 0 {
				plants = append(plants, fmt.Sprintf("%s %d", or(p.DisplayName, "?"), p.IndexInfo.Value))
			}
		}
		if len(plants) > 0 {
			text += " plants: " + strings.Join(plants, ", ") + ";"
		}
		if highest >= 3 && advice != "" {
			text += " advice: " + advice
		}
		text += "\n"
	}
	return text
}

func (s *Service) airQuality(at spot) string {
	body, _ := json.Marshal(map[string]any{
		"location":          map[string]any{"latitude": at.latitude, "longitude": at.longitude},
		"languageCode":      "de",
		"extraComputations": []string{"LOCAL_AQI", "HEALTH_RECOMMENDATIONS", "DOMINANT_POLLUTANT_CONCENTRATION"},
	})
	var doc struct {
		Indexes []struct {
			DisplayName string  `json:"displayName"`
			AqiDisplay  *string `json:"aqiDisplay"`
			Category    string  `json:"category"`
		} `json:"indexes"`
		Pollutants []struct {
			FullName      string `json:"fullName"`
			Concentration struct {
				Value float64 `json:"value"`
				Units string  `json:"units"`
			} `json:"concentration"`
		} `json:"pollutants"`
		HealthRecommendations struct {
			GeneralPopulation string `json:"generalPopulation"`
		} `json:"healthRecommendations"`
	}
	if msg := s.request("air", prompts.Int("AIR_QUALITY_MONTHLY_LIMIT", 9000), airQualityHost,
		"/v1/currentConditions:lookup", "", body, &doc); msg != "" {
		return "Air quality: " + msg + "\n"
	}
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	text := "Air quality:"
	for _, index := range doc.Indexes {
		text += " " + or(index.DisplayName, "?")
		if index.AqiDisplay != nil {
			text += " " + *index.AqiDisplay
		}
		text += " " + index.Category + ";"
	}
	if len(doc.Pollutants) > 0 {
		p := doc.Pollutants[0]
		unit := ""
		switch p.Concentration.Units {
		case "PARTS_PER_BILLION":
			unit = " ppb"
		case "MICROGRAMS_PER_CUBIC_METER":
			unit = " µg/m³"
		}
		text += fmt.Sprintf(" dominant %s %.0f%s;", or(p.FullName, "?"), p.Concentration.Value, unit)
	}
	if advice := doc.HealthRecommendations.GeneralPopulation; advice != "" {
		text += " advice: " + advice
	}
	return text + "\n"
}

func (s *Service) getWeather(args map[string]any, _ tools.Context) string {
	include, _ := args["include"].([]any)
	if len(include) == 0 {
		return "Error: include missing (now, hours, days, alerts, pollen, air)"
	}
	wants := func(part string) bool {
		for _, item := range include {
			if item == part {
				return true
			}
		}
		return false
	}
	at, msg := s.locate(tools.String(args, "place"))
	if msg != "" {
		return msg
	}
	where := fmt.Sprintf("location.latitude=%.5f&location.longitude=%.5f&languageCode=de", at.latitude, at.longitude)
	metric := where + "&unitsSystem=METRIC"
	days, ok := tools.Int(args, "days")
	if !ok {
		days = 3
	}
	days = constrain(days, 1, 10)
	hours, ok := tools.Int(args, "hours")
	if !ok {
		hours = 12
	}
	text := "Weather for " + at.name + " (" + time.Now().Format("02.01. 15:04") + "):\n"
	if wants("now") {
		text += s.weatherNow(metric)
	}
	if wants("hours") {
		text += s.weatherHours(metric, constrain(hours, 1, 24))
	}
	if wants("days") {
		text += s.weatherDays(metric, days)
	}
	if wants("alerts") {
		text += s.weatherAlerts(where)
	}
	if wants("pollen") {
		text += s.pollen(where, days)
	}
	if wants("air") {
		text += s.airQuality(at)
	}
	return text
}
