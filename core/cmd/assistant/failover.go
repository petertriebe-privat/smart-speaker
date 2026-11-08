package main

import (
	"bytes"
	"context"
	"echo-assistant/internal/prompts"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"echo-assistant/internal/board"
	"echo-assistant/internal/config"
	"echo-assistant/internal/errorlog"
	"echo-assistant/internal/peer"
)

// The primary role moves when its Echo fails (phase 5 of devices/echo/docs/18-several-echos.md, #64).
//
//   - The role lives in state_dir/role.json (primary, epoch), which no deploy touches: it stays
//     where a takeover moved it (decided 18.10.2026: no automatic return).
//   - A secondary pulls a mirror of the primary's state files every mirrorEvery and takes the
//     Google and MCP logins from it at once.
//   - Silent primary for takeoverAfter: the secondary with the highest priority, if it reaches
//     Home Assistant itself (not a Wi-Fi problem of its own), takes the mirror as its state,
//     writes role.json with a higher epoch and restarts as the primary (the service loop starts
//     it again within 10 s).
//   - At its start a device listens to the others first (settleFor); a primary that hears a
//     stronger primary (higher epoch; the same epoch and a higher priority) becomes secondary
//     before any poller starts. While running, the same rule demotes it.
//   - ctl primary moves the role by hand: the primary hands its state over and restarts as
//     secondary, this one restarts as primary.
//   - The primary tells the admin after a takeover and when another Echo is missing for long.

const (
	settleFor     = 7 * time.Second
	takeoverAfter = 120 * time.Second // a reboot of the primary takes ~45 s until the assistant runs, plus settleFor
	mirrorEvery   = 2 * time.Minute
	missingAfter  = 10 * time.Minute
	idleWaitMax   = 5 * time.Minute // a restart waits this long for the conversations to end
	mirrorDir     = "mirror"
	takeoverNote  = "takeover.txt"
	treeBudget    = 16 << 20 // readTree in all: the Echo has 190 MB and no swap
)

// mirrorSkip: per device, or the mirror itself (top-level names in state_dir). Never read: the
// wake word recordings ("voices", "raw", #61, ~150 MB) made the primary read every file up to
// 8 MB into RAM for each mirror pull, and Echo 1 hung until the watchdog reset it (20.10.2026).
var mirrorSkip = []string{"wake", "voice", "voices", "raw", "quick", "mobile-voice", "spotify", mirrorDir, mirrorDir + ".new",
	"errors.log", "health.log", config.RoleFile, config.RoleFile + ".new", takeoverNote}

type stateFiles struct {
	Epoch int               `json:"epoch"`
	Files map[string][]byte `json:"files"` // path in state_dir -> content
}

// settleRole: before the services start, listen to the others; a primary that finds a stronger
// one becomes secondary.
func (d *device) settleRole() {
	s := &d.cfg.Settings
	if d.peers == nil {
		return
	}
	time.Sleep(settleFor) // the others answer a new device's heartbeat at once
	if s.Primary {
		if q := d.strongerPrimary(); q != nil {
			log.Printf("[Failover] %s is the primary (epoch %d): this device is secondary", q.Name, q.Epoch)
			s.Primary, s.Epoch = false, q.Epoch
			if err := config.SaveRole(s.StateDir, false, q.Epoch); err != nil {
				log.Printf("[Failover] role: %v", err)
			}
		}
	}
	d.peers.SetRole(s.Primary, s.Epoch)
	log.Printf("[Failover] role: primary %v, epoch %d, %d other device(s)", s.Primary, s.Epoch, len(d.peers.Alive()))
}

// strongerPrimary: another primary that keeps the role against this one.
func (d *device) strongerPrimary() *peer.Peer {
	s := d.cfg.Settings
	for _, p := range d.peers.Alive() {
		if p.Primary && (p.Epoch > s.Epoch || p.Epoch == s.Epoch &&
			(p.Priority > s.Priority || p.Priority == s.Priority && p.Name < d.peers.Name)) {
			return &p
		}
	}
	return nil
}

// bestCandidate: no other secondary present comes before this one. A device that never leads
// (the M5) does not count: it would block the takeover.
func (d *device) bestCandidate() bool {
	s := d.cfg.Settings
	for _, p := range d.peers.Alive() {
		if !p.Primary && !p.NoLead && (p.Priority > s.Priority || p.Priority == s.Priority && p.Name < d.peers.Name) {
			return false
		}
	}
	return true
}

// reachesHA: Home Assistant answers here (a device that lost its own Wi-Fi must not take over).
func (d *device) reachesHA() bool {
	u, err := url.Parse(d.cfg.Secrets.String("HA_URL"))
	if err != nil || u.Host == "" {
		return true // nothing to check against
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(u.Hostname(), port), 3*time.Second)
	if err != nil {
		log.Printf("[Failover] Home Assistant not reachable: %v", err)
		return false
	}
	c.Close()
	return true
}

