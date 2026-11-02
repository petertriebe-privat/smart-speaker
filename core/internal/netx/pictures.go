package netx

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pictureTimeout: a picture that does not answer this fast would not show on the TV either.
const pictureTimeout = 4 * time.Second

// Pictures returns which of the links answer with an image (status 200, an image content type),
// checked at once. Search results named pictures that the TV could not load: Instagram's links
// answer outside Instagram with a page or an error, and the gallery showed its heading alone
// (29.10.2026).
func Pictures(links []string) []bool {
	ok := make([]bool, len(links))
	client := Client(pictureTimeout)
	var wg sync.WaitGroup
	for i, link := range links {
		if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
			continue
		}
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			ok[i] = isPicture(client, link)
		}(i, link)
	}
	wg.Wait()
	return ok
}

func isPicture(client *http.Client, link string) bool {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return false
	}
	// a browser's request: some hosts refuse others outright
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 11; AFTKM) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "image/*")
	req.Header.Set("Range", "bytes=0-1023") // the head of the picture is enough
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return false
	}
	return strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "image/")
}
