package mail

import (
	"bufio"
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLiteralLength(t *testing.T) {
	for line, want := range map[string]int{"* 1 FETCH (BODY[] {342}\r\n": 342, "a {0}\r\n": 0, "x {12+}\r\n": 12} {
		if n, ok := literalLength([]byte(line)); !ok || n != want {
			t.Errorf("%q: %d %v", line, n, ok)
		}
	}
	for _, line := range []string{"* OK hello\r\n", "* 1 FETCH (UID 4)\r\n", "text {abc}\r\n", "a {5} b\r\n"} {
		if _, ok := literalLength([]byte(line)); ok {
			t.Errorf("%q is no literal", line)
		}
	}
}

func TestReadResponseLiterals(t *testing.T) {
	// A literal that itself contains a CRLF and something that looks like a tagged line.
	wire := "* 1 FETCH (UID 7 BODY[HEADER.FIELDS (FROM)] {18}\r\nFrom: a@b\r\nJ9 OK\r\n)\r\nJ3 OK done\r\n"
	br := bufio.NewReader(strings.NewReader(wire))
	raw, err := readResponse(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), ")\r\n") || !strings.Contains(string(raw), "J9 OK") {
		t.Fatalf("response: %q", raw)
	}
	f, ok := parseFetch(raw)
	if !ok || f.UID() != 7 {
		t.Fatalf("fetch: %+v %v", f, ok)
	}
	body, _ := f.Body()
	if body != "From: a@b\r\nJ9 OK\r\n" {
		t.Errorf("body %q", body)
	}
	next, err := readResponse(br)
	if err != nil || string(next) != "J3 OK done\r\n" {
		t.Errorf("next: %q %v", next, err)
	}
	if _, err := readResponse(br); err == nil {
		t.Error("EOF expected")
	}
}

func TestReadResponseTruncatedLiteral(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("* 1 FETCH (BODY[] {50}\r\nshort"))
	if _, err := readResponse(br); err == nil {
		t.Error("a cut literal is an error")
	}
	br = bufio.NewReader(strings.NewReader("* 1 FETCH (BODY[] {999999999999}\r\n"))
	if _, err := readResponse(br); err == nil {
		t.Error("an absurd literal is an error")
	}
}

func TestParseTokens(t *testing.T) {
	got, err := parseTokens([]byte(`(\HasNoChildren \Trash) "/" "INBOX/Tr\"ash" NIL {3}` + "\r\nabc"))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{[]any{`\HasNoChildren`, `\Trash`}, "/", `INBOX/Tr"ash`, nil, "abc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%#v", got)
	}
	if _, err := parseTokens([]byte("(a (b)")); err == nil {
		t.Error("unbalanced list")
	}
	if _, err := parseTokens([]byte("a )")); err == nil {
		t.Error("stray parenthesis")
	}
}

func TestParseFetchItems(t *testing.T) {
	raw := []byte("* 12 FETCH (UID 345 RFC822.SIZE 2048 FLAGS (\\Seen) BODY[]<0> {5}\r\nhello)\r\n")
	f, ok := parseFetch(raw)
	if !ok || f.Seq != 12 || f.UID() != 345 || f.Size() != 2048 {
		t.Fatalf("%+v", f)
	}
	if body, ok := f.Body(); !ok || body != "hello" {
		t.Errorf("body %q", body)
	}
	if _, ok := parseFetch([]byte("* 3 EXISTS\r\n")); ok {
		t.Error("EXISTS is no fetch")
	}
	// NIL body (a mail without the section)
	f, ok = parseFetch([]byte("* 1 FETCH (UID 1 BODY[HEADER.FIELDS (FROM)] NIL)\r\n"))
	if !ok {
		t.Fatal("nil body")
	}
	if _, ok := f.Body(); ok {
		t.Error("NIL is no body")
	}
}

func TestParseSearch(t *testing.T) {
	untagged := [][]byte{[]byte("* OK something\r\n"), []byte("* SEARCH 9 3 12\r\n"), []byte("* SEARCH 5\r\n")}
	got, found := parseSearch(untagged)
	if !found || !reflect.DeepEqual(got, []uint32{3, 5, 9, 12}) {
		t.Errorf("%v %v", got, found)
	}
	got, found = parseSearch([][]byte{[]byte("* SEARCH\r\n")})
	if !found || len(got) != 0 {
		t.Errorf("empty search: %v %v", got, found)
	}
	if _, found = parseSearch([][]byte{[]byte("* OK x\r\n")}); found {
		t.Error("no SEARCH line means the search failed")
	}
}

