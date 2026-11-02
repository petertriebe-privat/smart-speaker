package live

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"echo-assistant/internal/prompts"
	"echo-assistant/internal/transcript"
)

func (s *Session) loop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	for range ticker.C {
		s.words.Update()
		s.tick()
	}
}

func (s *Session) tick() {
	s.mu.Lock()
	now := time.Now()
	state, age := s.state, now.Sub(s.stateSince)
	s.mu.Unlock()
	// Paused (someone at home is on the line themselves): the conversation waits, so none of the
	// timers below may speak up or end it. Only a connection that is still coming up or already
	// closing is carried on.
	if s.paused.Load() && state == Live {
		return
	}
	// Warm (Request.Warm): connected ahead, nothing may speak up or end it before Commit.
	if state == Live && s.Warm() {
		return
	}
	if state != Idle && state != Closing {
		s.acknowledgeIfSilent(now)
	}
	switch state {
	case Connecting, Starting:
		if age > connectTimeout {
			if state == Connecting {
				s.fail("Verbindung fehlgeschlagen")
			} else {
				s.fail("Keine Antwort")
			}
		}
	case Closing:
		if age > closeTimeout {
			s.finish() // final usage unconfirmed
		}
	case Live:
		if s.quickCommand(now) {
			return
		}
		if s.holdExpired(now) {
			s.mu.Lock()
			// GPT Live answered words it never transcribed: a guess ("ich starte sie" to a
			// "Pause" under loud music, 18.10.2026). 400 ms of voice, not 100: with voice_peak 120
			// the music's fading tail after the wake word counted, and "Wie bitte, Sir?" came
			// just as the user started the command (20.10.2026).
			heardNothing := s.userText.Len() == 0 && s.voiceMs >= notUnderstoodVoiceMs
			s.mu.Unlock()
			if heardNothing && s.hooks.NotUnderstood != nil && s.hooks.NotUnderstood() {
				log.Print("[Hold] the user was not understood: the held answer is dropped")
				s.mu.Lock()
				s.holdUntil = now.Add(time.Hour) // stays held until the device closed it
				s.mu.Unlock()
				return
			}
			go s.Release("deadline") // queuing the held answer may block for a moment
			return
		}
		if s.Holding() {
			return // no greeting, nudge or hang-up while the device decides
		}
		s.nudgeIfClaimed(now)
		s.greetIfSilent(now)
		s.deliverNotices(now)
		if s.State() == Live {
			s.hangUpWhenQuiet(now)
		}
	}
}

// quickCommand: what the user said first goes to the QuickCommand hook whenever the transcript
// grew and then rested for quickSilence (checking only once, at the first pause, missed "Spiele
// meine … Lieblingsmusik" on 16.10.2026), for up to quickWindow after their first word.
// Handled -> the conversation ends now.
func (s *Session) quickCommand(now time.Time) bool {
	s.mu.Lock()
	if s.quickChecked || s.hooks.QuickCommand == nil || s.userText.Len() == 0 {
		s.mu.Unlock()
		return false
	}
	if now.Sub(s.userFirst) > quickWindow {
		s.quickChecked = true
		s.mu.Unlock()
		return false
	}
	rest := now.Sub(s.userDelta)
	grew := s.userText.Len() != s.quickLen
	settled := rest >= quickSettled
	// once when it grew and rested briefly, once more when it settled
	fresh := s.decisionNew // a trigger's answer came: look again
	if rest < quickSilence || !grew && !fresh && (s.quickSettledSeen || !settled) {
		s.mu.Unlock()
		return false
	}
	if grew {
		s.quickSettledSeen = false
	}
	if settled {
		s.quickSettledSeen = true
	}
	s.quickLen = s.userText.Len()
	s.decisionNew = false
	text, decision := s.userText.String(), s.currentDecisionLocked()
	s.mu.Unlock()
	if !s.hooks.QuickCommand(text, settled, decision) {
		return false
	}
	s.mu.Lock()
	s.quickChecked = true
	s.mu.Unlock()
	log.Printf("[Quick] %q done on the device, conversation ended", text)
	s.out().Flush() // whatever GPT Live began to say about it
	s.Close()
	return true
}

// claimed: Jarvis says it is doing something ("Ich starte Eminem", "Wird gemacht").
// Also a done reported without anything done: "Video läuft, ich ziehe mich zurück" (18.10.2026).
var claimed = regexp.MustCompile(`(?i)\bich (starte|spiele|schalte|mache|sende|schicke|stelle|pausiere|lege|setze|trage|rufe|drucke|öffne|schließe|lösche|kümmere|erledige)\b|\bwird (gemacht|erledigt)\b|\b(läuft|laufen) (jetzt|wieder)\b|\b(video|musik|wiedergabe) läuft\b|\berledigt\b|\bist (jetzt )?(pausiert|gestartet|angehalten)\b`)

