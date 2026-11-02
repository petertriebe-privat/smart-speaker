package whatsapp

import (
	"strings"

	"echo-assistant/internal/chatcore"
)

// plainLinks turns Markdown links as the web search adds its sources ("([site](https://...))")
// into text: WhatsApp shows bare addresses as links, Markdown raw.
func plainLinks(text string) string {
	return chatcore.MarkdownLinks(text, func(label, url string) string {
		// A source named by its site ("timeanddate.de") is the address itself.
		if site := !strings.Contains(label, " ") && strings.Index(label, ".") > 0; site {
			return url
		}
		return label + " (" + url + ")"
	})
}

// spoken: the answer read out for a voice reply, without formatting marks and links, cut when
// long (the text comes as well).
func spoken(answer string, maxChars int) string {
	text := chatcore.StripSources(answer)
	text = plainLinks(text)
	text = chatcore.StripAddresses(text)
	for _, mark := range []string{"*", "_", "~", "`", "• "} {
		text = strings.ReplaceAll(text, mark, "")
	}
	return chatcore.Spoken(text, maxChars)
}

// waID is a person's WhatsApp id (international, digits only: "49171...") from their numbers: a
// mobile before a landline, never the PBX's internal ones ("**6xx"). "" if none.
func waID(phones []string) string {
	best := ""
	for _, number := range phones {
		if strings.HasPrefix(number, "**") {
			continue
		}
		id := number
		switch {
		case strings.HasPrefix(number, "00"):
			id = number[2:]
		case strings.HasPrefix(number, "0"):
			id = "49" + number[1:]
		}
		if strings.HasPrefix(number, "01") { // German mobile
			return id
		}
		if best == "" {
			best = id
		}
	}
	return best
}
