package youtube

import (
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
)

const searchWindow = 3 * time.Second // how long TVs may answer the SSDP search

// tv: friendly name and the DIAL Application-URL (".../apps/").
type tv struct {
	name, appURL string
}

// location: the Location header of an SSDP answer, "" if there is none.
func location(packet string) string {
	lower := strings.ToLower(packet)
	at := strings.Index(lower, "\nlocation:")
	if at < 0 {
		return ""
	}
	rest := packet[at+len("\nlocation:"):]
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// between: the text between open and close, "" if either is missing.
func between(text, open, close string) string {
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return ""
	}
	return text[start : start+end]
}

// discover: an SSDP search for DIAL devices, then each one's description: friendly name and the
// Application-URL header.
func (s *Service) discover() {
	var locations []string
	if conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 50000 + rand.IntN(10000)}); err == nil {
		search := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n" +
			"ST: urn:dial-multiscreen-org:service:dial:1\r\n\r\n"
		group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
		for i := 0; i < 2; i++ {
			conn.WriteTo([]byte(search), group)
		}
		conn.SetReadDeadline(time.Now().Add(searchWindow))
		packet := make([]byte, 1024)
		for {
			n, _, err := conn.ReadFrom(packet)
			if err != nil {
				break
			}
			loc := location(string(packet[:n]))
			known := loc == ""
			for _, seen := range locations {
				known = known || seen == loc
			}
			if !known {
				locations = append(locations, loc)
			}
		}
		conn.Close()
	}

	var found []tv
	for _, loc := range locations {
		resp, err := s.lan.Get(loc)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		appURL := resp.Header.Get("Application-URL")
		description := string(data)
		name := between(description, "<friendlyName>", "</friendlyName>")
		open, close := strings.Index(description, "<friendlyName>"), strings.Index(description, "</friendlyName>")
		if appURL != "" && open >= 0 && close > open {
			if !strings.HasSuffix(appURL, "/") {
				appURL += "/"
			}
			found = append(found, tv{name, appURL})
		}
	}
	log.Printf("[YouTube] %d TVs found (DIAL)", len(found))
	s.tvMu.Lock()
	s.tvs = found
	s.foundAt = time.Now()
	s.tvMu.Unlock()
}

// knownTVs: the TVs found last. An old list is used as it is and searched again behind it: the
// search takes 3-7 s, and "pause the video" waited for it after every restart (18.10.2026).
func (s *Service) knownTVs(fresh bool) []tv {
	s.tvMu.Lock()
	if !fresh && len(s.tvs) > 0 {
		list := append([]tv(nil), s.tvs...)
		stale := time.Since(s.foundAt) > time.Duration(prompts.Int("TV_DISCOVERY_MS", 3600000))*time.Millisecond
		s.tvMu.Unlock()
		if stale && s.searching.CompareAndSwap(false, true) {
			go func() {
				s.discover()
				s.searching.Store(false)
			}()
		}
		return list
	}
	s.tvMu.Unlock()
	s.discover()
	s.tvMu.Lock()
	defer s.tvMu.Unlock()
	return append([]tv(nil), s.tvs...)
}

func tvNames(list []tv) string {
	names := make([]string, len(list))
	for i, t := range list {
		names[i] = "\"" + t.name + "\""
	}
	if len(names) == 0 {
		return "none found (are they switched on?)"
	}
	return strings.Join(names, ", ")
}

// matchTV picks the one TV whose name contains wanted (case-insensitive); else an error text.
func matchTV(list []tv, wanted string) (tv, string) {
	var matches []tv
	key := strings.ToLower(wanted)
	for _, candidate := range list {
		if key != "" && strings.Contains(strings.ToLower(candidate.name), key) {
			matches = append(matches, candidate)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], ""
	case len(matches) > 1:
		return tv{}, "Error: several TVs match \"" + wanted + "\": " + tvNames(matches)
	}
	return tv{}, "Error: no TV \"" + wanted + "\"; TVs: " + tvNames(list)
}

// findTV: the TV matching wanted. fresh: search the LAN again.
func (s *Service) findTV(wanted string, fresh bool) (tv, string) {
	return matchTV(s.knownTVs(fresh), wanted)
}

// --- DIAL app state -----------------------------------------------------------------------

// appInfo: GET the app's DIAL state; "" on a failure. running: its state says so.
func (s *Service) appInfo(t tv) (body string, running bool) {
	req, err := http.NewRequest("GET", t.appURL+"YouTube", nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Origin", "https://www.youtube.com") // Fire TV refuses the YouTube app without it
	resp, err := s.lan.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	body = string(data)
	return body, strings.Contains(body, "<state>running</state>")
}
