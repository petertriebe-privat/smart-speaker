package google

import (
	"bytes"
	"echo-assistant/internal/prompts"
	"encoding/json"
	"errors"
	"log"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
	"sync"

	"echo-assistant/internal/store"
)

// The assistant's files in the user's Google Drive: a visible folder "Jarvis" with the backups (core.json,
// people.json, tasks.json) and subfolders for the memory's archive ("Themen", "Gespräche") and the
// tables ("Tabellen"). Scope drive.file: The assistant sees only the files it made itself (or that
// tools/migrate_archive.py made with the same OAuth client). Drive keeps older versions of a
// file (revisions). Folder ids and the backups' file ids are kept in state_dir/drive.json. As
// devices/atoms3r/src/services/drive.cpp.

const (
	driveHost    = "https://www.googleapis.com"
	folderMIME   = "application/vnd.google-apps.folder"
	driveIDsFile = "drive.json" // {"folder": id, "folders": {name: id}, "files": {name: id}}
)

// Drive is the assistant's part of the user's Drive. Get it with Account.Drive.
type Drive struct {
	a  *Account
	mu sync.Mutex // the ids file
}

type driveIDs struct {
	Folder  string            `json:"folder,omitempty"`
	Folders map[string]string `json:"folders,omitempty"`
	Files   map[string]string `json:"files,omitempty"`
}

// Drive: the shared Drive client of this account.
func (a *Account) Drive() *Drive {
	a.drvOnce.Do(func() { a.drv = &Drive{a: a} })
	return a.drv
}

// Configured: the Google account is set up.
func (d *Drive) Configured() bool { return d.a.Enabled() }

