package main

import (
	"fmt"
	"time"
)

// The time and the date as quick commands: the device knows them and answers itself, without
// GPT Live ("Wie spät ist es?" took ~10 s through GPT Live, 28.10.2026).

var (
	weekdays = []string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}
	months   = []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September",
		"Oktober", "November", "Dezember"}
)

// clockAnswer: what Jarvis says to "time" or "date" at now.
func clockAnswer(action string, now time.Time) string {
	if action == "date" {
		return fmt.Sprintf("Heute ist %s, der %d. %s.", weekdays[now.Weekday()], now.Day(), months[now.Month()-1])
	}
	if now.Minute() == 0 {
		return fmt.Sprintf("Es ist %d Uhr.", now.Hour())
	}
	return fmt.Sprintf("Es ist %d Uhr %d.", now.Hour(), now.Minute())
}