// watchRole: the role while running; every 5 s.
func (d *device) watchRole() {
	if d.peers == nil {
		return
	}
	lastPrimary, lastMirror, epoch := time.Now(), time.Time{}, d.cfg.Settings.Epoch
	known := map[string]time.Time{}
	reported := map[string]bool{}
	for range time.Tick(5 * time.Second) {
		s := d.cfg.Settings
		for _, p := range d.peers.Alive() {
			epoch = max(epoch, p.Epoch)
		}
		if s.Primary {
			if q := d.strongerPrimary(); q != nil {
				log.Printf("[Failover] %s is the primary (epoch %d): stepping down", q.Name, q.Epoch)
				if err := config.SaveRole(s.StateDir, false, q.Epoch); err == nil {
					d.restart("another Echo is the primary")
				}
				continue
			}
			d.reportMissing(known, reported)
			continue
		}
		if d.peers.Leader() != nil {
			lastPrimary = time.Now()
			every := mirrorEvery
			if board.Phone {
				every = phoneQuietEvery // 1.3 MB every 2 min on the phone's Wi-Fi; it never takes over
			}
			if time.Since(lastMirror) > every {
				if err := d.pullMirror(); err != nil {
					log.Printf("[Failover] mirror: %v", err)
				} else {
					lastMirror = time.Now()
				}
			}
			continue
		}
		if s.NoLead || time.Since(lastPrimary) < takeoverAfter || !d.bestCandidate() || !d.reachesHA() {
			continue
		}
		d.takeOver(fmt.Sprintf("the primary has been silent for %s", time.Since(lastPrimary).Round(time.Second)), epoch+1, nil)
	}
}

// reportMissing: the primary tells the admin once when an Echo it knew is gone for long.
func (d *device) reportMissing(known map[string]time.Time, reported map[string]bool) {
	for _, p := range d.peers.Alive() {
		if reported[p.Name] {
			log.Printf("[Failover] %s is back", p.Name)
		}
		known[p.Name], reported[p.Name] = p.Seen, false
	}
	for name, seen := range known {
		if !reported[name] && time.Since(seen) > missingAfter {
			reported[name] = true
			d.tellAdmin(fmt.Sprintf(prompts.Name()+": Der Echo „%s“ meldet sich seit %d Minuten nicht mehr.", name,
				int(time.Since(seen).Minutes())))
		}
	}
}

func (d *device) tellAdmin(text string) {
	log.Printf("[Failover] to the admin: %s", text)
	if d.svc.people == nil {
		return
	}
	if r := d.svc.people.Message("", text); strings.HasPrefix(r, "Error") {
		log.Printf("[Failover] not sent: %s", r)
	}
}

// reportTakeover: after a start as the primary, the note a takeover left goes to the admin.
func (d *device) reportTakeover() {
	path := filepath.Join(d.cfg.Settings.StateDir, takeoverNote)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if !d.cfg.Settings.Primary { // the role moved on before the note went out: it is stale
		os.Remove(path)
		return
	}
	time.Sleep(90 * time.Second) // Telegram and WhatsApp are up by then
	d.tellAdmin(strings.TrimSpace(string(data)))
	os.Remove(path)
}

// takeOver: this device becomes the primary: the state (files, or the mirror), the role, the
// note for the admin, a restart.
func (d *device) takeOver(why string, epoch int, files map[string][]byte) {
	s := d.cfg.Settings
	log.Printf("[Failover] taking over as the primary (epoch %d): %s", epoch, why)
	if files == nil {
		files = readTree(filepath.Join(s.StateDir, mirrorDir))
	}
	if err := writeFiles(s.StateDir, files); err != nil {
		log.Printf("[Failover] state: %v", err)
	}
	if err := config.SaveRole(s.StateDir, true, epoch); err != nil {
		log.Printf("[Failover] role: %v (no takeover)", err)
		return
	}
	note := fmt.Sprintf(prompts.Name()+": Der Echo „%s“ ist jetzt das Hauptgerät (Telefon, Telegram, WhatsApp, Mail, Aufgaben), "+
		"weil %s.", d.peers.Name, germanReason(why))
	os.WriteFile(filepath.Join(s.StateDir, takeoverNote), []byte(note), 0o600)
	d.restart("now the primary")
}

func germanReason(why string) string {
	switch {
	case strings.HasPrefix(why, "the primary has been silent"):
		return "sich das bisherige Hauptgerät nicht mehr meldet"
	case strings.HasPrefix(why, "by hand"):
		return "es so gewünscht war"
	}
	return why
}

// restart: the process ends once no conversation runs (at most idleWaitMax); the service loop
// starts it again with the new role.
func (d *device) restart(why string) {
	deadline := time.Now().Add(idleWaitMax)
	for d.anyOpen() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	errorlog.Add("Restart: %s", why)
	log.Printf("[Failover] restarting: %s", why)
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}

// --- The mirror --------------------------------------------------------------------------------

