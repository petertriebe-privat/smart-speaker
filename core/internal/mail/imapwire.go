package mail

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// This file is the pure part of the IMAP client: reading responses with their literals, parsing
// them into tokens, and the small syntaxes around them (UID sets, folder names, dates). The
// connection itself is in imap.go.

// maxLiteral: the largest literal a response may announce; a rogue server must not make the Echo
// allocate gigabytes.
const maxLiteral = 64 << 20

// literalLength: whether line ends with a literal announcement "{n}\r\n" ("{n+}" too) and how
// many bytes follow it.
func literalLength(line []byte) (int, bool) {
	if !bytes.HasSuffix(line, []byte("}\r\n")) && !bytes.HasSuffix(line, []byte("}\n")) {
		return 0, false
	}
	open := bytes.LastIndexByte(line, '{')
	if open < 0 {
		return 0, false
	}
	digits := bytes.TrimRight(line[open+1:], "}\r\n")
	digits = bytes.TrimSuffix(digits, []byte("+"))
	n, err := strconv.Atoi(string(digits))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// readResponse reads one complete server response: a line plus, for every literal it announces,
// the literal's bytes and the rest of the line. The result keeps the "{n}\r\n" markers and the
// literal bytes in place, so parseTokens can read it later; it ends with CRLF.
func readResponse(br *bufio.Reader) ([]byte, error) {
	var raw []byte
	for {
		line, err := br.ReadBytes('\n')
		raw = append(raw, line...)
		if err != nil {
			if err == io.EOF && len(raw) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return raw, err
		}
		n, ok := literalLength(line)
		if !ok {
			return raw, nil
		}
		if n > maxLiteral {
			return raw, fmt.Errorf("literal of %d bytes", n)
		}
		start := len(raw)
		raw = append(raw, make([]byte, n)...)
		if _, err := io.ReadFull(br, raw[start:]); err != nil {
			return raw, err
		}
	}
}

// parseTokens reads an IMAP response body: atoms, quoted strings and literals become strings, NIL
// becomes nil, parentheses become []any. An atom may contain a bracketed part with spaces
// (BODY[HEADER.FIELDS (FROM TO)]).
func parseTokens(b []byte) ([]any, error) {
	t := &tokenizer{b: b}
	items, err := t.list(0)
	return items, err
}

type tokenizer struct {
	b []byte
	i int
}

var errTokens = errors.New("malformed IMAP response")

func (t *tokenizer) list(depth int) ([]any, error) {
	var items []any
	for t.i < len(t.b) {
		c := t.b[t.i]
		switch {
		case c == ' ' || c == '\r' || c == '\n' || c == '\t':
			t.i++
		case c == '(':
			t.i++
			sub, err := t.list(depth + 1)
			if err != nil {
				return items, err
			}
			items = append(items, sub)
		case c == ')':
			t.i++
			if depth == 0 {
				return items, errTokens
			}
			return items, nil
		case c == '"':
			s, err := t.quoted()
			if err != nil {
				return items, err
			}
			items = append(items, s)
		case c == '{':
			s, err := t.literal()
			if err != nil {
				return items, err
			}
			items = append(items, s)
		default:
			atom := t.atom()
			if atom == "NIL" {
				items = append(items, nil)
			} else {
				items = append(items, atom)
			}
		}
	}
	if depth > 0 {
		return items, errTokens
	}
	return items, nil
}

func (t *tokenizer) quoted() (string, error) {
	t.i++ // opening quote
	var sb strings.Builder
	for t.i < len(t.b) {
		c := t.b[t.i]
		t.i++
		switch c {
		case '\\':
			if t.i < len(t.b) {
				sb.WriteByte(t.b[t.i])
				t.i++
			}
		case '"':
			return sb.String(), nil
		default:
			sb.WriteByte(c)
		}
	}
	return "", errTokens
}

func (t *tokenizer) literal() (string, error) {
	end := bytes.IndexByte(t.b[t.i:], '}')
	if end < 0 {
		return "", errTokens
	}
	digits := strings.TrimSuffix(string(t.b[t.i+1:t.i+end]), "+")
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return "", errTokens
	}
	t.i += end + 1
	if t.i < len(t.b) && t.b[t.i] == '\r' {
		t.i++
	}
	if t.i < len(t.b) && t.b[t.i] == '\n' {
		t.i++
	}
	if t.i+n > len(t.b) {
		return "", errTokens
	}
	s := string(t.b[t.i : t.i+n])
	t.i += n
	return s, nil
}

