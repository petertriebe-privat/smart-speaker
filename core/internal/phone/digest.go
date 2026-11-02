package phone

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

// Challenge: a Digest challenge (RFC 2617, MD5, qop auth) of the registrar, as phone.cpp.
type Challenge struct {
	Realm, Nonce, Opaque, QOP string
	Proxy                     bool // 407: Proxy-Authorization
	count                     uint32
}

func md5hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// digestParam: a parameter of a Digest challenge: realm="fritz.box" or algorithm=MD5.
func digestParam(h, name string) string {
	key := name + "="
	at := strings.Index(h, key)
	for at > 0 && isAlnum(h[at-1]) { // "nonce" in "cnonce"
		next := strings.Index(h[at+1:], key)
		if next < 0 {
			return ""
		}
		at += 1 + next
	}
	if at < 0 {
		return ""
	}
	at += len(key)
	if at < len(h) && h[at] == '"' {
		end := strings.Index(h[at+1:], `"`)
		if end < 0 {
			return h[at+1:]
		}
		return h[at+1 : at+1+end]
	}
	end := at
	for end < len(h) && h[end] != ',' && h[end] != ' ' && h[end] != ';' {
		end++
	}
	return h[at:end]
}

func parseChallenge(response *Message) Challenge {
	c := Challenge{Proxy: response.Status == 407}
	name := "www-authenticate"
	if c.Proxy {
		name = "proxy-authenticate"
	}
	h := response.Header(name)
	c.Realm, c.Nonce = digestParam(h, "realm"), digestParam(h, "nonce")
	c.Opaque, c.QOP = digestParam(h, "opaque"), digestParam(h, "qop")
	return c
}

// digestResponse: the response value of RFC 2617 3.2.2.1; qop "" = the old RFC 2069 form.
func digestResponse(user, realm, password, method, uri, nonce, nc, cnonce, qop string) string {
	ha1 := md5hex(user + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)
	if strings.Contains(qop, "auth") {
		return md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	}
	return md5hex(ha1 + ":" + nonce + ":" + ha2)
}

// Authorization: the (Proxy-)Authorization header line (with CRLF) answering the challenge.
func (c *Challenge) Authorization(user, password, method, uri string) string {
	return c.authorize(user, password, method, uri, randomHex(8))
}

func (c *Challenge) authorize(user, password, method, uri, cnonce string) string {
	name := "Authorization"
	if c.Proxy {
		name = "Proxy-Authorization"
	}
	h := fmt.Sprintf(`%s: Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5`,
		name, user, c.Realm, c.Nonce, uri)
	nc := ""
	if strings.Contains(c.QOP, "auth") {
		c.count++
		nc = fmt.Sprintf("%08x", c.count)
		h += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	}
	h += fmt.Sprintf(`, response="%s"`, digestResponse(user, c.Realm, password, method, uri, c.Nonce, nc, cnonce, c.QOP))
	if c.Opaque != "" {
		h += fmt.Sprintf(`, opaque="%s"`, c.Opaque)
	}
	return h + "\r\n"
}