// pullMirror: the primary's state into state_dir/mirror; the logins from it at once.
func (d *device) pullMirror() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := d.peers.CallPrimary(ctx, "/state", []byte("{}"))
	if err != nil {
		return err
	}
	var st stateFiles
	if err := json.Unmarshal(out, &st); err != nil {
		return err
	}
	dir := d.cfg.Settings.StateDir
	tmp := filepath.Join(dir, mirrorDir+".new")
	os.RemoveAll(tmp)
	if err := writeFiles(tmp, st.Files); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(dir, mirrorDir))
	if err := os.Rename(tmp, filepath.Join(dir, mirrorDir)); err != nil {
		return err
	}
	d.applyLogins(st.Files)
	log.Printf("[Failover] mirror: %d files (%d KB)", len(st.Files), len(out)/1024)
	return nil
}

// applyLogins: the Google login and the MCP servers of the primary are this device's too.
func (d *device) applyLogins(files map[string][]byte) {
	dir := d.cfg.Settings.StateDir
	if g, ok := files["google.json"]; ok && d.svc.google != nil {
		current, _ := os.ReadFile(filepath.Join(dir, "google.json"))
		var saved struct {
			RefreshToken string `json:"refresh_token"`
			Web          bool   `json:"web"`
		}
		if !bytes.Equal(current, g) && json.Unmarshal(g, &saved) == nil && saved.RefreshToken != "" {
			if err := d.svc.google.SetRefreshToken(saved.RefreshToken, saved.Web); err == nil {
				log.Print("[Failover] Google login taken from the primary")
			}
		}
	}
	for _, name := range []string{"mcp.json", "drive.json"} {
		if f, ok := files[name]; ok {
			current, _ := os.ReadFile(filepath.Join(dir, name))
			if !bytes.Equal(current, f) {
				os.WriteFile(filepath.Join(dir, name), f, 0o600) // MCP servers: from the next start
				log.Printf("[Failover] %s taken from the primary", name)
			}
		}
	}
}

// serveState: the primary's state files for a secondary's mirror.
func (d *device) serveState([]byte) ([]byte, error) {
	if !d.cfg.Settings.Primary {
		return nil, errNotPrimary
	}
	return json.Marshal(stateFiles{Epoch: d.cfg.Settings.Epoch, Files: d.stateFiles()})
}

// serveGoogleLogin: the primary's Google login (its state_dir/google.json: refresh_token, web) for
// a member M5, which cannot pull the whole mirror (its RAM) and otherwise kept the copy in its
// secrets.h until that expired (08.11.2026). The same login the mirror carries, under the same
// signature.
func (d *device) serveGoogleLogin([]byte) ([]byte, error) {
	if !d.cfg.Settings.Primary {
		return nil, errNotPrimary
	}
	data, err := os.ReadFile(filepath.Join(d.cfg.Settings.StateDir, "google.json"))
	if err != nil {
		return nil, errors.New("no renewed Google login on the primary")
	}
	return data, nil
}

func (d *device) stateFiles() map[string][]byte {
	dir := d.cfg.Settings.StateDir
	return readTree(dir, mirrorSkip...)
}

// serveHandover: ctl primary on another Echo: the state goes there, this one steps down.
func (d *device) serveHandover([]byte) ([]byte, error) {
	s := d.cfg.Settings
	if !s.Primary {
		return nil, errNotPrimary
	}
	if d.anyOpen() {
		return nil, errors.New("a conversation runs here; try again after it")
	}
	out, err := json.Marshal(stateFiles{Epoch: s.Epoch, Files: d.stateFiles()})
	if err != nil {
		return nil, err
	}
	if err := config.SaveRole(s.StateDir, false, s.Epoch+1); err != nil {
		return nil, err
	}
	log.Print("[Failover] handing the primary role over")
	go func() {
		time.Sleep(2 * time.Second) // the answer goes out first
		d.restart("handed the primary role over")
	}()
	return out, nil
}

// becomePrimary: ctl primary.
func (d *device) becomePrimary() string {
	if d.peers == nil {
		return "error: peer link off"
	}
	if d.cfg.Settings.Primary {
		return "already the primary"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := d.peers.CallPrimary(ctx, "/handover", []byte("{}"))
	if err != nil {
		return "error: " + err.Error()
	}
	var st stateFiles
	if err := json.Unmarshal(out, &st); err != nil {
		return "error: " + err.Error()
	}
	go d.takeOver("by hand (ctl primary)", st.Epoch+1, st.Files)
	return fmt.Sprintf("taking over with %d files, restarting", len(st.Files))
}

// --- Files -------------------------------------------------------------------------------------

// readTree: the regular files under dir by slash path (up to 2 MB each, treeBudget in all: the
// state is ~0.4 MB, 20.10.2026); top-level names in skip are not read at all.
func readTree(dir string, skip ...string) map[string][]byte {
	files := map[string][]byte{}
	total := int64(0)
	filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if filepath.Dir(path) == filepath.Clean(dir) && slices.Contains(skip, e.Name()) {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil
		}
		if total += info.Size(); total > treeBudget {
			log.Printf("[Failover] %s holds more than %d MB of state: the rest is left out", dir, treeBudget>>20)
			return fs.SkipAll
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			files[filepath.ToSlash(rel)] = data
		}
		return nil
	})
	return files
}

// writeFiles: files into dir, each through a temporary file.
func writeFiles(dir string, files map[string][]byte) error {
	for name, data := range files {
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("bad path %q", name)
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
	}
	return nil
}
