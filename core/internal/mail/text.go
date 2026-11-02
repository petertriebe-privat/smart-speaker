package mail

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// invisible: code points newsletters put between words by the hundred: soft hyphens, grapheme
// joiners, zero-width spaces and joiners, directional marks, word joiners, the BOM.
func invisible(r rune) bool {
	return r == 0xAD || r == 0x34F || (r >= 0x200B && r <= 0x200F) || r == 0x2060 || r == 0xFEFF
}

// CleanText is text without invisible characters, runs of spaces collapsed, trimmed.
func CleanText(text string) string {
	var out []byte
	endsWithBlank := func() bool {
		return len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\n')
	}
	for _, r := range text {
		switch {
		case invisible(r):
		case r == 0xA0: // no-break space
			if len(out) == 0 || out[len(out)-1] != ' ' {
				out = append(out, ' ')
			}
		case (r == ' ' || r == '\t') && endsWithBlank():
		default:
			out = utf8.AppendRune(out, r)
		}
	}
	return strings.TrimSpace(string(out))
}

// namedEntities: the HTML entities mails use in practice.
var namedEntities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "nbsp": " ", "quot": "\"", "apos": "'", "auml": "ä", "ouml": "ö",
	"uuml": "ü", "Auml": "Ä", "Ouml": "Ö", "Uuml": "Ü", "szlig": "ß", "euro": "€", "ndash": "–",
	"mdash": "—", "bdquo": "„", "ldquo": "“", "rdquo": "”", "hellip": "…", "zwnj": "", "zwj": "",
	"shy": "", "copy": "©",
}

func appendCodePoint(text []byte, c uint32) []byte {
	if c == 0 || invisible(rune(c)) || c >= 0x110000 {
		return text
	}
	if c == 0xA0 {
		c = ' '
	}
	return utf8.AppendRune(text, rune(c))
}

// asciiLower lower-cases ASCII letters only, so byte offsets in the result match the input
// (strings.ToLower changes the length of some letters).
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// HTMLToText is the plain text of an HTML body: block tags as line breaks, list items as dashes,
// no style or script, entities decoded, whitespace collapsed.
func HTMLToText(html string) string {
	text := make([]byte, 0, len(html)/2)
	blankEnd := func() bool { return len(text) > 0 && (text[len(text)-1] == ' ' || text[len(text)-1] == '\n') }
	for i := 0; i < len(html); {
		c := html[i]
		switch {
		case c == '<':
			end := strings.IndexByte(html[i:], '>')
			if end < 0 {
				i = len(html)
				break
			}
			tag := asciiLower(html[i+1 : i+end])
			i += end + 1
			// Content that is no text: skip to the closing tag.
			for _, skipped := range []string{"style", "script", "head", "title"} {
				n := len(skipped)
				if !strings.HasPrefix(tag, skipped) || (len(tag) > n && isAlnum(tag[n])) {
					continue
				}
				rest := asciiLower(html[i:])
				at := strings.Index(rest, "</"+skipped)
				if at < 0 {
					i = len(html)
				} else if close := strings.IndexByte(rest[at:], '>'); close < 0 {
					i = len(html)
				} else {
					i += at + close + 1
				}
				tag = ""
				break
			}
			switch {
			case strings.HasPrefix(tag, "li"):
				text = append(text, "\n- "...)
			case strings.HasPrefix(tag, "br"), strings.HasPrefix(tag, "/p"), strings.HasPrefix(tag, "/h"),
				strings.HasPrefix(tag, "/tr"), strings.HasPrefix(tag, "/ul"), strings.HasPrefix(tag, "/ol"),
				strings.HasPrefix(tag, "/div"), strings.HasPrefix(tag, "/table"):
				text = append(text, '\n')
			case strings.HasPrefix(tag, "/td"), strings.HasPrefix(tag, "/th"):
				text = append(text, "  "...)
			}
		case c == '&':
			end := strings.IndexByte(html[i:], ';')
			if end > 1 && end <= 10 {
				entity := html[i+1 : i+end]
				known := false
				if entity[0] == '#' {
					digits, base := entity[1:], 10
					if len(digits) > 0 && (digits[0] == 'x' || digits[0] == 'X') {
						digits, base = digits[1:], 16
					}
					n, _ := strconv.ParseUint(digits, base, 32)
					text = appendCodePoint(text, uint32(n))
					known = true
				} else if v, ok := namedEntities[entity]; ok {
					text = append(text, v...)
					known = true
				}
				if known {
					i += end + 1
					break
				}
			}
			text = append(text, c)
			i++
		case c == '\r':
			i++
		case c == '\n' || c == '\t' || c == ' ':
			if !blankEnd() && len(text) > 0 {
				text = append(text, ' ')
			}
			i++
		default:
			text = append(text, c)
			i++
		}
	}
	// Lines trimmed, at most one empty line in a row.
	var out strings.Builder
	empty := false
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			empty = true
			continue
		}
		if out.Len() > 0 {
			if empty {
				out.WriteString("\n\n")
			} else {
				out.WriteString("\n")
			}
		}
		out.WriteString(line)
		empty = false
	}
	return CleanText(out.String())
}