// promised: Jarvis asks to wait for an answer it still has to get ("Einen Augenblick, ich schaue
// nach"). Only a delegation brings that answer.
var promised = regexp.MustCompile(`(?i)\b(einen|ein) (moment|augenblick|sekunde)\b|\bgleich hab ich|\bich (schau|schaue|seh|sehe|prüfe|blick|blicke|hole|suche)\b|\bmoment(chen)?,? bitte\b`)

// nudgeIfClaimed: GPT Live announced an action and then did not delegate it ("Sehr wohl. Ich
// starte Eminem auf Ihrem Lautsprecher." and silence, several times on 16.10.2026). Once he is
// quiet for 3 s without a delegation since the user spoke, a system note tells him to do it.
func (s *Session) nudgeIfClaimed(now time.Time) {
	s.mu.Lock()
	// announced in words, or the referee (GPT Live's Decisions trigger) says the user's turn needs
	// an action: either way nothing was delegated while the assistant talked
	// (only once the assistant said something since the user's last words: it fired with nothing said,
	// three times while the user was still speaking, 18.10.2026)
	said := s.assistantSaid.String()
	// a promise to come back ("Gleich hab ich's, einen Moment") without a delegation in this turn
	// is no answer either: 20.10.2026 the assistant said it and the session ended in silence (#73); with
	// a delegation the backend answers (no tool needed)
	promise := promised.MatchString(said) && !s.delegatedTurn
	needed := strings.TrimSpace(said) != "" && (claimed.MatchString(said) || promise || s.turnAction >= refereeSure)
	// A delegation is not enough: the backend said "Erledigt, die Musik ist aus" without calling a
	// tool (18.10.2026). What counts is a tool that ran in this turn.
	if s.nudgedTurn || s.toolTurn || s.working || s.assistantDelta.IsZero() || now.Sub(s.assistantDelta) < 3*time.Second ||
		!needed {
		s.mu.Unlock()
		return
	}
	s.nudgedTurn = true
	user, delegated := s.turnUser.String(), s.delegatedTurn
	s.mu.Unlock()
	if s.out().Speaking(echoTail) {
		return
	}
	log.Printf("[Nudge] announced, nothing done (delegated %v): %.120q", delegated, said)
	if s.hooks.Unfulfilled == nil {
		if !delegated {
			s.Commentary(prompts.Text("LIVE_DELEGATE_NUDGE"))
		}
		return
	}
	// The reminder alone often did not help: "ich pausiere es" three times and nothing happened
	// (18.10.2026). What the device can do itself (music, the video), it does.
	go func() {
		recent := s.words.Full()
		if len(recent) > 800 {
			recent = recent[len(recent)-800:]
		}
		if note := s.hooks.Unfulfilled(user, strings.ToValidUTF8(recent, ""), said); note != "" {
			s.mu.Lock()
			s.delegatedTurn = true // a delegation that comes after all is not reminded again
			s.mu.Unlock()
			s.Commentary(note)
			return
		}
		if !delegated { // the reminder is about delegating; a delegation that did nothing has no fix here
			s.Commentary(prompts.Text("LIVE_DELEGATE_NUDGE"))
		}
	}()
}

// acknowledgeIfSilent: after the button the user goes on talking -> no clip (it would cover
// them); quiet for ACK_WAIT_MS -> "Ja, Sir?" (or the beep while the clips are not ready).
func (s *Session) acknowledgeIfSilent(now time.Time) {
	s.mu.Lock()
	if s.ackAt.IsZero() || now.Before(s.ackAt) {
		s.mu.Unlock()
		return
	}
	s.ackAt = time.Time{}
	if s.voiceMs >= 100 {
		s.mu.Unlock()
		log.Print("[Ack] none, the user is talking")
		return
	}
	s.mu.Unlock()
	var length time.Duration
	if s.hooks.Acknowledge != nil {
		length = s.hooks.Acknowledge()
	}
	if length == 0 {
		s.speaker.Tone(880, 120, 0.15)
		return
	}
	s.mu.Lock()
	s.ackQueued = time.Now()
	s.ackUntil = time.Now().Add(length + 400*time.Millisecond) // plus the jitter buffer's delay
	s.mu.Unlock()
}

// endsSilently: instead of the greeting the conversation ends without a word. Woken by the wake
// word and nobody said anything: most likely the TV or talk in the room. Alexa ends those silently;
// the greeting into the empty room made every false start heard and cost ~20 s of GPT Live
// (22.10.2026, #78). The button still gets its greeting.
func endsSilently(req Request, heardUser bool) bool {
	return req.Wake && !heardUser && req.Heard == "" && !req.Phone && req.Conference == "" && req.Reason == ""
}

