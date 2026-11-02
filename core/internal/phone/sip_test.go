package phone

import (
	"strings"
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/people"
)

const inviteText = "INVITE sip:jarvis@192.0.2.10:5060 SIP/2.0\r\n" +
	"Via: SIP/2.0/UDP 192.0.2.1:5060;branch=z9hG4bKabc\r\n" +
	"Via: SIP/2.0/UDP 192.0.2.2:5060;branch=z9hG4bKdef\r\n" +
	"f: \"Someone\" <sip:**610@192.0.2.1>;tag=xyz\r\n" +
	"To: <sip:jarvis@192.0.2.1>\r\n" +
	"i: call-1@192.0.2.1\r\n" +
	"CSeq: 7 INVITE\r\n" +
	"Contact: <sip:**610@192.0.2.1:5060;transport=udp>\r\n" +
	"Record-Route: <sip:192.0.2.1;lr>\r\n" +
	"Subject: a folded\r\n  subject\r\n" +
	"c: application/sdp\r\n" +
	"l: 5\r\n\r\n" +
	"v=0\r\nrest"

func TestParseRequest(t *testing.T) {
	m, ok := Parse(inviteText)
	if !ok || !m.Request || m.Method != "INVITE" || m.URI != "sip:jarvis@192.0.2.10:5060" {
		t.Fatalf("parsed: %+v %v", m, ok)
	}
	if got := m.All("via"); len(got) != 2 || !strings.Contains(got[1], "192.0.2.2") {
		t.Errorf("via: %v", got)
	}
	if m.Header("call-id") != "call-1@192.0.2.1" || m.Header("content-type") != "application/sdp" {
		t.Errorf("compact forms: %q %q", m.Header("call-id"), m.Header("content-type"))
	}
	if m.Header("subject") != "a folded subject" {
		t.Errorf("folding: %q", m.Header("subject"))
	}
	if m.Body != "v=0\r\n" {
		t.Errorf("body cut to Content-Length: %q", m.Body)
	}
	if m.CSeqMethod() != "INVITE" || m.CSeqNumber() != 7 {
		t.Errorf("cseq: %s %d", m.CSeqMethod(), m.CSeqNumber())
	}
	if uriUser(m.Header("from")) != "**610" {
		t.Errorf("user: %q", uriUser(m.Header("from")))
	}
	if uriOf(m.Header("contact")) != "sip:**610@192.0.2.1:5060;transport=udp" {
		t.Errorf("contact: %q", uriOf(m.Header("contact")))
	}
}

