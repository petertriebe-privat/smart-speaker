package google

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/vault"
)

// fakeDrive answers the few Drive requests the assistant makes, with a small evaluator for the
// queries it writes.
type fakeDrive struct {
	mu    sync.Mutex
	files map[string]*fakeFile
	next  int
}

type fakeFile struct {
	id, name, mime, parent, content string
	props                           map[string]string
}

var (
	qName     = regexp.MustCompile(`name=('(?:[^'\\]|\\.)*')`)
	qParent   = regexp.MustCompile(`'([^']+)' in parents`)
	qMIME     = regexp.MustCompile(`mimeType='([^']+)'`)
	qProperty = regexp.MustCompile(`appProperties has \{ key='([^']+)' and value='([^']*)' \}`)
	qFullText = regexp.MustCompile(`fullText contains '([^']*)'`)
)

func unquote(literal string) string {
	return strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(literal[1 : len(literal)-1])
}

func (f *fakeDrive) add(name, mimeType, parent, content string, props map[string]string) *fakeFile {
	f.next++
	file := &fakeFile{id: fmt.Sprintf("id%d", f.next), name: name, mime: mimeType, parent: parent, content: content, props: props}
	f.files[file.id] = file
	return file
}

func (f *fakeDrive) matches(file *fakeFile, q string) bool {
	if g := qName.FindStringSubmatch(q); g != nil && file.name != unquote(g[1]) {
		return false
	}
	if parents := qParent.FindAllStringSubmatch(q, -1); parents != nil {
		ok := false
		for _, p := range parents {
			ok = ok || p[1] == file.parent
		}
		if !ok {
			return false
		}
	}
	if types := qMIME.FindAllStringSubmatch(q, -1); types != nil {
		ok := false
		for _, m := range types {
			ok = ok || m[1] == file.mime
		}
		if !ok {
			return false
		}
	}
	for _, p := range qProperty.FindAllStringSubmatch(q, -1) {
		if file.props[p[1]] != p[2] {
			return false
		}
	}
	if terms := qFullText.FindAllStringSubmatch(q, -1); terms != nil {
		ok := false
		for _, t := range terms {
			ok = ok || strings.Contains(strings.ToLower(file.content), t[1])
		}
		if !ok {
			return false
		}
	}
	return true
}

func (f *fakeDrive) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	answer := func(status int, v any) (*http.Response, error) {
		data, _ := json.Marshal(v)
		if s, ok := v.(string); ok {
			data = []byte(s)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
	}
	path := req.URL.Path
	id := path[strings.LastIndexByte(path, '/')+1:]
	switch {
	case req.Method == "GET" && path == "/drive/v3/files":
		q := req.URL.Query().Get("q")
		var out []map[string]any
		for _, file := range f.files {
			if f.matches(file, q) {
				out = append(out, map[string]any{"id": file.id, "name": file.name, "mimeType": file.mime, "appProperties": file.props})
			}
		}
		return answer(200, map[string]any{"files": out})
	case req.Method == "GET" && req.URL.Query().Get("alt") == "media":
		return answer(200, f.files[id].content)
	case req.Method == "POST" && path == "/drive/v3/files":
		var meta struct {
			Name, MimeType string
			Parents        []string
		}
		json.Unmarshal(body, &meta)
		return answer(200, map[string]any{"id": f.add(meta.Name, meta.MimeType, meta.Parents[0], "", nil).id})
	case req.Method == "POST" && path == "/upload/drive/v3/files":
		_, params, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		part, _ := r.NextPart()
		var meta struct {
			Name, MimeType string
			Parents        []string
			AppProperties  map[string]string
		}
		json.NewDecoder(part).Decode(&meta)
		part, _ = r.NextPart()
		content, _ := io.ReadAll(part)
		return answer(200, map[string]any{"id": f.add(meta.Name, meta.MimeType, meta.Parents[0], string(content), meta.AppProperties).id})
	case req.Method == "PATCH" && strings.HasPrefix(path, "/upload/"):
		f.files[id].content = string(body)
		return answer(200, map[string]any{"id": id})
	case req.Method == "PATCH":
		var meta struct {
			Name, MimeType string
			AppProperties  map[string]string
		}
		json.Unmarshal(body, &meta)
		file := f.files[id]
		if meta.Name != "" {
			file.name, file.mime = meta.Name, meta.MimeType
		}
		if meta.AppProperties != nil {
			file.props = meta.AppProperties
		}
		return answer(200, map[string]any{"id": id})
	}
	return answer(404, "{}")
}

