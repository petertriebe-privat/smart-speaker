package screen

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckPictures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xff, 0xd8})
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>login</html>"))
	}))
	defer srv.Close()
	good, bad := srv.URL+"/ok.jpg", srv.URL+"/page"
	c := card{Image: bad, Blocks: []map[string]any{
		{"type": "gallery", "items": []any{map[string]any{"url": good}, map[string]any{"url": bad}, bad}},
		{"type": "image", "url": bad},
		{"type": "gallery", "items": []any{bad}},
		{"type": "list", "items": []any{"a"}},
	}}
	total, dropped := checkPictures(&c)
	if total != 6 || dropped != 5 {
		t.Fatalf("total %d, dropped %d", total, dropped)
	}
	if c.Image != "" || len(c.Blocks) != 2 {
		t.Fatalf("image %q, blocks %v", c.Image, c.Blocks)
	}
	if items := c.Blocks[0]["items"].([]any); len(items) != 1 {
		t.Fatalf("gallery kept %v", items)
	}
	if c.Blocks[1]["type"] != "list" {
		t.Fatalf("second block %v", c.Blocks[1])
	}
}
