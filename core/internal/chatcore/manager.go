package chatcore

import (
	"log"
	"strings"
	"sync"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/store"
	"echo-assistant/internal/textutil"
)

// Memory is what the chats need of the memory (memory.Memory has it).
type Memory interface {
	// Consolidate hands a finished chat over like a conversation.
	Consolidate(transcript string, started time.Time, person, channel string)
	// SetOngoing shows a running chat in every prompt with this person ("" clears it).
	SetOngoing(person, text string)
}

// Chat is one person's chat: a Responses thread until it is quiet for the thread time, and the
// transcript that goes to the memory afterwards.
type Chat struct {
	Person        string // people id
	Thread        string // previous_response_id, "" = none
	Transcript    string
	Chatting      bool
	Started       time.Time
	LastMessageAt time.Time
}

// Manager keeps the chats of one channel. Message handling is sequential (one goroutine, the
// channel's Run); the mutex only protects the list.
type Manager struct {
	Label   string                     // "Telegram" / "WhatsApp" for logs and the ongoing note
	Channel string                     // the memory's channel (memory.Telegram / memory.WhatsApp)
	File    string                     // where unconsolidated chats are kept (in the store)
	Mem     Memory                     // nil = chats are only kept in the store
	Name    func(person string) string // people name of an id
	// Timeout after which a quiet chat is archived (TELEGRAM_THREAD_MS).
	Timeout time.Duration

	mu    sync.Mutex
	chats []*Chat
}

func transcriptMax() int { return prompts.Int("MEMORY_TRANSCRIPT_CHARS", 16000) }

// Of returns the person's chat, creating it.
func (m *Manager) Of(person string) *Chat {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.chats {
		if c.Person == person {
			return c
		}
	}
	c := &Chat{Person: person}
	m.chats = append(m.chats, c)
	return c
}

// Start marks a chat as running (first message after a pause).
func (m *Manager) Start(c *Chat) {
	if !c.Chatting {
		c.Chatting = true
		c.Started = time.Now()
	}
}

type savedChat struct {
	Person     string `json:"person"`
	Started    int64  `json:"started"`
	Transcript string `json:"transcript"`
}

// Save keeps the chats not consolidated yet in the store: a restart (update, crash) must not
// lose them.
func (m *Manager) Save() {
	m.mu.Lock()
	var list []savedChat
	for _, c := range m.chats {
		if c.Transcript != "" {
			list = append(list, savedChat{c.Person, c.Started.Unix(), c.Transcript})
		}
	}
	m.mu.Unlock()
	if len(list) == 0 {
		store.Remove(m.File)
		return
	}
	if err := store.SaveJSON(m.File, map[string]any{"chats": list}); err != nil {
		log.Printf("[%s] chat could not be saved: %v", m.Label, err)
	}
}

// Restore, after a restart: chats left in the store go to the memory (their threads are gone).
func (m *Manager) Restore() {
	if !store.Exists(m.File) {
		return
	}
	var doc struct {
		Chats []savedChat `json:"chats"`
	}
	read := store.LoadJSON(m.File, &doc)
	if read && m.Mem != nil {
		for _, c := range doc.Chats {
			m.Mem.Consolidate(c.Transcript, time.Unix(c.Started, 0), c.Person, m.Channel)
		}
	}
	if read {
		log.Printf("[%s] chats of the last start handed to the memory", m.Label)
	}
	store.Remove(m.File)
}

// Archive: the chat so far goes to the memory like a conversation.
func (m *Manager) Archive(c *Chat) {
	if c.Transcript != "" && m.Mem != nil {
		m.Mem.Consolidate(c.Transcript, c.Started, c.Person, m.Channel)
	}
	c.Transcript = ""
	c.Started = time.Now()
	if m.Mem != nil {
		m.Mem.SetOngoing(c.Person, "")
	}
	m.Save()
}

// ShowOngoing: until the chat is consolidated, every prompt with this person (also a voice
// conversation's) gets its end.
func (m *Manager) ShowOngoing(c *Chat) {
	if m.Mem == nil {
		return
	}
	const tail = 3000
	text := c.Transcript
	if len(text) > tail {
		line := strings.Index(text[len(text)-tail:], "\n")
		if line < 0 {
			text = text[len(text)-tail:]
		} else {
			text = text[len(text)-tail+line+1:]
		}
		text = "…\n" + text
	}
	name := ""
	if m.Name != nil {
		name = m.Name(c.Person)
	}
	m.Mem.SetOngoing(c.Person, "Ongoing "+m.Label+" chat with "+name+" since "+c.Started.Format("15:04")+
		" (not summarized yet):\n"+text)
}

// End archives the chat and forgets its thread ("/neu").
func (m *Manager) End(c *Chat) {
	m.Archive(c)
	c.Thread = ""
	c.Chatting = false
}

// Sweep ends the chats that were quiet for the timeout. Call regularly.
func (m *Manager) Sweep() {
	m.mu.Lock()
	chats := append([]*Chat(nil), m.chats...)
	m.mu.Unlock()
	for _, c := range chats {
		if c.Chatting && time.Since(c.LastMessageAt) > m.Timeout {
			m.End(c)
		}
	}
}

// Record adds an exchange to the transcript; how says in which form the message came
// ("per Telegram, Sprachnachricht").
func (m *Manager) Record(c *Chat, how, text, answer string) {
	c.LastMessageAt = time.Now()
	quarter := transcriptMax() / 4
	c.Transcript += "Du (" + how + "): " + textutil.Prefix(text, quarter) + "\n" + prompts.Name() + ": " + textutil.Prefix(answer, quarter) + "\n"
}

// Finish, after the answer went out: a long chat goes to the memory in parts, a short one is
// shown as ongoing and saved.
func (m *Manager) Finish(c *Chat) {
	if len(c.Transcript) > transcriptMax()/2 {
		m.Archive(c)
		return
	}
	m.ShowOngoing(c)
	m.Save()
}
