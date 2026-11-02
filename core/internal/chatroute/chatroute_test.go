package chatroute

import (
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
	"echo-assistant/internal/store"
)

type fake struct {
	name   string
	result string
	got    []string
}

func (f *fake) Configured() bool { return true }
func (f *fake) Send(text, person string) string {
	f.got = append(f.got, "text:"+text)
	return f.result
}
func (f *fake) SendPhoto(_ []byte, caption, _ string) string {
	f.got = append(f.got, "photo:"+caption)
	return f.result
}
func (f *fake) SendDocument(_ []byte, filename, _, _, _ string) string {
	f.got = append(f.got, "doc:"+filename)
	return f.result
}

func TestWhatsAppText(t *testing.T) {
	in := `<b>Termin</b> &amp; <i>Ort</i>: <a href="https://example.org/a?b=1&amp;c=2">Karte</a>, <a href="https://x.org">https://x.org</a> <code>42</code> <u>u</u>`
	want := "*Termin* & _Ort_: Karte (https://example.org/a?b=1&c=2), https://x.org `42` u"
	if got := WhatsAppText(in); got != want {
		t.Errorf("WhatsAppText = %q", got)
	}
}

func TestRouterOrder(t *testing.T) {
	store.Dir = t.TempDir()
	reg := people.New(config.Secrets{"OWNER_NAME": "Admin", "PHONE_OWNER": "+491701234567"})
	tg, wa := &fake{name: "tg", result: "sent"}, &fake{name: "wa", result: "sent"}
	reg.Channels.Telegram = tg.Send
	reg.Channels.WhatsApp = wa.Send
	r := &Router{People: reg, Telegram: tg, WhatsApp: wa}

	// The admin has a phone but no Telegram account: WhatsApp, with the HTML turned into its format.
	if got := r.Send("<b>Hallo</b>", ""); got != "sent" || len(wa.got) != 1 || wa.got[0] != "text:*Hallo*" || len(tg.got) != 0 {
		t.Fatalf("send = %q, wa %v, tg %v", got, wa.got, tg.got)
	}
	// WhatsApp fails: no other channel reaches the admin.
	wa.result = "Error: down"
	if got := r.SendPhoto(nil, "Bild", ""); got != "Error: whatsapp: Error: down" {
		t.Errorf("photo = %q", got)
	}
}