// Quoted: text as a Drive query literal: 'text' with \ and ' escaped.
func Quoted(text string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, c := range []byte(text) {
		if c == '\\' || c == '\'' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
	return b.String()
}

// --- Cached ids: "folder" (The assistant), "folders" (subfolders), "files" (backups) ----------------------

func loadIDs(ids *driveIDs) bool { return store.LoadJSON(driveIDsFile, ids) }
func saveIDs(ids driveIDs) error { return store.SaveJSON(driveIDsFile, ids) }
func removeIDs()                 { store.Remove(driveIDsFile) }

func (d *Drive) readIDs() driveIDs {
	var ids driveIDs
	if !loadIDs(&ids) {
		ids = driveIDs{}
	}
	return ids
}

// cachedID: group "" = the folder the assistant.
func (d *Drive) cachedID(group, name string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.readIDs()
	switch group {
	case "":
		return ids.Folder
	case "folders":
		return ids.Folders[name]
	}
	return ids.Files[name]
}

// cacheID: id "" forgets it.
func (d *Drive) cacheID(group, name, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := d.readIDs()
	switch group {
	case "":
		ids.Folder = id
	case "folders":
		ids.Folders = setOrDelete(ids.Folders, name, id)
	default:
		ids.Files = setOrDelete(ids.Files, name, id)
	}
	if err := saveIDs(ids); err != nil {
		log.Printf("[Drive] id file not written: %v", err)
	}
}

func setOrDelete(m map[string]string, name, id string) map[string]string {
	if id == "" {
		delete(m, name)
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[name] = id
	return m
}

// forgetAll: something was deleted in Drive: look everything up again.
func (d *Drive) forgetAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	removeIDs()
}

// --- Requests -------------------------------------------------------------------------------------

func (d *Drive) json(method, path string, in, out any) error {
	ctx, cancel := callContext()
	defer cancel()
	return d.a.JSON(ctx, method, driveHost+path, in, out)
}

// searchOne: the id of the first file matching query, "" if none.
func (d *Drive) searchOne(query string) (string, error) {
	var answer map[string]any
	if err := d.json("GET", "/drive/v3/files?spaces=drive&fields=files(id)&pageSize=1&q="+enc(query), nil, &answer); err != nil {
		return "", err
	}
	return digS(answer, "files", 0, "id"), nil
}

func (d *Drive) createMetadata(metadata map[string]any) (string, error) {
	var answer map[string]any
	if err := d.json("POST", "/drive/v3/files?fields=id", metadata, &answer); err != nil {
		return "", err
	}
	id := digS(answer, "id")
	if id == "" {
		return "", errors.New("Error: Drive gave no id")
	}
	return id, nil
}

// findFolder: a folder named name in parent ("" = anywhere), made when missing.
func (d *Drive) findFolder(name, parent string) (string, error) {
	query := "name=" + Quoted(name) + " and mimeType='" + folderMIME + "' and trashed=false"
	if parent != "" {
		query += " and '" + parent + "' in parents"
	}
	id, err := d.searchOne(query)
	if err != nil || id != "" {
		return id, err
	}
	metadata := map[string]any{"name": name, "mimeType": folderMIME}
	if parent != "" {
		metadata["parents"] = []string{parent}
	}
	if id, err = d.createMetadata(metadata); err != nil {
		return "", err
	}
	log.Printf("[Drive] folder %s made", name)
	return id, nil
}

func (d *Drive) rootID() (string, error) {
	if id := d.cachedID("", ""); id != "" {
		return id, nil
	}
	id, err := d.findFolder(prompts.Name(), "") // the folder is named after the assistant
	if err != nil {
		return "", err
	}
	d.cacheID("", "", id)
	return id, nil
}

func (d *Drive) subfolderID(folder string) (string, error) {
	if id := d.cachedID("folders", folder); id != "" {
		return id, nil
	}
	root, err := d.rootID()
	if err != nil {
		return "", err
	}
	id, err := d.findFolder(folder, root)
	if err != nil {
		return "", err
	}
	d.cacheID("folders", folder, id)
	return id, nil
}

// fileID: a backup's id; "" (and no error) if there is none and make is false.
func (d *Drive) fileID(name, mime string, make bool) (string, error) {
	if id := d.cachedID("files", name); id != "" {
		return id, nil
	}
	root, err := d.rootID()
	if err != nil {
		return "", err
	}
	id, err := d.searchOne("name=" + Quoted(name) + " and '" + root + "' in parents and trashed=false")
	if err != nil {
		return "", err
	}
	if id == "" && make {
		if id, err = d.createMetadata(map[string]any{"name": name, "mimeType": mime, "parents": []string{root}}); err != nil {
			return "", err
		}
	}
	if id != "" {
		d.cacheID("files", name, id)
	}
	return id, nil
}

// --- Backups --------------------------------------------------------------------------------------

// Known: true once this device stored or found the backup name (its id is known).
func (d *Drive) Known(name string) bool { return d.cachedID("files", name) != "" }

// Store replaces the content of the backup name in the folder the assistant (makes folder and file when
// missing).
func (d *Drive) Store(name, mime string, data []byte) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var id string
		if id, err = d.fileID(name, mime, true); err != nil {
			return err
		}
		if err = d.Replace(id, mime, data); err == nil || StatusOf(err) != 404 {
			return err
		}
		d.forgetAll()
	}
	return err
}

// Load returns the content of the backup name; found is false (without error) if there is none.
func (d *Drive) Load(name string) (data []byte, found bool, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		id, err := d.fileID(name, "", false)
		if err != nil {
			return nil, false, err
		}
		if id == "" {
			return nil, false, nil
		}
		data, err := d.Download(id)
		if err == nil {
			return data, true, nil
		}
		if StatusOf(err) != 404 {
			return nil, false, err
		}
		d.forgetAll()
	}
	return nil, false, nil
}

// --- Files in subfolders (the archive, the tables) ------------------------------------------------

// Entry is a file in Drive.
type Entry struct {
	ID, Name   string
	Type       string // MIME type
	Modified   string // RFC 3339 time
	Source     string // appProperties.source
	Properties map[string]string
	Size       int
}