func (t *tokenizer) atom() string {
	start := t.i
	for t.i < len(t.b) {
		c := t.b[t.i]
		if c == ' ' || c == '(' || c == ')' || c == '\r' || c == '\n' {
			break
		}
		t.i++
		if c == '[' { // BODY[HEADER.FIELDS (FROM TO)]: up to the closing bracket
			for t.i < len(t.b) && t.b[t.i] != ']' {
				t.i++
			}
			if t.i < len(t.b) {
				t.i++
			}
		}
	}
	return string(t.b[start:t.i])
}

// fetchData is one "* n FETCH (...)" response.
type fetchData struct {
	Seq   uint32
	Items map[string]any // upper-case item names; values as parseTokens gives them
}

// parseFetch parses an untagged FETCH response; false if raw is something else.
func parseFetch(raw []byte) (fetchData, bool) {
	if !bytes.HasPrefix(raw, []byte("* ")) {
		return fetchData{}, false
	}
	rest := raw[2:]
	sp := bytes.IndexByte(rest, ' ')
	if sp < 1 {
		return fetchData{}, false
	}
	seq, err := strconv.ParseUint(string(rest[:sp]), 10, 32)
	if err != nil {
		return fetchData{}, false
	}
	rest = rest[sp+1:]
	if len(rest) < 6 || !strings.EqualFold(string(rest[:5]), "FETCH") {
		return fetchData{}, false
	}
	tokens, err := parseTokens(rest[5:])
	if err != nil || len(tokens) == 0 {
		return fetchData{}, false
	}
	list, ok := tokens[0].([]any)
	if !ok {
		return fetchData{}, false
	}
	f := fetchData{Seq: uint32(seq), Items: map[string]any{}}
	for i := 0; i+1 < len(list); i += 2 {
		if key, ok := list[i].(string); ok {
			f.Items[strings.ToUpper(key)] = list[i+1]
		}
	}
	return f, true
}

// UID of the message, 0 if the response has none.
func (f fetchData) UID() uint32 { return f.number("UID") }

// Size is RFC822.SIZE.
func (f fetchData) Size() uint32 { return f.number("RFC822.SIZE") }