// greetIfSilent: the user started the assistant but says nothing (yet): it greets instead of waiting in
// silence. When the assistant spoke up itself the occasion's greeting is used.
func (s *Session) greetIfSilent(now time.Time) {
	s.mu.Lock()
	if s.greetAt.IsZero() || now.Before(s.greetAt) {
		s.mu.Unlock()
		return
	}
	// The microphone heard the user but no transcript came yet: what was said during the
	// connection still goes through GPT Live's turn detection. A greeting now would talk over it.
	if !s.req.Phone && s.voiceMs >= 100 && (now.Sub(s.lastVoice) < greetingAfterVoice || now.Sub(s.stateSince) < greetingAfterVoice) {
		s.mu.Unlock()
		return
	}
	silent := endsSilently(s.req, s.heardUser)
	if silent && now.Sub(s.stateSince) < wakeSilentEnd {
		s.mu.Unlock()
		return // the user may still be thinking
	}
	s.greetAt = time.Time{}
	req := s.req
	acked := !s.ackUntil.IsZero()
	s.silentEnd = silent
	s.mu.Unlock()
	if silent {
		log.Print("[Wake word] nobody spoke: ended silently")
		s.Close()
		return
	}
	// "Ja, Sir?" already asked: a greeting on top made the user wait for a second prompt.
	if acked && !req.Phone && req.Reason == "" && req.Conference == "" {
		log.Print("[Greeting] none: the acknowledgement asked already")
		return
	}
	log.Print("[Greeting] the user is silent")
	switch {
	case req.Conference != "": // brought into a call: hello to everyone in it
		s.Commentary(strings.ReplaceAll(prompts.TextOr("LIVE_CONFERENCE_GREETING", conferenceGreeting), "{names}", req.Conference))
	case req.Phone && req.Callback != "": // a number the assistant called for the user calls back
		s.Commentary(prompts.Text("LIVE_CALLBACK_GREETING"))
	case req.Phone && req.CallerName != "": // someone from the address book called
		s.Commentary(strings.ReplaceAll(prompts.Text("LIVE_NAMED_GREETING"), "{name}", req.CallerName))
	case req.Phone && req.Stranger:
		s.Commentary(prompts.Text("LIVE_GUEST_GREETING"))
	case req.Phone:
		s.Commentary(prompts.Text("LIVE_PHONE_GREETING"))
	case req.Reason != "":
		s.Commentary(prompts.Text("LIVE_SPEAKER_GREETING"))
	default:
		s.Commentary(prompts.Text("LIVE_GREETING"))
	}
}

// deliverNotices: pending news goes to the model at a pause: not while the assistant speaks or the
// backend works, not while the user's answer is due, not before the greeting. Urgent news does
// not wait for the quiet; anything waits at most NOTICE_MAX_WAIT_MS.
func (s *Session) deliverNotices(now time.Time) {
	s.mu.Lock()
	if len(s.notices) == 0 || s.insisting {
		s.mu.Unlock()
		return
	}
	urgent, oldest := false, now
	for _, n := range s.notices {
		urgent = urgent || n.urgent
		if n.at.Before(oldest) {
			oldest = n.at
		}
	}
	silent := !s.out().Speaking(0) && !s.working && s.greetAt.IsZero()
	quiet := silent && now.Sub(s.lastActivity) > noticeQuiet && s.words.LastSpeaker() != transcript.User
	send := quiet || (urgent && silent) || now.Sub(oldest) > noticeMaxWait
	s.mu.Unlock()
	if send {
		s.sendNotices()
	}
}

func (s *Session) sendNotices() {
	s.mu.Lock()
	var items []string
	for _, n := range s.notices {
		prefix := "- "
		if n.urgent {
			prefix += "(urgent) "
		}
		items = append(items, prefix+n.text)
	}
	s.notices = nil
	s.lastActivity = time.Now() // the model answers: the silence timers start again
	s.mu.Unlock()
	if len(items) == 0 {
		return
	}
	list := strings.Join(items, "\n")
	log.Printf("[Notice] %s", list)
	s.Commentary(strings.ReplaceAll(prompts.Text("LIVE_NOTICE"), "{items}", list))
}

// offerAgenda: before hanging up, agenda entries that matter now and have not come up, once per
// conversation and only if someone talked. True if the model got them (hanging up waits).
func (s *Session) offerAgenda() bool {
	s.mu.Lock()
	if s.closingOffered || !s.heardUser || s.hooks.Closing == nil {
		s.mu.Unlock()
		return false
	}
	s.closingOffered = true
	s.mu.Unlock()
	s.words.Flush()
	items := s.hooks.Closing(s.words.Full())
	if items == "" {
		return false
	}
	log.Printf("[Agenda] before hanging up: %s", items)
	s.Commentary(strings.ReplaceAll(prompts.Text("LIVE_CLOSING_NOTICE"), "{items}", items))
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
	return true
}