// List: files matching the Drive query ("" = all), newest first, at most limit: in the subfolder
// folder of Jarvis, or anywhere in the assistant's files (folder "").
func (d *Drive) List(folder, query string, limit int) ([]Entry, error) {
	q := "trashed=false"
	if folder != "" {
		parent, err := d.subfolderID(folder)
		if err != nil {
			return nil, err
		}
		q += " and '" + parent + "' in parents"
	}
	if query != "" {
		q += " and (" + query + ")"
	}
	var answer map[string]any
	err := d.json("GET", "/drive/v3/files?spaces=drive&orderBy=modifiedTime%20desc"+
		"&fields=files(id,name,mimeType,size,modifiedTime,appProperties)&pageSize="+strconv.Itoa(limit)+"&q="+enc(q), nil, &answer)
	if StatusOf(err) == 404 {
		d.forgetAll()
	}
	if err != nil {
		return nil, err
	}
	var found []Entry
	for _, file := range digL(answer, "files") {
		e := Entry{ID: digS(file, "id"), Name: digS(file, "name"), Type: digS(file, "mimeType"),
			Modified: digS(file, "modifiedTime"), Source: digS(file, "appProperties", "source")}
		e.Size, _ = strconv.Atoi(digS(file, "size")) // Drive sends the size as a string
		if props, ok := dig(file, "appProperties").(map[string]any); ok {
			e.Properties = map[string]string{}
			for k, v := range props {
				e.Properties[k], _ = v.(string)
			}
		}
		found = append(found, e)
	}
	return found, nil
}

// Download: the content of the file id.
func (d *Drive) Download(id string) ([]byte, error) {
	ctx, cancel := callContext()
	defer cancel()
	return d.a.Request(ctx, "GET", driveHost+"/drive/v3/files/"+id+"?alt=media", "", nil, "")
}

// Create makes a new file in the subfolder (made when missing), with properties (appProperties)
// to find it again. One request (multipart).
func (d *Drive) Create(folder, name, mime string, data []byte, properties map[string]string) error {
	parent, err := d.subfolderID(folder)
	if err != nil {
		return err
	}
	metadata := map[string]any{"name": name, "mimeType": mime, "parents": []string{parent}}
	if len(properties) > 0 {
		metadata["appProperties"] = properties
	}
	// multipart/related: the metadata, then the content, in one request.
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	head := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
	part, _ := w.CreatePart(head)
	json.NewEncoder(part).Encode(metadata)
	part, _ = w.CreatePart(textproto.MIMEHeader{"Content-Type": {mime}})
	part.Write(data)
	w.Close()
	ctx, cancel := callContext()
	defer cancel()
	_, err = d.a.Request(ctx, "POST", driveHost+"/upload/drive/v3/files?uploadType=multipart&fields=id",
		"multipart/related; boundary="+w.Boundary(), body.Bytes(), "")
	if StatusOf(err) == 404 {
		d.forgetAll() // the subfolder was deleted
	}
	return err
}

// CreateEmpty makes a new empty file of mime in the subfolder, e.g. a Google Sheet
// (application/vnd.google-apps.spreadsheet); returns its id.
func (d *Drive) CreateEmpty(folder, name, mime string) (string, error) {
	parent, err := d.subfolderID(folder)
	if err != nil {
		return "", err
	}
	return d.createMetadata(map[string]any{"name": name, "mimeType": mime, "parents": []string{parent}})
}

// Replace replaces the content of the file id.
func (d *Drive) Replace(id, mime string, data []byte) error {
	ctx, cancel := callContext()
	defer cancel()
	_, err := d.a.Request(ctx, "PATCH", driveHost+"/upload/drive/v3/files/"+id+"?uploadType=media&fields=id", mime, data, "")
	return err
}

// TrashFile moves one file to Drive's trash.
func (d *Drive) TrashFile(id string) error {
	return d.json("PATCH", "/drive/v3/files/"+id+"?fields=id", map[string]any{"trashed": true}, nil)
}

// Trash moves the subfolder with everything in it to Drive's trash (the user can restore it for 30
// days). No error when there was none.
func (d *Drive) Trash(folder string) error {
	root, err := d.rootID()
	if err != nil {
		return err
	}
	id, err := d.searchOne("name=" + Quoted(folder) + " and mimeType='" + folderMIME + "' and '" + root +
		"' in parents and trashed=false")
	if err != nil {
		return err
	}
	d.cacheID("folders", folder, "")
	if id == "" {
		return nil
	}
	return d.TrashFile(id)
}
