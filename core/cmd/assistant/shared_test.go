package main

import (
	"strconv"
	"testing"
)

func TestReplayedRunsOnce(t *testing.T) {
	r := &replayed{answers: map[string][]byte{}}
	if _, ok := r.answered(""); ok {
		t.Error("a call without id counts as replayed")
	}
	r.done("a", []byte(`{"text":"ok"}`))
	if out, ok := r.answered("a"); !ok || string(out) != `{"text":"ok"}` {
		t.Errorf("replayed answer lost: %q %v", out, ok)
	}
	for i := range replaysKept {
		r.done(strconv.Itoa(i), nil)
	}
	if _, ok := r.answered("a"); ok {
		t.Error("the oldest answer is kept beyond the limit")
	}
	if len(r.answers) != replaysKept || len(r.order) != replaysKept {
		t.Errorf("kept %d answers, %d ids", len(r.answers), len(r.order))
	}
}
