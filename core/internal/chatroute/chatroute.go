// Package chatroute sends images, files and messages to a person's chat on the channel that
// reaches them (#34): Telegram or WhatsApp, in the order of people.Register.ChatOrder (their
// notify setting, else Telegram when their account is known, else WhatsApp), the other one if
// the first fails. Router has the shape of telegram.Bot's sending side, so images and files take
// it in place of the bot.
package chatroute

import (
	"html"
	"regexp"
	"strings"

	"echo-assistant/internal/people"
)

// Sender is one chat channel (*telegram.Bot, *whatsapp.Service).
type Sender interface {
	Configured() bool
	Send(text, person string) string
	SendPhoto(jpeg []byte, caption, person string) string
	SendDocument(data []byte, filename, mime, caption, person string) string
}

// Router picks the channel per person. Telegram's Send takes Telegram HTML; for WhatsApp it is
// turned into WhatsApp's formatting.
type Router struct {
	People   *people.Register
	Telegram Sender // nil = not set up
	WhatsApp Sender // nil = not set up
}

// Configured: some chat channel is set up.
func (r *Router) Configured() bool {
	return ok(r.Telegram) || ok(r.WhatsApp)
}

func ok(s Sender) bool { return s != nil && s.Configured() }

// try runs send on the person's channels in order until one says "sent".
func (r *Router) try(person string, send func(s Sender, whatsapp bool) string) string {
	var errs []string
	for _, channel := range r.People.ChatOrder(person) {
		s, whatsapp := r.Telegram, false
		if channel == "whatsapp" {
			s, whatsapp = r.WhatsApp, true
		}
		if !ok(s) {
			continue
		}
		result := send(s, whatsapp)
		if result == "sent" {
			return result
		}
		errs = append(errs, channel+": "+result)
	}
	if len(errs) == 0 {
		return "Error: this person has neither Telegram nor WhatsApp."
	}
	return "Error: " + strings.Join(errs, "; ")
}

// Send sends Telegram HTML to a person's chat ("" = the admin).
func (r *Router) Send(htmlText, person string) string {
	return r.try(person, func(s Sender, whatsapp bool) string {
		if whatsapp {
			return s.Send(WhatsAppText(htmlText), person)
		}
		return s.Send(htmlText, person)
	})
}

// SendPhoto sends a JPEG with a plain caption.
func (r *Router) SendPhoto(jpeg []byte, caption, person string) string {
	return r.try(person, func(s Sender, _ bool) string { return s.SendPhoto(jpeg, caption, person) })
}

// SendDocument sends a file with a plain caption.
func (r *Router) SendDocument(data []byte, filename, mime, caption, person string) string {
	return r.try(person, func(s Sender, _ bool) string { return s.SendDocument(data, filename, mime, caption, person) })
}

var (
	link = regexp.MustCompile(`(?is)<a\s+href="([^"]*)"[^>]*>(.*?)</a>`)
	bold = regexp.MustCompile(`(?i)</?(b|strong)>`)
	ital = regexp.MustCompile(`(?i)</?(i|em)>`)
	code = regexp.MustCompile(`(?i)</?(code|pre)>`)
	tag  = regexp.MustCompile(`<[^>]*>`)
)

// WhatsAppText turns Telegram HTML into WhatsApp's formatting: *bold*, _italic_, `code`, links
// as "text (url)", other tags dropped, entities decoded.
func WhatsAppText(h string) string {
	h = link.ReplaceAllStringFunc(h, func(m string) string {
		parts := link.FindStringSubmatch(m)
		target, text := html.UnescapeString(parts[1]), strings.TrimSpace(tag.ReplaceAllString(parts[2], ""))
		if text == "" || html.UnescapeString(text) == target {
			return target
		}
		return text + " (" + target + ")"
	})
	h = bold.ReplaceAllString(h, "*")
	h = ital.ReplaceAllString(h, "_")
	h = code.ReplaceAllString(h, "`")
	h = tag.ReplaceAllString(h, "")
	return html.UnescapeString(h)
}
