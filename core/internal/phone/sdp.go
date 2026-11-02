package phone

import (
	"net"
	"strconv"
	"strings"
)

// Media: what an SDP offer or answer says about the audio.
type Media struct {
	IP      net.IP
	Port    int
	Payload uint8 // 8 = A-law, 0 = µ-law
	DTMF    int   // payload type of telephone-event, -1 = none
}

// Addr: where the RTP goes.
func (m Media) Addr() *net.UDPAddr { return &net.UDPAddr{IP: m.IP, Port: m.Port} }

// ParseMedia reads the media of an offer or answer: address, port, A-law before µ-law, the
// telephone-event type. False if there is no G.711 or no usable address.
func ParseMedia(description string) (Media, bool) {
	m := Media{DTMF: -1}
	var types []int
	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			m.IP = net.ParseIP(strings.TrimSpace(line[9:]))
		case strings.HasPrefix(line, "m=audio "):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			m.Port, _ = strconv.Atoi(fields[1])
			types = types[:0]
			for _, f := range fields[3:] {
				if n, err := strconv.Atoi(f); err == nil {
					types = append(types, n)
				}
			}
		case strings.HasPrefix(line, "a=rtpmap:") && strings.Contains(line, "telephone-event/8000"):
			m.DTMF, _ = strconv.Atoi(line[9:strings.IndexAny(line+" ", " ")])
		}
	}
	offered := func(t int) bool {
		for _, v := range types {
			if v == t {
				return true
			}
		}
		return false
	}
	switch {
	case offered(payloadPCMA):
		m.Payload = payloadPCMA
	case offered(payloadPCMU):
		m.Payload = payloadPCMU
	default:
		return m, false
	}
	if m.DTMF >= 0 && !offered(m.DTMF) {
		m.DTMF = -1
	}
	return m, m.Port > 0 && m.IP != nil && !m.IP.IsUnspecified()
}

// BuildAnswer: our SDP for an incoming call (the negotiated codec and key type only).
func BuildAnswer(localIP string, rtpPort int, session int64, payload uint8, dtmf int) string {
	typ := strconv.Itoa(int(payload))
	text := "v=0\r\no=assistant " + strconv.FormatInt(session, 10) + " 1 IN IP4 " + localIP + "\r\ns=assistant\r\nc=IN IP4 " +
		localIP + "\r\nt=0 0\r\nm=audio " + strconv.Itoa(rtpPort) + " RTP/AVP " + typ
	if dtmf >= 0 {
		text += " " + strconv.Itoa(dtmf)
	}
	name := " PCMU/8000"
	if payload == payloadPCMA {
		name = " PCMA/8000"
	}
	text += "\r\na=rtpmap:" + typ + name + "\r\n"
	if dtmf >= 0 {
		d := strconv.Itoa(dtmf)
		text += "a=rtpmap:" + d + " telephone-event/8000\r\na=fmtp:" + d + " 0-16\r\n"
	}
	return text + "a=ptime:20\r\na=sendrecv\r\n"
}

// BuildOffer: our offer of an outgoing call: A-law, µ-law and the keys.
func BuildOffer(localIP string, rtpPort int, session int64) string {
	return "v=0\r\no=assistant " + strconv.FormatInt(session, 10) + " 1 IN IP4 " + localIP + "\r\ns=assistant\r\nc=IN IP4 " +
		localIP + "\r\nt=0 0\r\nm=audio " + strconv.Itoa(rtpPort) +
		" RTP/AVP 8 0 101\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\n" +
		"a=fmtp:101 0-16\r\na=ptime:20\r\na=sendrecv\r\n"
}