func (f fetchData) number(key string) uint32 {
	s, _ := f.Items[key].(string)
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

// Body is the content of the first BODY[...] item (a partial fetch names it BODY[]<0>).
func (f fetchData) Body() (string, bool) {
	for key, v := range f.Items {
		if strings.HasPrefix(key, "BODY[") {
			s, ok := v.(string)
			return s, ok
		}
	}
	return "", false
}

// parseSearch collects the numbers of the "* SEARCH" responses, sorted ascending. found says
// whether the server sent a SEARCH line at all (none = the search failed, not "nothing").
func parseSearch(untagged [][]byte) (numbers []uint32, found bool) {
	for _, raw := range untagged {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(strings.ToUpper(line), "* SEARCH") {
			continue
		}
		found = true
		for _, field := range strings.Fields(line[len("* SEARCH"):]) {
			if n, err := strconv.ParseUint(field, 10, 32); err == nil && n > 0 {
				numbers = append(numbers, uint32(n))
			}
		}
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	return numbers, found
}

// responseCode finds "[NAME value]" in a response text and returns value.
func responseCode(text, name string) (string, bool) {
	at := strings.Index(strings.ToUpper(text), "["+name+" ")
	if at < 0 {
		return "", false
	}
	rest := text[at+len(name)+2:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// listEntry is one "* LIST (attrs) delimiter name" response.
type listEntry struct {
	Attributes string // lower case, space separated
	Delimiter  string
	Name       string // as the server names it (modified UTF-7)
}

func parseListEntry(raw []byte) (listEntry, bool) {
	line := bytes.TrimPrefix(raw, []byte("* "))
	if len(line) < 5 || !strings.EqualFold(string(line[:4]), "LIST") {
		return listEntry{}, false
	}
	tokens, err := parseTokens(line[4:])
	if err != nil || len(tokens) < 3 {
		return listEntry{}, false
	}
	var e listEntry
	if attrs, ok := tokens[0].([]any); ok {
		for _, a := range attrs {
			if s, ok := a.(string); ok {
				e.Attributes += strings.ToLower(s) + " "
			}
		}
	}
	e.Delimiter, _ = tokens[1].(string)
	e.Name, _ = tokens[2].(string)
	return e, e.Name != ""
}

// quoteString makes text an IMAP quoted string.
func quoteString(text string) string {
	text = strings.NewReplacer("\r", " ", "\n", " ").Replace(text)
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
}

// searchString makes text safe inside a quoted search argument (quotes and backslashes would end
// the string; they carry no meaning for a search anyway).
func searchString(text string) string {
	return strings.NewReplacer(`"`, "", `\`, "", "\r", " ", "\n", " ").Replace(text)
}

// SubjectCriteria is the IMAP search for notes with this subject (substring match).
func SubjectCriteria(subject string) string { return `SUBJECT "` + searchString(subject) + `"` }

// uidList writes UIDs as an IMAP set ("5,7,9").
func uidList(uids []uint32) string {
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(parts, ",")
}

// uidSet expands an IMAP UID set ("5,7:9") into its UIDs, at most 1000.
func uidSet(set string) []uint32 {
	var uids []uint32
	for _, part := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), ":")
		first, err := strconv.ParseUint(lo, 10, 32)
		if err != nil {
			break
		}
		last := first
		if isRange {
			if last, err = strconv.ParseUint(hi, 10, 32); err != nil {
				break
			}
		}
		for u := first; u <= last && len(uids) < 1000; u++ {
			uids = append(uids, uint32(u))
		}
	}
	return uids
}

// imapDate is the date of a SINCE search, "26-Sep-2026" (no leading zero on the day).
func imapDate(t time.Time) string { return t.Local().Format("2-Jan-2006") }

// --- Folder names: modified UTF-7 (RFC 3501 5.1.3) ----------------------------------------------

var mutf7 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// FolderName converts a folder name to what the server needs (modified UTF-7).
func FolderName(name string) string {
	var out strings.Builder
	var units []uint16
	flush := func() {
		if len(units) == 0 {
			return
		}
		buf := make([]byte, 0, len(units)*2)
		for _, u := range units {
			buf = append(buf, byte(u>>8), byte(u))
		}
		out.WriteString("&" + mutf7.EncodeToString(buf) + "-")
		units = units[:0]
	}
	for _, r := range name {
		if r >= 0x20 && r < 0x7F {
			flush()
			out.WriteRune(r)
			if r == '&' {
				out.WriteByte('-')
			}
			continue
		}
		units = append(units, utf16.Encode([]rune{r})...)
	}
	flush()
	return out.String()
}

// decodeFolder converts a folder name from the server (modified UTF-7) to UTF-8.
func decodeFolder(name string) string {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		end := -1
		if name[i] == '&' {
			if k := strings.IndexByte(name[i+1:], '-'); k >= 0 {
				end = i + 1 + k
			}
		}
		if end < 0 {
			out.WriteByte(name[i])
			continue
		}
		if end == i+1 { // "&-"
			out.WriteByte('&')
			i = end
			continue
		}
		raw, err := mutf7.DecodeString(name[i+1 : end])
		if err != nil || len(raw)%2 != 0 {
			out.WriteString(name[i : end+1])
			i = end
			continue
		}
		units := make([]uint16, len(raw)/2)
		for k := range units {
			units[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		out.WriteString(string(utf16.Decode(units)))
		i = end
	}
	return out.String()
}

// isASCII: whether s has only 7-bit characters (a search with others needs CHARSET UTF-8).
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