// insist: The assistant spoke up itself and nobody answered yet: no hanging up, it tries again after
// a pause (a sleeper does not answer the first time), until SPEAKER_INSIST_MS.
func (s *Session) insist(now time.Time) {
	s.mu.Lock()
	s.endRequested = time.Time{} // not before the user answered
	if now.Sub(s.sessionSince) > speakerInsist {
		attempts := s.attempts + 1
		s.mu.Unlock()
		log.Printf("[Hang up] nobody answered at the speaker after %d attempts", attempts)
		s.Close()
		return
	}
	if !s.greetAt.IsZero() || s.out().Speaking(0) || s.working || now.Sub(s.lastActivity) < s.req.Repeat {
		s.mu.Unlock()
		return
	}
	s.attempts++
	s.lastActivity = now
	attempt := s.attempts + 1
	s.mu.Unlock()
	log.Printf("[Speaker] no answer, attempt %d", attempt)
	s.Commentary(strings.ReplaceAll(prompts.Text("LIVE_SPEAKER_AGAIN"), "{n}", fmt.Sprint(attempt)))
}

// hangUpWhenQuiet: silence = nothing audible, no transcript and no delegated work running.
func (s *Session) hangUpWhenQuiet(now time.Time) {
	speaking := s.out().Speaking(0)
	s.mu.Lock()
	// The transcript runs ahead of the audio: count the silence from the end of the audible
	// answer, not from the last transcript fragment.
	if speaking {
		s.lastActivity = now
	}
	if s.insisting {
		s.mu.Unlock()
		s.insist(now)
		return
	}
	busy := speaking || (s.working && now.Sub(s.workingSince) < thinkingTimeout)
	userLast := s.words.LastSpeaker() == transcript.User
	quietFor := now.Sub(s.lastActivity)
	pending := len(s.notices) > 0
	endRequested := s.endRequested
	working := s.working
	s.mu.Unlock()

	// News still pending: the model hears it before the conversation may end.
	if !speaking && pending && (!endRequested.IsZero() && now.Sub(endRequested) > 800*time.Millisecond ||
		endRequested.IsZero() && !working && quietFor > endCheckQuiet) {
		s.mu.Lock()
		s.endRequested = time.Time{}
		s.mu.Unlock()
		s.sendNotices()
		return
	}
	// end_call: GPT Live decided the conversation is over. Not waiting for more words: after
	// end_call GPT Live often narrates the tool's result.
	if !endRequested.IsZero() && now.Sub(endRequested) > 800*time.Millisecond && !speaking {
		log.Print("[Hang up] the assistant ends the conversation")
		s.Close()
		return
	}
	// Is the conversation over? Asked once per quiet moment after Jarvis spoke; a "no" is asked
	// once more if it stays quiet (one answer of the model can be wrong).
	s.mu.Lock()
	if !s.checked.Equal(s.lastActivity) {
		s.checks = 0
	}
	stamp := s.lastActivity
	// Realtime (push to talk): the user's key decides, silence is free: no end check.
	if !s.realtime() && !working && !speaking && !userLast && quietFor > endCheckQuiet && s.checks < 2 &&
		(s.checks == 0 || now.Sub(s.checkedAt) > endCheckQuiet+3*time.Second) {
		s.checked, s.checkedAt = stamp, now
		s.checks++
		s.mu.Unlock()
		s.words.Flush() // the last line reaches Full() only after a pause
		s.ended.check(s.words.Full(), stamp)
	} else {
		s.mu.Unlock()
	}
	if s.ended.over(stamp) && !speaking {
		if s.offerAgenda() {
			return
		}
		log.Print("[Hang up] the conversation is over")
		s.Close()
		return
	}
	// On the phone a pause is normal (the caller thinks, looks something up): wait longer.
	limit := idleTimeout
	switch {
	case s.realtime(): // the next press of the key goes on in the same conversation
		limit = realtimeIdle
	case s.Request().Conference != "": // the two talk with each other: he leaves after a while
		limit = conferenceIdle
	case s.Request().Phone:
		limit = phoneIdleTimeout
	case userLast:
		limit = answerTimeout
	}
	s.mu.Lock()
	sinceStart := now.Sub(s.stateSince)
	s.mu.Unlock()
	if !busy && quietFor > limit {
		if !userLast && s.offerAgenda() {
			return
		}
		who := prompts.Name()
		if userLast {
			who = "the user"
		}
		log.Printf("[Silence] %d s without speech, last spoke %s - ending the session", int(quietFor.Seconds()), who)
		s.Close()
	} else if sinceStart > maxSession && (!busy || sinceStart > maxSession+thinkingTimeout) {
		log.Print("[Maximum duration reached]")
		s.Close()
	}
}
