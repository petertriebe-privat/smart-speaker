package chatcore

import (
	"fmt"
	"strings"
	"time"

	"echo-assistant/internal/people"
	"echo-assistant/internal/tools"
)

// How says in which form a message came, for the transcript ("per Telegram, Sprachnachricht").
func How(label string, voice bool, image string, file *Attachment) string {
	switch {
	case voice:
		return "per " + label + ", Sprachnachricht"
	case image != "":
		return "per " + label + ", mit Foto"
	case file != nil:
		return fmt.Sprintf("per %s, mit Datei %s (%d KB)", label, file.Name, len(file.Data)/1024)
	}
	return "per " + label
}

// FromLine is tools::fromLine of the M5: the line that tells a recipient who a message is from
// ("Von Anna (Lautsprecher, 16.10. 09:30):"); "" when the sender is the recipient or unknown and
// not at the speaker (a task or the console is the admin's).
func FromLine(reg *people.Register, ctx tools.Context, recipient string) string {
	to := recipient
	if to == "" {
		to = reg.Admin()
	}
	voice := ctx.Origin == "Lautsprecher" || strings.HasPrefix(ctx.Origin, "Anruf")
	if (ctx.Person == "" && !voice) || to == ctx.Person {
		return ""
	}
	who := reg.Name(ctx.Person)
	if who == "" {
		if ctx.Origin == "Lautsprecher" {
			who = "jemandem zu Hause"
		} else {
			who = "jemandem"
		}
	}
	origin := ""
	if ctx.Origin != "" {
		origin = ctx.Origin + ", "
	}
	return "Von " + who + " (" + origin + time.Now().Format("02.01. 15:04") + "):"
}

// Recipient resolves the "to" argument of send_telegram / send_whatsapp: the person's id, "" =
// the caller (ctx.Person). errText is the error for the model if the name is unknown.
func Recipient(reg *people.Register, to string, ctx tools.Context) (person, errText string) {
	if to == "" {
		return ctx.Person, ""
	}
	person = reg.ByName(to)
	if person == "" {
		return "", "Error: " + to + " is not registered as a person."
	}
	return person, ""
}