func fakeAccount(t *testing.T) (*Account, *fakeDrive) {
	store.Dir = t.TempDir()
	f := &fakeDrive{files: map[string]*fakeFile{}}
	a := &Account{creds: Credentials{ClientID: "c", ClientSecret: "s", RefreshToken: "r"}, http: &http.Client{Transport: f},
		tokens: map[string]*token{"r": {access: "a", until: time.Now().Add(time.Hour)}}}
	return a, f
}

// The archive is an Obsidian vault: old notes are converted and renamed, a topic follows its
// title, the search stays in the assistant's folder.
func TestArchiveVault(t *testing.T) {
	a, f := fakeAccount(t)
	root := f.add(prompts.Name(), folderMIME, "root", "", nil)
	other := f.add(prompts.Name(), folderMIME, "elsewhere", "", nil) // the same name, not the assistant's
	topics := f.add(driveTopics, folderMIME, root.id, "", nil)
	conversations := f.add(driveConversations, folderMIME, root.id, "", nil)
	own := f.add("Eigene Notizen", folderMIME, root.id, "", nil)
	f.add(".obsidian", folderMIME, root.id, "", nil)
	oldTopic := f.add("Thema [heizung]: Heizung.txt", "text/plain", topics.id,
		"Thema: Heizung\nSchlüssel: heizung\nStand (2026-10-02): Läuft.\nStichwörter: heizung\nStatus: offen\nAngelegt: 2026-10-01\n\nVerlauf:\n--- 2026-10-01 09:00 (Heizung kalt) ---\nKalt.\n",
		map[string]string{"topic": "heizung"})
	oldConv := f.add("Gespräch 2026-10-01 09:00 – Heizung kalt.txt", "text/plain", conversations.id,
		"Gespräch vom 2026-10-01 09:00: Heizung kalt\nKurzfassung: Kalt.\nStichwörter: heizung\nThemen: Heizung\n\nTranskript:\nAnna: Kalt.",
		map[string]string{"at": "2026-10-01 09:00"})
	household := f.add("Einkauf: Woche.txt", "text/plain", conversations.id, "Milch", nil)
	f.add("Werkstatt.md", "text/markdown", own.id, "Die Heizung macht Geräusche.", nil)
	f.add("Fremd.md", "text/markdown", other.id, "Heizung fremd", nil)

	archive := a.Archive()
	if more, err := archive.Tidy(10); more || err != nil {
		t.Fatalf("tidy: %v %v", more, err)
	}
	if root.props["assistant"] != "root" {
		t.Error("the folder is not marked")
	}
	if oldTopic.name != "Heizung.md" || oldTopic.mime != vault.MIME || !strings.HasPrefix(oldTopic.content, "---\ntyp: \"Thema\"") {
		t.Errorf("topic: %q %q\n%s", oldTopic.name, oldTopic.mime, oldTopic.content)
	}
	if oldConv.name != "2026-10-01 09.00 – Heizung kalt.md" || !strings.Contains(oldConv.content, "themen:\n  - \"[[Heizung]]\"") {
		t.Errorf("conversation: %q\n%s", oldConv.name, oldConv.content)
	}
	if household.name != "Einkauf – Woche.md" || household.content != "Milch" {
		t.Errorf("the household's note: %q %q", household.name, household.content)
	}

	// A new title renames the dossier, the history stays in it.
	text := "---\ntyp: \"Thema\"\n---\n# Heizung im Keller\n" + vault.HistoryMarker + "### 2026-11-14 18:38 – x\n"
	if err := archive.StoreTopic("heizung", "Thema [heizung]: Heizung im Keller", text); err != nil {
		t.Fatal(err)
	}
	if oldTopic.name != "Heizung im Keller.md" || oldTopic.content != text {
		t.Errorf("renamed: %q", oldTopic.name)
	}
	if err := archive.StoreConversation("2026-11-14 18:38", "Gespräch 2026-11-14 18:38 – Neu: Test", "x"); err != nil {
		t.Fatal(err)
	}
	if found, _ := archive.d.List(driveConversations, "name="+Quoted("2026-11-14 18.38 – Neu – Test.md"), 1); len(found) != 1 {
		t.Error("the new conversation note is missing")
	}

	// The search finds the household's own note in the vault, never the folder of the same name.
	notes, err := archive.Search([]string{"geräusche"})
	if err != nil || len(notes) != 1 || notes[0] != "Die Heizung macht Geräusche." {
		t.Errorf("search: %v %q", err, notes)
	}
	if notes, _ = archive.Search([]string{"fremd"}); len(notes) != 0 {
		t.Errorf("found outside the folder: %q", notes)
	}
}
