package maps

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"echo-assistant/internal/netx"
	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
)

// quotaFile is the M5's /quota.json: {"month": "2026-09", "count": {"places": 12}, "warned": {...}}.
const quotaFile = "quota.json"

type quotaDoc struct {
	Month  string          `json:"month"`
	Count  map[string]int  `json:"count"`
	Warned map[string]bool `json:"warned"`
}

// Quota counts requests per API and calendar month, before they are sent. One Quota is shared by
// every user of the same key (maps, youtube, websearch), as the M5 shares /quota.json.
type Quota struct {
	// Warn is called once a month and API at QUOTA_WARN_PERCENT with a German text for the admin
	// (the M5 sends it by Telegram). May be nil.
	Warn func(text string)

	mu     sync.Mutex
	now    func() time.Time
	synced func() bool
}

// NewQuota returns a counter using the system clock (only trusted once netx has synced it).
func NewQuota() *Quota {
	return &Quota{now: time.Now, synced: func() bool { return netx.TimeSynced }}
}

// Count counts one request of api unless the month's limit is reached; then the error text is for
// the model. The same method as the M5's maps::count().
func (q *Quota) Count(api string, limit int) error {
	if !q.synced() {
		return fmt.Errorf("Error: the clock is not set yet.")
	}
	month := q.now().Format("2006-01")
	var used int
	var warn bool
	q.mu.Lock()
	var doc quotaDoc
	if !store.LoadJSON(quotaFile, &doc) {
		doc = quotaDoc{}
	}
	if doc.Month != month {
		doc = quotaDoc{Month: month}
	}
	if doc.Count == nil {
		doc.Count = map[string]int{}
	}
	if doc.Warned == nil {
		doc.Warned = map[string]bool{}
	}
	used = doc.Count[api]
	if used >= limit {
		q.mu.Unlock()
		return fmt.Errorf("Error: monthly limit reached (%d %s requests this month); tell the user, it resets on the 1st.", limit, api)
	}
	used++
	doc.Count[api] = used
	if used*100 >= limit*prompts.Int("QUOTA_WARN_PERCENT", 80) && !doc.Warned[api] {
		doc.Warned[api] = true
		warn = true
	}
	if err := store.SaveJSON(quotaFile, doc); err != nil {
		log.Printf("[Maps] quota file not written: %v", err)
	}
	q.mu.Unlock()
	log.Printf("[Maps] %s request %d of %d this month", api, used, limit)
	if warn && q.Warn != nil {
		q.Warn(fmt.Sprintf("⚠️ Kontingent %s: %d von %d Anfragen in diesem Monat verbraucht. Bei %d sperrt "+prompts.Name()+" weitere.",
			api, used, limit, limit))
	}
	return nil
}

// Usage: the month's counters as compact JSON (tool maps_usage).
func (q *Quota) Usage() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var doc quotaDoc
	if !store.LoadJSON(quotaFile, &doc) {
		return "No requests counted yet."
	}
	data, _ := json.Marshal(doc)
	return string(data)
}
