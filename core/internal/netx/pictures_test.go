package netx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPictures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xff, 0xd8, 0xff})
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html></html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got := Pictures([]string{srv.URL + "/a.jpg", srv.URL + "/page", srv.URL + "/gone.jpg", "ftp://x"})
	want := []bool{true, false, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %v, want %v", i, got[i], want[i])
		}
	}
}