func TestParseResponse(t *testing.T) {
	m, ok := Parse("SIP/2.0 401 Unauthorized\r\nCSeq: 2 REGISTER\r\nWWW-Authenticate: Digest realm=\"r\"\r\n\r\n")
	if !ok || m.Request || m.Status != 401 || m.Reason != "Unauthorized" || m.CSeqMethod() != "REGISTER" {
		t.Fatalf("parsed: %+v %v", m, ok)
	}
	for _, bad := range []string{"", "garbage", "SIP/2.0 \r\n\r\n", "INVITE\r\n\r\n"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestUriHelpers(t *testing.T) {
	if got := uriUser(`"Name" <tel:+4930123>;tag=x`); got != "+4930123" {
		t.Errorf("tel: %q", got)
	}
	if got := uriOf("sip:a@b;tag=1"); got != "sip:a@b" {
		t.Errorf("bare: %q", got)
	}
	if got := withTag("<sip:a@b>"); !strings.Contains(got, ";tag=") || withTag(got) != got {
		t.Errorf("tag: %q", got)
	}
}

func TestBuildResponse(t *testing.T) {
	req, _ := Parse(inviteText)
	out := buildResponse(req, 200, "OK", "<sip:jarvis@192.0.2.1>;tag=t", "<sip:j@192.0.2.10:5060>", "v=0\r\n")
	for _, want := range []string{
		"SIP/2.0 200 OK\r\n",
		"Via: SIP/2.0/UDP 192.0.2.1:5060;branch=z9hG4bKabc\r\nVia: SIP/2.0/UDP 192.0.2.2:5060;branch=z9hG4bKdef\r\n",
		"To: <sip:jarvis@192.0.2.1>;tag=t\r\n", "CSeq: 7 INVITE\r\n", "Record-Route: <sip:192.0.2.1;lr>\r\n",
		"Contact: <sip:j@192.0.2.10:5060>\r\n", "Content-Type: application/sdp\r\n", "Content-Length: 5\r\n\r\nv=0\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	// 100 Trying: no Contact, no body.
	trying := buildResponse(req, 100, "Trying", "t", "<c>", "")
	if strings.Contains(trying, "Contact") || strings.Contains(trying, "Content-Type") || !strings.HasSuffix(trying, "Content-Length: 0\r\n\r\n") {
		t.Errorf("trying: %s", trying)
	}
	// What we build, we can parse.
	if back, ok := Parse(out); !ok || back.Status != 200 || back.Body != "v=0\r\n" {
		t.Errorf("round trip: %+v", back)
	}
}

// RFC 2617 section 3.5.
func TestDigestRFC2617(t *testing.T) {
	got := digestResponse("Mufasa", "testrealm@host.com", "Circle Of Life", "GET", "/dir/index.html",
		"dcd98b7102dd2f0e8b11d0f600bfb0c093", "00000001", "0a4f113b", "auth")
	if got != "6629fae49393a05397450978507c4ef1" {
		t.Errorf("response %s", got)
	}
}

func TestChallengeAuthorization(t *testing.T) {
	resp, _ := Parse("SIP/2.0 401 Unauthorized\r\nCSeq: 1 REGISTER\r\n" +
		"WWW-Authenticate: Digest realm=\"fritz.box\", nonce=\"abc123\", opaque=\"op\", qop=\"auth\", algorithm=MD5\r\n\r\n")
	c := parseChallenge(resp)
	if c.Realm != "fritz.box" || c.Nonce != "abc123" || c.Opaque != "op" || c.QOP != "auth" || c.Proxy {
		t.Fatalf("challenge %+v", c)
	}
	line := c.authorize("user", "pw", "REGISTER", "sip:fritz.box", "cafe")
	want := digestResponse("user", "fritz.box", "pw", "REGISTER", "sip:fritz.box", "abc123", "00000001", "cafe", "auth")
	for _, s := range []string{"Authorization: Digest username=\"user\"", `realm="fritz.box"`, `uri="sip:fritz.box"`,
		"algorithm=MD5", "qop=auth, nc=00000001", `cnonce="cafe"`, `response="` + want + `"`, `opaque="op"`} {
		if !strings.Contains(line, s) {
			t.Errorf("missing %q in %s", s, line)
		}
	}
	if !strings.HasSuffix(line, "\r\n") {
		t.Error("no CRLF")
	}
	if again := c.authorize("user", "pw", "REGISTER", "sip:fritz.box", "cafe"); !strings.Contains(again, "nc=00000002") {
		t.Error("nonce count does not rise")
	}
	// No qop (RFC 2069) and a proxy challenge.
	proxy, _ := Parse("SIP/2.0 407 Proxy Authentication Required\r\nProxy-Authenticate: Digest realm=\"r\", nonce=\"n\"\r\n\r\n")
	pc := parseChallenge(proxy)
	line = pc.authorize("u", "p", "INVITE", "sip:x", "unused")
	if !strings.HasPrefix(line, "Proxy-Authorization: ") || strings.Contains(line, "qop") || strings.Contains(line, "cnonce") {
		t.Errorf("proxy: %s", line)
	}
	if want := md5hex(md5hex("u:r:p") + ":n:" + md5hex("INVITE:sip:x")); !strings.Contains(line, want) {
		t.Errorf("rfc2069 response missing in %s", line)
	}
}

func TestDigestParamIgnoresCnonce(t *testing.T) {
	h := `Digest cnonce="zzz", nonce="real", realm="x"`
	if got := digestParam(h, "nonce"); got != "real" {
		t.Errorf("nonce %q", got)
	}
	if got := digestParam(h, "missing"); got != "" {
		t.Errorf("missing %q", got)
	}
	if got := digestParam("Digest algorithm=MD5, realm=x", "algorithm"); got != "MD5" {
		t.Errorf("unquoted %q", got)
	}
	if got := digestParam("<sip:a@b>;expires=120;q=1", "expires"); got != "120" {
		t.Errorf("expires %q", got)
	}
}

func testPhone() *Phone {
	return New(config.Secrets{"SIP_USER": "u", "SIP_PASSWORD": "p", "PHONE_CALLERS": "+49 171 1234567, **",
		"PHONE_OWNER": "0171 1234567"}, &people.Register{}, nil, Deps{})
}

func TestNewReadsTheSecrets(t *testing.T) {
	p := testPhone()
	if !p.Configured() || p.registrar != defaultRegistrar || p.owner != "01711234567" {
		t.Errorf("%+v", p)
	}
	if len(p.callers) != 2 || p.callers[0] != "01711234567" || p.callers[1] != "**" {
		t.Errorf("callers %v", p.callers)
	}
	if New(config.Secrets{}, nil, nil, Deps{}).Configured() {
		t.Error("configured without secrets")
	}
	if p.ownerNumber() != "01711234567" {
		t.Errorf("owner %q", p.ownerNumber())
	}
}

func TestCheckTarget(t *testing.T) {
	for number, wantErr := range map[string]bool{
		"07161 123456": false, "+49 7161 123456": false, "0049 7161 123456": false,
		"+41 44 123456": true, "112": true, "0900 123456": true, "0180 5 123456": true, "0712": true, "": true,
	} {
		if got := checkTarget(number) != ""; got != wantErr {
			t.Errorf("%q: error %v, want %v (%s)", number, got, wantErr, checkTarget(number))
		}
	}
}

// The speaker's name from HA is the display name in From (#65); quotes cannot break the header.
func TestFromDisplayName(t *testing.T) {
	p := &Phone{user: "jarvis", registrar: "192.0.2.1"}
	if got := p.from(); got != "<sip:jarvis@192.0.2.1>" {
		t.Fatalf("without a name: %q", got)
	}
	p.deps.DisplayName = ` Jarvis "Küche"` + "\r\n"
	if got := p.from(); got != `"Jarvis Küche" <sip:jarvis@192.0.2.1>` {
		t.Fatalf("with a name: %q", got)
	}
}
