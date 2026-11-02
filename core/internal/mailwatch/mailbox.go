package mailwatch

import (
	"fmt"
	"strings"
)

// Mailbox is one entry of WATCHED_MAILBOXES: {person, label, host, user, secret}.
type Mailbox struct {
	Person string // name or id in the register, "" = the admin
	Label  string // how the assistant names it ("Gmail", "Work")
	Host   string // IMAP host or "gmail"
	User   string
	Secret string // IMAP password, or the Gmail account's refresh token ("" = the admin's)
}

// Gmail: the mailbox is read over the Gmail API instead of IMAP.
func (b Mailbox) Gmail() bool { return b.Host == "gmail" }

func (b Mailbox) key() string { return b.Label + "|" + b.User }

// ParseMailboxes reads the raw text of the M5's WATCHED_MAILBOXES macro (make_secrets.py keeps
// brace lists as they are): `{"", "Privat", "imap.example.com", "me@example.com", "pw"}, {...}`.
// Values are C string literals (adjacent literals are joined, escapes \" \\ \n \t \r \' are
// understood); a missing trailing value is "". Empty text gives no mailboxes.
func ParseMailboxes(raw string) ([]Mailbox, error) {
	p := &macroParser{s: raw}
	var boxes []Mailbox
	for {
		p.skip(true)
		if p.done() {
			return boxes, nil
		}
		if p.peek() != '{' {
			return nil, p.errorf("expected {")
		}
		p.pos++
		var values []string
		for {
			p.skip(false)
			if p.peek() == '}' {
				p.pos++
				break
			}
			value, err := p.literal()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
			p.skip(false)
			switch p.peek() {
			case ',':
				p.pos++
			case '}':
			default:
				return nil, p.errorf("expected , or }")
			}
		}
		if len(values) < 4 || len(values) > 5 {
			return nil, fmt.Errorf("mailbox entry %d: %d values, expected person, label, host, user[, secret]", len(boxes)+1, len(values))
		}
		for len(values) < 5 {
			values = append(values, "")
		}
		boxes = append(boxes, Mailbox{values[0], values[1], values[2], values[3], values[4]})
	}
}

type macroParser struct {
	s   string
	pos int
}

func (p *macroParser) done() bool { return p.pos >= len(p.s) }

func (p *macroParser) peek() byte {
	if p.done() {
		return 0
	}
	return p.s[p.pos]
}

func (p *macroParser) errorf(format string, args ...any) error {
	return fmt.Errorf("WATCHED_MAILBOXES at %d: %s", p.pos, fmt.Sprintf(format, args...))
}

// skip passes white space and, between entries (commas), the separating commas.
func (p *macroParser) skip(commas bool) {
	for !p.done() {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\\' || (commas && c == ',') {
			p.pos++
			continue
		}
		return
	}
}

// literal reads one or more adjacent C string literals as one value.
func (p *macroParser) literal() (string, error) {
	var out strings.Builder
	n := 0
	for {
		p.skip(false)
		if p.peek() != '"' {
			if n == 0 {
				return "", p.errorf("expected a string")
			}
			return out.String(), nil
		}
		p.pos++
		for {
			if p.done() {
				return "", p.errorf("string not closed")
			}
			c := p.s[p.pos]
			p.pos++
			if c == '"' {
				break
			}
			if c == '\\' && !p.done() {
				e := p.s[p.pos]
				p.pos++
				switch e {
				case 'n':
					c = '\n'
				case 't':
					c = '\t'
				case 'r':
					c = '\r'
				default:
					c = e // \" \\ \'
				}
			}
			out.WriteByte(c)
		}
		n++
	}
}
