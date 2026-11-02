// Package chatcore: what the Telegram and WhatsApp modules share (on the M5 the two files
// telegram.cpp and whatsapp.cpp carry the same code twice): the request handed to the backend
// agent, the per-person chat state with its persistence and hand-over to the memory, message
// splitting and the small file helpers.
package chatcore

import (
	"context"
	"encoding/base64"
	"strings"

	"echo-assistant/internal/textutil"
)

// Attachment is a document a person sent (agent::File on the M5).
type Attachment struct {
	Name string // file name
	MIME string // MIME type, "" if unknown
	Data []byte
}

// Request is everything the M5's chat() hands to agent::run for one incoming message. The
// caller (assistant/agent in Go) must answer it as agent::run does:
//
//   - run Task with Instructions as the instruction base, with the tools of the channel
//     tools.Agent, for Person, with Origin = Channel ("Telegram" / "WhatsApp") for sender lines;
//   - continue the Responses thread *Thread (previous_response_id, "" = start a new one) and, after
//     a clean end (no unanswered function calls), store the new response id in *Thread;
//   - show Image (a data URL for input_image) and File (text, PDF or image part) to the model;
//   - return the answer text. A failure is returned as a text that starts with "Error"; the chat
//     then sends it as text without a voice reply.
//
// The call blocks for up to a minute; Ctx ends when Run is stopped.
type Request struct {
	Ctx          context.Context
	Channel      string      // origin for the agent: "Telegram" or "WhatsApp"
	Person       string      // people id of the sender
	Task         string      // the prompt for the agent: Text, with "[Sprachnachricht] " before a voice note and the admin's stranger note in front
	Text         string      // the plain text (typed, transcribed or a photo's caption)
	Voice        bool        // it came as a voice note (the answer goes out as one, too)
	Image        string      // a photo as data URL, "" if none
	File         *Attachment // a document, nil if none
	Instructions string      // CHAT_INSTRUCTIONS or WHATSAPP_CHAT_INSTRUCTIONS
	Thread       *string     // the person's Responses thread, updated by the agent
}

// Readable: whether the model can read a document (text, PDF, image), as agent::readable.
func Readable(mime, name string) bool {
	lower := strings.ToLower(name)
	if IsPDF(mime, name) || strings.HasPrefix(mime, "text/") || ImageType(mime, name) != "" {
		return true
	}
	switch mime {
	case "application/json", "application/xml", "application/x-yaml", "application/csv":
		return true
	}
	for _, ending := range []string{".json", ".csv", ".txt", ".md", ".xml", ".yaml", ".yml", ".log", ".html", ".ini", ".tsv"} {
		if strings.HasSuffix(lower, ending) {
			return true
		}
	}
	return false
}

// IsPDF: a PDF by type or name.
func IsPDF(mime, name string) bool {
	return mime == "application/pdf" || strings.HasSuffix(strings.ToLower(name), ".pdf")
}

// ImageType: the MIME type of an image the model can see, "" if it is none.
func ImageType(mime, name string) string {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return mime
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	}
	return ""
}

// DataURL: data as a "data:<mime>;base64,..." URL for the model.
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// Split cuts text into messages of at most max bytes, at line breaks where possible (not in the
// first half of a message) and never inside a UTF-8 character.
func Split(text string, max int) []string {
	var messages []string
	for len(text) > max {
		head := textutil.Prefix(text, max)
		if cut := strings.LastIndex(head, "\n"); cut > max/2 {
			head = head[:cut]
		}
		if head == "" { // max smaller than one character: avoid looping forever
			break
		}
		messages = append(messages, head)
		text = strings.TrimLeft(text[len(head):], "\n")
	}
	if text != "" {
		messages = append(messages, text)
	}
	return messages
}

// MarkdownLinks calls f for each Markdown link "[label](http...)" in text, as the web search adds
// its sources, and replaces it with f's result; other text stays. f gets label and url.
func MarkdownLinks(text string, f func(label, url string) string) string {
	var out strings.Builder
	from := 0
	for {
		open := indexFrom(text, "[", from)
		if open < 0 {
			break
		}
		middle := indexFrom(text, "](", open)
		closing := -1
		if middle >= 0 {
			closing = indexFrom(text, ")", middle+2)
		}
		var url, label string
		if closing >= 0 {
			url = text[middle+2 : closing]
		}
		if middle >= 0 {
			label = text[open+1 : middle]
		}
		if !strings.HasPrefix(url, "http") || strings.Contains(label, "\n") || strings.Contains(label, "[") {
			out.WriteString(text[from : open+1])
			from = open + 1
			continue
		}
		out.WriteString(text[from:open])
		out.WriteString(f(label, url))
		from = closing + 1
	}
	out.WriteString(text[from:])
	return out.String()
}

func indexFrom(s, sub string, from int) int {
	if from > len(s) {
		return -1
	}
	i := strings.Index(s[from:], sub)
	if i < 0 {
		return -1
	}
	return from + i
}

// StripSources removes the web search's sources "([site](url))" from text (not read out).
func StripSources(text string) string {
	for {
		at := strings.Index(text, "([")
		if at < 0 {
			return text
		}
		end := indexFrom(text, "))", at)
		if end < 0 {
			return text
		}
		text = text[:at] + text[end+2:]
	}
}

// StripAddresses removes bare http(s) addresses (up to the next white space) from text.
func StripAddresses(text string) string {
	for _, scheme := range []string{"https://", "http://"} {
		for {
			at := strings.Index(text, scheme)
			if at < 0 {
				break
			}
			end := at
			for end < len(text) && !isSpace(text[end]) {
				end++
			}
			text = text[:at] + text[end:]
		}
	}
	return text
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// Spoken finishes a text for a voice reply: small clean-ups, trimmed, cut after maxChars bytes
// with a pointer to the chat (the written answer comes as well).
func Spoken(text string, maxChars int) string {
	text = strings.NewReplacer(" .", ".", " ,", ",").Replace(text)
	text = strings.TrimSpace(text)
	if len(text) > maxChars {
		text = textutil.Prefix(text, maxChars) + " … Den Rest findest du im Chat."
	}
	return text
}
