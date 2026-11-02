package mail

import (
	"net/mail"
	"reflect"
	"strings"
	"testing"

	"echo-assistant/internal/backup"
	"echo-assistant/internal/config"
	"echo-assistant/internal/memory"
	"echo-assistant/internal/tools"
)

var (
	_ backup.Target  = (*BackupTarget)(nil)
	_ memory.Archive = (*ArchiveTier)(nil)
)

func TestCleanText(t *testing.T) {
	tab, nl := string(rune(9)), string(rune(10))
	in := "Hallo" + string(rune(0xAD)) + " " + string(rune(0x200B)) + "welt" + string(rune(0xA0)) + string(rune(0xA0)) + "  und" + tab + tab + "mehr" + string(rune(0xFEFF)) + " " + nl + "  zweite Zeile " + string(rune(0x2060)) + " "
	if got, want := CleanText(in), "Hallo welt und\t\tmehr \nzweite Zeile"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestHTMLToText(t *testing.T) {
	html := `<html><head><title>Titel</title><style>p { color: red }</style></head><body>
<h1>Überschrift</h1><p>Erster&nbsp;Absatz &amp; &lt;mehr&gt; &#228;&#xF6;&euro; &unknown; &#0;</p>
<script>var x = "<p>nein</p>";</script>
<ul><li>eins</li><li>zwei</li></ul>
<table><tr><td>a</td><td>b</td></tr></table>
<p>Ende<br>Zeile</p></body></html>`
	want := "Überschrift\nErster Absatz & <mehr> äö€ &unknown;\n- eins\n- zwei\na b\nEnde\nZeile"
	got := HTMLToText(html)
	// Empty lines between blocks may appear; compare without them.
	if strings.ReplaceAll(got, "\n\n", "\n") != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := HTMLToText("<p>unterbrochen <b"); got != "unterbrochen" {
		t.Errorf("unterminated tag: %q", got)
	}
	if got := HTMLToText("<style>a{}"); got != "" {
		t.Errorf("unterminated style: %q", got)
	}
	// Uppercase tags, a tag that merely starts like style.
	if got := HTMLToText("<STYLE>x</STYLE><Stylish>kept</Stylish>"); got != "kept" {
		t.Errorf("%q", got)
	}
}

func TestHTMLToTextEmptyLines(t *testing.T) {
	// Two empty lines in a row collapse into one.
	if got := HTMLToText("a<br><br><br><br>b"); got != "a\n\nb" {
		t.Errorf("%q", got)
	}
}

func TestVerifiedSender(t *testing.T) {
	good := mail.Header{
		"From":                   {"Max <max@example.org>"},
		"Authentication-Results": {`mx.provider.example; dmarc=pass header.from="example.org"`},
	}
	addresses := []string{"anna@example.net", "max@example.org"}
	if got := verifiedSender(good, addresses, "mx.provider.example"); got != "max@example.org" {
		t.Errorf("verified: %q", got)
	}
	if verifiedSender(good, addresses, "") != "" {
		t.Error("without an authserv id nothing is trusted")
	}
	if verifiedSender(good, addresses, "other.example") != "" {
		t.Error("another authserv")
	}
	forged := mail.Header{
		"From":                   {"Max <max@example.org>"},
		"Authentication-Results": {`evil.example; dmarc=pass header.from="example.org"`},
	}
	if verifiedSender(forged, addresses, "mx.provider.example") != "" {
		t.Error("a forged header below the provider's must not count")
	}
	failed := mail.Header{
		"From":                   {"Max <max@example.org>"},
		"Authentication-Results": {`mx.provider.example; dmarc=fail header.from="example.org"`},
	}
	if verifiedSender(failed, addresses, "mx.provider.example") != "" {
		t.Error("dmarc fail")
	}
	stranger := mail.Header{
		"From":                   {"Eve <eve@example.com>"},
		"Authentication-Results": {`mx.provider.example; dmarc=pass header.from="example.com"`},
	}
	if verifiedSender(stranger, addresses, "mx.provider.example") != "" {
		t.Error("not a person of the household")
	}
}

func TestFromCriteria(t *testing.T) {
	if got := fromCriteria([]string{"a@x.example"}); got != `FROM "a@x.example"` {
		t.Errorf("%s", got)
	}
	if got := fromCriteria([]string{"a@x.example", "b@y.example", "c@z.example"}); got !=
		`OR FROM "a@x.example" OR FROM "b@y.example" FROM "c@z.example"` {
		t.Errorf("%s", got)
	}
}

func TestRecipients(t *testing.T) {
	got, ok := recipients("a@x.example, B <b@y.example>")
	if !ok || !reflect.DeepEqual(got, []string{"a@x.example", "b@y.example"}) {
		t.Errorf("%v %v", got, ok)
	}
	for _, bad := range []string{"", "nobody", "@x.example"} {
		if _, ok := recipients(bad); ok {
			t.Errorf("%q is no address", bad)
		}
	}
}

func TestTopicCriteria(t *testing.T) {
	if topicCriteria("urlaub", "Urlaub") != `SUBJECT "[urlaub]"` || topicCriteria("", "Urlaub") != `SUBJECT "Urlaub"` {
		t.Error("topic criteria")
	}
}

func TestCommandTask(t *testing.T) {
	c := Command{Subject: "Licht", Text: "Mach das Licht an", Sender: "max@example.org"}
	if got := c.Task("Max"); got != "Task by e-mail from Max (max@example.org, sender verified).\nSubject: Licht\n\nMach das Licht an" {
		t.Errorf("%q", got)
	}
}

func TestToolsRegistered(t *testing.T) {
	s := New(config.Secrets{"MAIL_ADDRESS": "jarvis@example.org", "MAIL_PASSWORD": "x"})
	var r tools.Registry
	s.AddTools(&r)
	if got := strings.Join(r.Names(), ","); got != "send_email,print_document,list_emails,read_email" {
		t.Errorf("tools: %s", got)
	}
	// Invalid recipient: refused before any network use.
	if got := r.Call("send_email", `{"to":"nobody","subject":"s","html":"h"}`, tools.Context{Channel: tools.Voice}); got != "Error: invalid recipient address" {
		t.Errorf("%q", got)
	}
	var empty tools.Registry
	New(config.Secrets{}).AddTools(&empty)
	if len(empty.Names()) != 0 {
		t.Error("no tools without a mailbox")
	}
	if s.TakesCommands() || New(config.Secrets{"MAIL_ADDRESS": "a@b", "MAIL_PASSWORD": "x", "MAIL_AUTHSERV_ID": "mx"}).TakesCommands() == false {
		t.Error("TakesCommands needs MAIL_AUTHSERV_ID")
	}
}
