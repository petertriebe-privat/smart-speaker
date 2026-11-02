package phone

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// Message: a SIP request or response (RFC 3261), as phone.cpp's Message.
type Message struct {
	Request bool
	Method  string
	URI     string
	Status  int
	Reason  string
	headers []header // lower-case full names, in order
	Body    string
}

type header struct{ name, value string }

// Header: the first value of a header ("" if none); name in lower case, full form.
func (m *Message) Header(name string) string {
	for _, h := range m.headers {
		if h.name == name {
			return h.value
		}
	}
	return ""
}

// All: every value of a header, in order (Via, Record-Route).
func (m *Message) All(name string) []string {
	var values []string
	for _, h := range m.headers {
		if h.name == name {
			values = append(values, h.value)
		}
	}
	return values
}

// CSeqMethod: the method of the CSeq header ("INVITE" in "3 INVITE").
func (m *Message) CSeqMethod() string {
	cseq := m.Header("cseq")
	return strings.TrimSpace(cseq[strings.Index(cseq, " ")+1:])
}

// CSeqNumber: the number of the CSeq header, 0 if missing.
func (m *Message) CSeqNumber() int {
	n, _ := strconv.Atoi(strings.Fields(m.Header("cseq") + " ")[0])
	return n
}

// fullName: compact header forms (RFC 3261 7.3.3) as their full lower-case names.
func fullName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "v":
		return "via"
	case "f":
		return "from"
	case "t":
		return "to"
	case "i":
		return "call-id"
	case "m":
		return "contact"
	case "l":
		return "content-length"
	case "c":
		return "content-type"
	}
	return name
}

// Parse reads one datagram; false if it is no SIP message.
func Parse(text string) (*Message, bool) {
	split := strings.Index(text, "\r\n\r\n")
	if split < 0 {
		return nil, false
	}
	m := &Message{Body: text[split+4:]}
	lines := strings.Split(text[:split], "\r\n")
	first := lines[0]
	if strings.HasPrefix(first, "SIP/2.0 ") {
		rest := first[8:]
		if len(rest) < 3 {
			return nil, false
		}
		m.Status, _ = strconv.Atoi(rest[:3])
		m.Reason = strings.TrimSpace(rest[3:])
	} else {
		m.Request = true
		space, last := strings.Index(first, " "), strings.LastIndex(first, " ")
		if space < 0 {
			return nil, false
		}
		m.Method = first[:space]
		if last > space {
			m.URI = first[space+1 : last]
		}
	}
	for _, line := range lines[1:] {
		// Folded lines continue the header before.
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(m.headers) > 0 {
			m.headers[len(m.headers)-1].value += " " + strings.TrimSpace(line)
			continue
		}
		if colon := strings.Index(line, ":"); colon > 0 {
			m.headers = append(m.headers, header{fullName(line[:colon]), strings.TrimSpace(line[colon+1:])})
		}
	}
	if n, err := strconv.Atoi(m.Header("content-length")); err == nil && n >= 0 && n < len(m.Body) {
		m.Body = m.Body[:n]
	}
	if m.Request {
		return m, m.Method != ""
	}
	return m, m.Status > 0
}

// uriUser: `"Name" <sip:user@host;params>;tag=x` -> user.
func uriUser(h string) string {
	start := strings.Index(h, "sip:")
	if start < 0 {
		start = strings.Index(h, "tel:")
	}
	if start < 0 {
		return ""
	}
	start += 4
	end := start
	for end < len(h) && !strings.ContainsRune("@;>", rune(h[end])) {
		end++
	}
	return h[start:end]
}

// uriOf: `<sip:user@host:port;transport=udp>` -> `sip:user@host:port;transport=udp`.
func uriOf(h string) string {
	if open := strings.Index(h, "<"); open >= 0 {
		if end := strings.Index(h[open:], ">"); end > 0 {
			return h[open+1 : open+end]
		}
		return h[open+1:]
	}
	if semicolon := strings.Index(h, ";"); semicolon >= 0 {
		return h[:semicolon]
	}
	return h
}

func randomHex(bytes int) string {
	b := make([]byte, bytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// withTag: the To header of a response gets our tag.
func withTag(h string) string {
	if strings.Contains(h, ";tag=") {
		return h
	}
	return h + ";tag=" + randomHex(4)
}

// buildResponse: a response to a request; to is the To header (with our tag in a call);
// contact is added to 1xx/2xx responses from 180 on.
func buildResponse(request *Message, status int, reason, to, contact, body string) string {
	var b strings.Builder
	b.WriteString("SIP/2.0 " + strconv.Itoa(status) + " " + reason + "\r\n")
	for _, v := range request.All("via") {
		b.WriteString("Via: " + v + "\r\n")
	}
	b.WriteString("From: " + request.Header("from") + "\r\nTo: " + to + "\r\nCall-ID: " +
		request.Header("call-id") + "\r\nCSeq: " + request.Header("cseq") + "\r\n")
	if status >= 180 && status < 300 {
		for _, v := range request.All("record-route") {
			b.WriteString("Record-Route: " + v + "\r\n")
		}
		b.WriteString("Contact: " + contact + "\r\n")
	}
	b.WriteString("User-Agent: smart-speaker\r\n")
	if body != "" {
		b.WriteString("Content-Type: application/sdp\r\n")
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	return b.String()
}
