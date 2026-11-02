package timers

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Command is a timer request the device runs itself, without GPT Live (#25).
type Command struct {
	Action string        // "set" (timer), "alarm", "cancel", "" = none
	After  time.Duration // set: how long
	Hour   int           // alarm: when (today or tomorrow)
	Minute int
	Label  string // set, alarm: what it is for ("Nudeln"), "" = none
}

var numberWords = map[string]int{
	"null": 0, "ein": 1, "eins": 1, "eine": 1, "einen": 1, "einer": 1, "zwei": 2, "drei": 3, "vier": 4,
	"fünf": 5, "sechs": 6, "sieben": 7, "acht": 8, "neun": 9, "zehn": 10, "elf": 11, "zwölf": 12,
	"dreizehn": 13, "vierzehn": 14, "fünfzehn": 15, "sechzehn": 16, "siebzehn": 17, "achtzehn": 18,
	"neunzehn": 19, "zwanzig": 20, "dreißig": 30, "vierzig": 40, "fünfzig": 50, "sechzig": 60,
	"neunzig": 90,
}

// number reads "10", "zehn", "fünfundzwanzig", "einundzwanzig"; ok false otherwise.
func number(word string) (int, bool) {
	if n, err := strconv.Atoi(word); err == nil {
		return n, true
	}
	if n, ok := numberWords[word]; ok {
		return n, true
	}
	if ones, tens, found := strings.Cut(word, "und"); found {
		o, ok1 := numberWords[ones]
		t, ok2 := numberWords[tens]
		if ok1 && ok2 && o < 10 && t >= 20 && t%10 == 0 {
			return t + o, true
		}
	}
	return 0, false
}

var (
	unitSeconds = map[string]time.Duration{
		"sekunde": time.Second, "sekunden": time.Second,
		"minute": time.Minute, "minuten": time.Minute, "min": time.Minute,
		"stunde": time.Hour, "stunden": time.Hour,
	}
	wordRe = regexp.MustCompile(`[\p{L}\d]+`)
	timeRe = regexp.MustCompile(`\b(\d{1,2})[:.](\d{2})\b`)
)

// cancelWords: "Timer aus", "stopp den Wecker", "lösch den Timer".
var cancelWords = []string{"aus", "stopp", "stop", "abbrechen", "löschen", "lösch", "beenden", "beende", "weg", "ab"}

// Parse: the timer command the whole utterance is, as GPT Live's or the device's transcript
// gives it ("Stell einen Timer auf zehn Minuten.", "Timer 5 Minuten für die Nudeln",
// "Weck mich um 6:30", "Wecker auf sieben Uhr", "Timer aus"). Anything it is not sure of
// stays with GPT Live, which has the tools too.
func Parse(text string) Command {
	lower := strings.ToLower(strings.TrimSpace(text))
	words := wordRe.FindAllString(lower, -1)
	if len(words) == 0 || len(words) > 14 {
		return Command{}
	}
	has := func(list ...string) bool {
		for _, w := range words {
			for _, l := range list {
				if w == l {
					return true
				}
			}
		}
		return false
	}
	timerWord := has("timer", "kurzzeitwecker", "eieruhr")
	alarmWord := has("wecker", "weck", "wecke", "wecken", "alarm")
	if !timerWord && !alarmWord {
		return Command{}
	}
	label := labelOf(lower)
	if has(cancelWords...) && !has("in", "um", "auf", "für") || has("abbrechen", "löschen", "lösch") {
		return Command{Action: "cancel"}
	}
	if alarmWord && !timerWord {
		if h, m, ok := clock(lower, words); ok {
			return Command{Action: "alarm", Hour: h, Minute: m, Label: label}
		}
		if d := duration(words); d > 0 && has("in") { // "weck mich in 20 Minuten"
			return Command{Action: "set", After: d, Label: label}
		}
		return Command{}
	}
	if d := duration(words); d > 0 {
		return Command{Action: "set", After: d, Label: label}
	}
	return Command{}
}

// duration: "zehn Minuten", "1 Stunde 30 Minuten", "eine halbe Stunde", "anderthalb Stunden",
// "eine Viertelstunde"; 0 if none.
func duration(words []string) time.Duration {
	var total time.Duration
	for i, w := range words {
		switch w {
		case "viertelstunde":
			total += 15 * time.Minute
			continue
		case "dreiviertelstunde":
			total += 45 * time.Minute
			continue
		}
		unit, ok := unitSeconds[w]
		if !ok || i == 0 {
			continue
		}
		prev := words[i-1]
		switch {
		case prev == "halbe" || prev == "halben":
			total += unit / 2
		case prev == "anderthalb" || prev == "eineinhalb":
			total += unit * 3 / 2
		default:
			if n, ok := number(prev); ok {
				total += time.Duration(n) * unit
			} else if prev == "eine" || prev == "einer" {
				total += unit
			}
		}
	}
	if total > 24*time.Hour {
		return 0
	}
	return total
}

// clock: "6:30", "6.30 Uhr", "sieben Uhr", "7 Uhr 15", "halb sieben".
func clock(lower string, words []string) (hour, minute int, ok bool) {
	if m := timeRe.FindStringSubmatch(lower); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		return h, mi, h < 24 && mi < 60
	}
	for i, w := range words {
		if w == "halb" && i+1 < len(words) {
			if h, ok := number(words[i+1]); ok && h >= 1 && h <= 24 {
				return (h - 1) % 24, 30, true
			}
		}
		if w == "uhr" && i > 0 {
			h, ok := number(words[i-1])
			if !ok || h > 23 {
				continue
			}
			mi := 0
			if i+1 < len(words) {
				if n, ok := number(words[i+1]); ok && n < 60 {
					mi = n
				}
			}
			return h, mi, true
		}
	}
	return 0, 0, false
}

var labelRe = regexp.MustCompile(`\bfür (?:den |die |das |meine |meinen |mein )?([\p{L}][\p{L} -]{1,30}?)[.!?]*$`)

// labelOf: "… für die Nudeln" -> "Nudeln"; a duration after "für" is no label.
func labelOf(lower string) string {
	m := labelRe.FindStringSubmatch(lower)
	if m == nil {
		return ""
	}
	label := strings.TrimSpace(m[1])
	for _, w := range strings.Fields(label) {
		if _, unit := unitSeconds[w]; unit {
			return ""
		}
		if _, n := number(w); n {
			return ""
		}
	}
	r := []rune(label)
	if len(r) == 0 {
		return ""
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