func TestResponseCode(t *testing.T) {
	if v, ok := responseCode("* OK [UIDVALIDITY 3857529045] UIDs valid", "UIDVALIDITY"); !ok || v != "3857529045" {
		t.Errorf("%q %v", v, ok)
	}
	if v, ok := responseCode("OK [COPYUID 38505 304,319:320 3956,3957:3958] Done", "COPYUID"); !ok || v != "38505 304,319:320 3956,3957:3958" {
		t.Errorf("%q", v)
	}
	if _, ok := responseCode("OK done", "COPYUID"); ok {
		t.Error("no code")
	}
	got := parseCopyUID("* OK [COPYUID 38505 304,319:320 3956,3957:3958] Done")
	if got[304] != 3956 || got[319] != 3957 || got[320] != 3958 {
		t.Errorf("%v", got)
	}
}

func TestParseListEntry(t *testing.T) {
	e, ok := parseListEntry([]byte("* LIST (\\HasNoChildren \\Trash) \".\" \"INBOX.Gel&APY-scht\"\r\n"))
	if !ok || e.Delimiter != "." || e.Name != "INBOX.Gel&APY-scht" || !strings.Contains(e.Attributes, `\trash`) {
		t.Errorf("%+v", e)
	}
	if decodeFolder(e.Name) != "INBOX.Gelöscht" {
		t.Errorf("decoded %q", decodeFolder(e.Name))
	}
	e, ok = parseListEntry([]byte("* LIST () NIL Drafts\r\n"))
	if !ok || e.Delimiter != "" || e.Name != "Drafts" {
		t.Errorf("%+v", e)
	}
}

func TestFolderNames(t *testing.T) {
	cases := map[string]string{
		"Entwürfe":           "Entw&APw-rfe",
		"Jarvis-Gedaechtnis": "Jarvis-Gedaechtnis",
		"A&B":                "A&-B",
		"日本語":                "&ZeVnLIqe-",
		"Spam 😀":             "Spam &2D3eAA-",
	}
	for plain, encoded := range cases {
		if got := FolderName(plain); got != encoded {
			t.Errorf("FolderName(%q) = %q, want %q", plain, got, encoded)
		}
		if got := decodeFolder(encoded); got != plain {
			t.Errorf("decodeFolder(%q) = %q, want %q", encoded, got, plain)
		}
	}
}

func TestUIDSets(t *testing.T) {
	if got := uidSet("5,7:9, 12"); !reflect.DeepEqual(got, []uint32{5, 7, 8, 9, 12}) {
		t.Errorf("%v", got)
	}
	if got := uidSet("1:5000"); len(got) != 1000 {
		t.Errorf("capped at 1000, got %d", len(got))
	}
	if uidList([]uint32{4, 8}) != "4,8" {
		t.Error("uidList")
	}
}

func TestQuoting(t *testing.T) {
	if got := quoteString(`a"b\c` + "\r\n"); got != `"a\"b\\c  "` {
		t.Errorf("%s", got)
	}
	if got := SubjectCriteria(`Jarvis "Kern"\`); got != `SUBJECT "Jarvis Kern"` {
		t.Errorf("%s", got)
	}
}

func TestIMAPDate(t *testing.T) {
	d := time.Date(2026, 9, 6, 12, 0, 0, 0, time.Local)
	if got := imapDate(d); got != "6-Sep-2026" {
		t.Errorf("%s", got)
	}
}

func TestSearchWords(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	got := searchWords(`rechnung, "strom";  zähler\`, 7, now)
	want := `SINCE 26-Sep-2026 TEXT "rechnung" TEXT "strom" TEXT "zähler"`
	if got != want {
		t.Errorf("%s", got)
	}
	if got := searchWords("", 1, now); got != "SINCE 2-Oct-2026" {
		t.Errorf("%s", got)
	}
}

func TestListCriteria(t *testing.T) {
	if got := listCriteria(true, false, "print@epson.example"); got != "UNSEEN NOT FROM epson.example" {
		t.Errorf("%s", got)
	}
	if got := listCriteria(false, false, ""); got != "ALL" {
		t.Errorf("%s", got)
	}
	if got := listCriteria(true, true, "print@epson.example"); got != "ALL" {
		t.Errorf("sent folder: %s", got)
	}
}

func TestMutf7Roundtrip(t *testing.T) {
	var buf bytes.Buffer
	for _, name := range []string{"Gesendet", "Archiv/2026", "Müll & Co", "Ärger"} {
		buf.WriteString(decodeFolder(FolderName(name)))
		if decodeFolder(FolderName(name)) != name {
			t.Errorf("%q", name)
		}
	}
}
