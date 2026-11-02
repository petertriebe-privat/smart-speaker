package youtube

import (
	"encoding/json"
	"strings"
	"testing"

	"echo-assistant/internal/config"
	"echo-assistant/internal/tools"
)

func TestVideoID(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/watch?v=abc_DEF-123&t=1": "abc_DEF-123",
		"https://youtu.be/abc123?si=x":                    "abc123",
		"https://www.youtube.com/shorts/abc123":           "abc123",
		"  abc123 ":                                       "abc123",
		"":                                                "",
	} {
		if got := videoID(in); got != want {
			t.Errorf("videoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLEncodeAndUnescape(t *testing.T) {
	if got := urlEncode("a b&c/ü~"); got != "a%20b%26c%2F%C3%BC~" {
		t.Errorf("urlEncode = %q", got)
	}
	if got := unescaped("Tom &amp; Jerry &quot;x&quot; &#39;y&#39; &lt;z&gt;"); got != "Tom & Jerry \"x\" 'y' <z>" {
		t.Errorf("unescaped = %q", got)
	}
}

func TestFormatVideos(t *testing.T) {
	var parsed searchReply
	json.Unmarshal([]byte(`{"items":[{"id":{"videoId":"abc"},"snippet":{"title":"A &amp; B",
		"channelTitle":"Chan","publishedAt":"2030-02-03T10:00:00Z"}}]}`), &parsed)
	want := "Videos for \"q\" (most relevant first):\n- A & B | channel Chan | 03.02.2030 | " +
		"https://www.youtube.com/watch?v=abc [video_id abc]\n"
	if got := formatVideos("q", parsed); got != want {
		t.Errorf("formatVideos = %q", got)
	}
	if got := formatVideos("q", searchReply{}); got != "No videos found for \"q\"." {
		t.Errorf("empty = %q", got)
	}
}

func TestBetweenAndLocation(t *testing.T) {
	if got := between("<a>x</a>", "<a>", "</a>"); got != "x" {
		t.Errorf("between = %q", got)
	}
	if between("<a>x", "<a>", "</a>") != "" || between("x", "<a>", "</a>") != "" {
		t.Error("between should be empty")
	}
	if got := location("HTTP/1.1 200 OK\r\nLOCATION: http://192.0.2.1:8008/dd.xml\r\nST: x\r\n\r\n"); got != "http://192.0.2.1:8008/dd.xml" {
		t.Errorf("location = %q", got)
	}
}

func TestMatchTV(t *testing.T) {
	list := []tv{{"Living Room TV", "a"}, {"Bedroom TV", "b"}, {"Bedroom Stick", "c"}}
	if got, msg := matchTV(list, "living"); msg != "" || got.appURL != "a" {
		t.Errorf("living: %v %q", got, msg)
	}
	if _, msg := matchTV(list, "bedroom"); !strings.HasPrefix(msg, "Error: several TVs match \"bedroom\": \"Bedroom TV\"") {
		t.Errorf("several: %q", msg)
	}
	if _, msg := matchTV(list, "kitchen"); msg != "Error: no TV \"kitchen\"; TVs: \"Living Room TV\", \"Bedroom TV\", \"Bedroom Stick\"" {
		t.Errorf("none: %q", msg)
	}
	if _, msg := matchTV(nil, ""); msg != "Error: no TV \"\"; TVs: none found (are they switched on?)" {
		t.Errorf("empty: %q", msg)
	}
}

func TestAddTools(t *testing.T) {
	var r tools.Registry
	New(config.Secrets{}, nil, nil).AddTools(&r)
	if len(r.Names()) != 0 {
		t.Fatal("tools without a key")
	}
	New(config.Secrets{"GOOGLE_MAPS_API_KEY": "k"}, nil, nil).AddTools(&r)
	if got := strings.Join(r.Names(), " "); got != "find_video play_video control_video" {
		t.Errorf("tools = %q", got)
	}
	r = tools.Registry{}
	New(config.Secrets{"GOOGLE_MAPS_API_KEY": "k", "GEMINI_API_KEY": "g"}, nil, nil).AddTools(&r)
	if len(r.Names()) != 4 {
		t.Errorf("tools = %v", r.Names())
	}
}
