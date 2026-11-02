package mail

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestDecodeHeader(t *testing.T) {
	cases := map[string]string{
		"=?utf-8?B?VGVzdA==?=":                                "Test",
		"=?UTF-8?Q?M=C3=BCller_und_Co?=":                      "Müller und Co",
		"=?iso-8859-1?Q?Gr=FC=DFe_aus_K=F6ln?= <a@b.example>": "Grüße aus Köln <a@b.example>",
		"=?windows-1252?Q?=80_5,=2D?= f":                      "€ 5,- f",
		"=?utf-8?Q?a?= =?utf-8?Q?b?=":                         "ab", // whitespace between encoded words is dropped
		"Plain subject":                                       "Plain subject",
		"=?utf-8?B?!!!?= broken":                              "=?utf-8?B?!!!?= broken",
		"=?utf-8?Q?lang?=\r\n =?utf-8?Q?er_Betreff?=":         "langer Betreff",
		"Re: =?UTF-8?B?w5xiZXIgZGllIFJlaXNl?=":                "Re: Über die Reise",
	}
	for in, want := range cases {
		if got := decodeHeader(in); got != want {
			t.Errorf("decodeHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeHeaderRoundtrip(t *testing.T) {
	for _, s := range []string{"Hello", "Gespräch 2026-10-03 18:38 – Über Müll", "Thema [urlaub]: Südtirol",
		strings.Repeat("Straße ", 30)} {
		enc := encodeHeader(s)
		for i := 0; i < len(enc); i++ {
			if enc[i] >= 0x80 {
				t.Fatalf("encoded header has 8-bit text: %q", enc)
			}
		}
		if got := decodeHeader(enc); got != strings.TrimSpace(s) {
			t.Errorf("roundtrip %q -> %q -> %q", s, enc, got)
		}
	}
	if encodeHeader("a\r\nBcc: x@y") != "a  Bcc: x@y" {
		t.Errorf("header injection: %q", encodeHeader("a\r\nBcc: x@y"))
	}
}

func TestToUTF8(t *testing.T) {
	if got := toUTF8("iso-8859-1", []byte{'M', 0xFC, 'l', 'l', 'e', 'r'}); got != "Müller" {
		t.Errorf("%q", got)
	}
	if got := toUTF8("Windows-1252", []byte{0x80, ' ', 0x93, 'x', 0x94}); got != "€ “x”" {
		t.Errorf("%q", got)
	}
	if got := toUTF8("iso-8859-15", []byte{0xA4}); got != "€" {
		t.Errorf("%q", got)
	}
	if got := toUTF8("us-ascii", []byte{'a', 0xE4}); got != "aä" {
		t.Errorf("8-bit ascii: %q", got)
	}
	if got := toUTF8("utf-8", []byte{'a', 0xFF, 'b'}); got != "a?b" {
		t.Errorf("%q", got)
	}
	if got := toUTF8("", []byte("Grüße")); got != "Grüße" {
		t.Errorf("%q", got)
	}
}

func TestDecodeBase64Tolerant(t *testing.T) {
	want := []byte("Hello, wörld! This is a test of the base64 decoder.")
	enc := base64.StdEncoding.EncodeToString(want)
	spaced := enc[:20] + "\r\n" + enc[20:40] + "\n " + enc[40:]
	if got := decodeBase64([]byte(spaced)); string(got) != string(want) {
		t.Errorf("%q", got)
	}
	// junk after the data (the tail of a FETCH response once got decoded into the body)
	if got := decodeBase64([]byte(enc + "\r\n)")); string(got) != string(want) {
		t.Errorf("%q", got)
	}
	if got := decodeBase64([]byte("QQ")); string(got) != "A" {
		t.Errorf("unpadded: %q", got)
	}
}

const multipartMail = "From: =?utf-8?Q?J=C3=BCrgen?= <j@example.org>\r\n" +
	"To: Jarvis <jarvis@example.org>\r\n" +
	"Subject: =?iso-8859-1?Q?Gr=FC=DFe?=\r\n" +
	"Date: Sat, 3 Oct 2026 21:03:26 +0200\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=inner\r\n\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Hallo, gr=FC=DFe aus K=F6ln!=\r\n Bis bald.\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n\r\n" +
	"<p>Hallo <b>Welt</b></p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"=?utf-8?Q?Rechnung_M=C3=A4rz.pdf?=\"\r\n" +
	"Content-Disposition: attachment; filename*=UTF-8''Rechnung%20M%C3%A4rz.pdf\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"JVBERi0xLjQKJSVFT0YK\r\n" +
	"--outer--\r\n"

func TestParseMultipart(t *testing.T) {
	p, err := parseMessage([]byte(multipartMail), parseOptions{keep: func(a Attachment) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.headerText("From"); got != "Jürgen <j@example.org>" {
		t.Errorf("from %q", got)
	}
	if got := p.headerText("Subject"); got != "Grüße" {
		t.Errorf("subject %q", got)
	}
	if p.Plain != "Hallo, grüße aus Köln! Bis bald." {
		t.Errorf("plain %q", p.Plain)
	}
	if p.HTML != "<p>Hallo <b>Welt</b></p>" {
		t.Errorf("html %q", p.HTML)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments %+v", p.Attachments)
	}
	a := p.Attachments[0]
	if a.Filename != "Rechnung März.pdf" || a.MIME != "application/pdf" || string(a.Data) != "%PDF-1.4\n%%EOF\n" {
		t.Errorf("attachment %+v %q", a, a.Data)
	}
	if got := attachmentList(p); got != "Rechnung März.pdf (application/pdf)" {
		t.Errorf("list %q", got)
	}
	if p.Text() != "Hallo, grüße aus Köln! Bis bald." {
		t.Errorf("text %q", p.Text())
	}
	// Without keep the data stays undecoded.
	p, _ = parseMessage([]byte(multipartMail), parseOptions{})
	if len(p.Attachments) != 1 || p.Attachments[0].Data != nil {
		t.Errorf("data kept: %+v", p.Attachments)
	}
}

func TestParseHTMLOnlyAndCut(t *testing.T) {
	raw := "Subject: x\r\nContent-Type: text/html; charset=windows-1252\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte("<html><head><title>t</title></head><body><p>Preis: 5\x80</p><ul><li>eins</li><li>zwei</li></ul></body></html>"))
	p, err := parseMessage([]byte(raw), parseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Text(); got != "Preis: 5€\n\n- eins\n- zwei" && got != "Preis: 5€\n- eins\n- zwei" {
		t.Errorf("%q", got)
	}
	// A partial fetch cuts the mail in the middle of a part: what came before still reads.
	cut := multipartMail[:strings.Index(multipartMail, "--inner--")]
	p, err = parseMessage([]byte(cut), parseOptions{})
	if err != nil || !strings.HasPrefix(p.Plain, "Hallo, grüße") {
		t.Errorf("cut mail: %v %q", err, p.Plain)
	}
}

func TestParseWithoutContentType(t *testing.T) {
	p, err := parseMessage([]byte("Subject: hi\n\nLine one\nLine two\n"), parseOptions{})
	if err != nil || p.Plain != "Line one\nLine two\n" {
		t.Errorf("%v %q", err, p.Plain)
	}
}

func TestBuildAndParseOwnMail(t *testing.T) {
	m := outMessage{From: "Jarvis <jarvis@example.org>", To: "a@example.org", Subject: "Süße Grüße – Test",
		Text: "Zeile eins\nZeile zwei mit Umlaut ä\n.\nPunkt", HTML: "<p>Zeile eins</p>",
		Attachment: &Attachment{Filename: "bild ä.jpg", MIME: "image/jpeg", Data: []byte{0xFF, 0xD8, 1, 2, 3, 0xFF, 0xD9}},
		Date:       time.Date(2026, 10, 3, 21, 3, 26, 0, time.UTC)}
	raw := m.bytes()
	if strings.Contains(string(raw), "\n") && strings.Contains(strings.ReplaceAll(string(raw), "\r\n", ""), "\n") {
		t.Error("bare LF in the message")
	}
	p, err := parseMessage(raw, parseOptions{keep: func(Attachment) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if p.headerText("Subject") != "Süße Grüße – Test" || p.Header.Get("Date") != "Sat, 03 Oct 2026 21:03:26 +0000" {
		t.Errorf("headers %q %q", p.headerText("Subject"), p.Header.Get("Date"))
	}
	if !strings.Contains(p.Header.Get("Message-Id"), "@example.org>") {
		t.Errorf("message id %q", p.Header.Get("Message-Id"))
	}
	if p.Plain != "Zeile eins\nZeile zwei mit Umlaut ä\n.\nPunkt" {
		t.Errorf("plain %q", p.Plain)
	}
	if p.HTML != "<p>Zeile eins</p>" {
		t.Errorf("html %q", p.HTML)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Filename != "bild ä.jpg" || len(p.Attachments[0].Data) != 7 {
		t.Errorf("attachment %+v", p.Attachments)
	}
}

// A mail on the user's behalf: Cc and Reply-To go to the user; without them neither header.
func TestCcAndReplyTo(t *testing.T) {
	m := outMessage{From: "Jarvis <j@example.org>", To: "shop@example.com", Cc: "user@example.org",
		ReplyTo: "user@example.org", Subject: "Nachfrage", Text: "Hallo"}
	p, err := parseMessage(m.bytes(), parseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Header.Get("Cc") != "user@example.org" || p.Header.Get("Reply-To") != "user@example.org" {
		t.Errorf("cc %q, reply-to %q", p.Header.Get("Cc"), p.Header.Get("Reply-To"))
	}
	m.Cc, m.ReplyTo = "", ""
	if raw := string(m.bytes()); strings.Contains(raw, "Cc:") || strings.Contains(raw, "Reply-To:") {
		t.Error("empty cc or reply-to written")
	}
}

func TestNoteRoundtrip(t *testing.T) {
	text := strings.Repeat("Ein langer Transkript-Satz mit Umlauten äöü. ", 40) + "\nZeile 2"
	n := outMessage{From: "Jarvis <j@example.org>", To: "Jarvis <j@example.org>", Subject: "Gespräch 2026-10-03 18:38 – Müll",
		Text: text + noteEnd + "\n", Base64Text: true}
	p, err := parseMessage(n.bytes(), parseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := p.Plain
	if end := strings.Index(got, noteEnd); end >= 0 {
		got = got[:end]
	}
	if got != text {
		t.Errorf("note: %q", got)
	}
	if p.headerText("Subject") != "Gespräch 2026-10-03 18:38 – Müll" {
		t.Errorf("subject %q", p.headerText("Subject"))
	}
}

func TestBackupJSON(t *testing.T) {
	if got := string(backupJSON("Summary\n" + jsonMarker + `{"a":1}`)); got != `{"a":1}` {
		t.Errorf("%q", got)
	}
	if got := string(backupJSON("header text\n{\"b\":2}")); got != `{"b":2}` {
		t.Errorf("%q", got)
	}
	if backupJSON("nothing here") != nil {
		t.Error("no JSON")
	}
}

func TestParseDate(t *testing.T) {
	want := time.Date(2026, 10, 3, 19, 3, 26, 0, time.UTC)
	for _, in := range []string{"Sat, 3 Oct 2026 21:03:26 +0200", "3 Oct 2026 21:03:26 +0200",
		"Sat, 03 Oct 2026 21:03:26 +0200 (CEST)"} {
		if got := parseDate(in); !got.Equal(want) {
			t.Errorf("%q -> %v", in, got)
		}
	}
	if !parseDate("yesterday").IsZero() {
		t.Error("garbage date")
	}
}

func TestParseHeaderBlock(t *testing.T) {
	h := parseHeaderBlock("From: A <a@b.example>\r\nSubject: =?utf-8?Q?lang?=\r\n =?utf-8?Q?er?=\r\nX-Two: 1\r\nX-Two: 2\r\n\r\n")
	if decodeHeader(h.Get("Subject")) != "langer" || h.Get("X-Two") != "1" || h.Get("From") != "A <a@b.example>" {
		t.Errorf("%v", h)
	}
}
