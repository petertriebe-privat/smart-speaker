package live

import (
	"encoding/json"
	"log"
)

// GPT Live's own Decisions triggers (#59): the session runs Hooks.Triggers on its transcript of
// the user while they speak and sends each result as decision.event, pointing at the transcript
// event it judged. The device gets the answer for the whole first utterance with QuickCommand,
// without a request of its own (it took ~0.3 s through /v1/decisions, after its own
// transcription, which misheard where GPT Live did not).

// refereeSure: from this probability that a turn needs an action, talking without delegating it
// gets the reminder or the device's own help (nudgeIfClaimed).
const refereeSure = 0.8

// Decision: what the trigger made of the user's words so far.
type Decision struct {
	Choice     string
	Confidence float64 // -1: not given
	Whole      bool    // for the whole transcript so far, not an earlier part of it
}

// onInputDelta numbers the user's transcript events (called with s.mu held).
func (s *Session) onInputDelta(eventID string) {
	s.deltaCount++
	if eventID != "" {
		if s.deltaOrder == nil {
			s.deltaOrder = map[string]int{}
		}
		s.deltaOrder[eventID] = s.deltaCount
	}
}

// onDecision: a trigger's result. Results can come out of order: the latest part wins.
func (s *Session) onDecision(sourceEventID string, raw json.RawMessage) {
	var ev struct {
		Type     string `json:"type"`
		Decision struct {
			Answers []struct {
				Type        string   `json:"type"`
				Name        string   `json:"name"`
				Choice      string   `json:"choice"`
				Confidence  *float64 `json:"confidence"`
				Probability *float64 `json:"probability"`
			} `json:"answers"`
		} `json:"decision"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Type != "decision.completed" {
		return
	}
	for _, a := range ev.Decision.Answers {
		if a.Type == "predicate" && a.Name == "needs_action" && a.Probability != nil {
			// the referee: does the user's turn need an action? (nudgeIfClaimed)
			s.mu.Lock()
			if at := s.deltaOrder[sourceEventID]; at > s.turnStartAt {
				s.turnAction = *a.Probability
			}
			s.mu.Unlock()
			continue
		}
		if a.Type != "choice" || a.Name != "command" {
			continue
		}
		sure := -1.0
		if a.Confidence != nil {
			sure = *a.Confidence
		}
		s.mu.Lock()
		at := s.deltaOrder[sourceEventID]
		if at > 0 && at >= s.decisionAt && !s.quickChecked {
			s.decision, s.decisionAt = Decision{Choice: a.Choice, Confidence: sure}, at
			s.decisionNew = true
			log.Printf("[Decision] %s (%.2f) for the first %d transcript parts of %d", a.Choice, sure, at, s.deltaCount)
		}
		s.mu.Unlock()
	}
}

// currentDecision: the latest result, Whole when it covers every transcript part (s.mu held).
func (s *Session) currentDecisionLocked() Decision {
	d := s.decision
	d.Whole = s.decisionAt > 0 && s.decisionAt == s.deltaCount
	return d
}
