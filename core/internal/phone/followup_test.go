package phone

import (
	"net"
	"strings"
	"testing"
	"time"

	"echo-assistant/internal/live"
	"echo-assistant/internal/store"
	"echo-assistant/internal/tools"
)

func TestNextAllowed(t *testing.T) {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.Local) }
	for _, c := range []struct{ in, want time.Time }{
		{at(22, 12, 30), at(22, 12, 30)}, // Thursday noon: as it is
		{at(22, 7, 0), at(22, 9, 0)},     // too early
		{at(22, 19, 0), at(23, 9, 0)},    // too late: the next morning
		{at(24, 20, 0), at(26, 9, 0)},    // Saturday evening: Monday, not Sunday
		{at(25, 11, 0), at(26, 9, 0)},    // Sunday
	} {
		if got := nextAllowed(c.in); !got.Equal(c.want) {
			t.Errorf("nextAllowed(%s) = %s, want %s", c.in.Format("Mon 02.01. 15:04"), got.Format("Mon 02.01. 15:04"),
				c.want.Format("Mon 02.01. 15:04"))
		}
	}
}

func TestParseOutcome(t *testing.T) {
	for _, c := range []struct{ answer, outcome, note string }{
		{"OUTCOME: done\nNOTE: Termin Montag 8 Uhr.", outcomeDone, "Termin Montag 8 Uhr."},
		{"outcome: Mailbox.\nnote: Nachricht hinterlassen", outcomeMailbox, "Nachricht hinterlassen"},
		{"OUTCOME: wait", outcomeWait, ""},
		{"I am not sure", outcomePartial, ""}, // doubtful: tried once more
		{"OUTCOME: maybe\nNOTE: x", outcomePartial, "x"},
	} {
		if outcome, note := parseOutcome(c.answer); outcome != c.outcome || note != c.note {
			t.Errorf("%q: %q %q", c.answer, outcome, note)
		}
	}
}

// A weekday at noon, so that nextAllowed keeps the times of the test.
var testNoon = time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)

func TestRecordAttemptPlansRetries(t *testing.T) {
	h := newHarness(t, nil)
	p := h.p
	p.startFollowupLocked(&callRequest{number: "07161123456", reason: "Fensterreinigung erfragen", by: "", at: testNoon}, testNoon)
	next, final := p.recordAttemptLocked("07161123456", outcomeBusy, "", "", false, testNoon)
	f := p.followupLocked("07161123456")
	if final || !f.RetryAt.Equal(testNoon.Add(10*time.Minute)) || !strings.Contains(next, "um 12:10 Uhr noch einmal an (Anruf 2 von 3)") {
		t.Fatalf("busy: %q %v %s", next, final, f.RetryAt)
	}
	// The retry: same task, the second call.
	p.startFollowupLocked(&callRequest{number: "07161123456", reason: "Fensterreinigung erfragen", retry: true}, testNoon)
	p.startFollowupLocked(&callRequest{number: "07161123456", reason: "Fensterreinigung erfragen", retry: true}, testNoon)
	next, final = p.recordAttemptLocked("07161123456", outcomePartial, "abgebrochen", "Gegenüber: Ja, doch", false, testNoon)
	if !final || !f.RetryAt.IsZero() || !strings.Contains(next, "Nach 3 Anrufen") || !f.Open {
		t.Fatalf("after three calls: %q %v %s open %v", next, final, f.RetryAt, f.Open)
	}
	// Done ends it; the calls stay as context for the next call to the number.
	next, final = p.recordAttemptLocked("07161123456", outcomeDone, "Termin Montag 8 Uhr", "", true, testNoon)
	if !final || next != "" || f.Open {
		t.Fatalf("done: %q %v open %v", next, final, f.Open)
	}
	history := historyText(f)
	if !strings.Contains(history, "they called back: done. Termin Montag 8 Uhr") || !strings.Contains(history, "The assistant called: busy") {
		t.Errorf("history %q", history)
	}
	// Kept on the data partition.
	var saved followupList
	if !store.LoadJSON(followupFile, &saved) || len(saved.Numbers) != 1 || len(saved.Numbers[0].Attempts) != 3 {
		t.Errorf("saved %+v", saved)
	}
	// A new task for the number starts its count again.
	p.startFollowupLocked(&callRequest{number: "07161123456", reason: "Termin absagen"}, testNoon)
	if f.Calls != 1 || f.Task != "Termin absagen" || len(f.Attempts) != 3 {
		t.Errorf("new task %+v", f)
	}
}

func TestNobodyAnswersIsRetriedLater(t *testing.T) {
	h := newHarness(t, nil)
	h.p.reg.registered = true
	h.p.dial(&callRequest{number: "0301234567", reason: "Tisch reservieren", stranger: true, at: time.Now()})
	inv := h.read(t)
	h.p.updateCall(time.Now().Add(ringTimeout + time.Second))
	h.read(t) // CANCEL
	select {
	case ev := <-h.events:
		if !strings.Contains(ev[3], "noch einmal an (Anruf 2 von 3)") {
			t.Errorf("the notice says nothing of the retry: %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("failed call not logged")
	}
	m, _ := Parse("SIP/2.0 487 Request Terminated\r\nCall-ID: " + inv.Header("call-id") + "\r\nTo: " + inv.Header("to") + ";tag=r\r\nCSeq: 1 INVITE\r\n\r\n")
	h.p.onResponse(m)
	h.read(t) // ACK
	h.sess.end()
	f := h.p.followupLocked("0301234567")
	if f == nil || f.RetryAt.IsZero() || f.Calls != 1 {
		t.Fatalf("follow-up %+v", f)
	}
	// Not before its time; then it is dialled again with the task and the earlier call.
	due := nextAllowed(f.RetryAt)
	h.p.retryIfDue(due.Add(-time.Minute))
	if h.p.wanted != nil {
		t.Fatal("retried too early")
	}
	h.p.retryIfDue(due)
	if h.p.wanted == nil || !h.p.wanted.retry || h.p.wanted.reason != "Tisch reservieren" {
		t.Fatalf("no retry: %+v", h.p.wanted)
	}
	h.p.dialIfRequested(time.Now())
	h.read(t) // INVITE
	if f.Calls != 2 {
		t.Errorf("calls %d", f.Calls)
	}
	reason := h.sess.started[len(h.sess.started)-1].Reason
	if !strings.HasPrefix(reason, "Tisch reservieren") || !strings.Contains(reason, "The assistant called: no_answer") {
		t.Errorf("the retry does not know the earlier call: %q", reason)
	}
}

func TestCallBackContinuesTheTask(t *testing.T) {
	h := newHarness(t, nil)
	h.p.deps.Delivery = func(string) string { return "telegram" }
	h.p.deps.Judge = func(task, transcript string) string { return "OUTCOME: done\nNOTE: Termin Montag 8 Uhr." }
	now := time.Now()
	h.p.startFollowupLocked(&callRequest{number: "0176123456", reason: "Fensterreinigung erfragen", reportCall: true}, now)
	h.p.recordAttemptLocked("0176123456", outcomeMailbox, "Nachricht hinterlassen", "", false, now)
	if h.p.followupLocked("0176123456").RetryAt.IsZero() {
		t.Fatal("no retry after the mailbox")
	}
	from := h.reg.LocalAddr().(*net.UDPAddr)
	h.p.onRequest(invite("cb1", "0176123456"), from)
	h.read(t)
	h.read(t)
	r := h.sess.started[0]
	if !r.Stranger || !strings.HasPrefix(r.Callback, "Fensterreinigung erfragen") || !strings.Contains(r.Callback, "mailbox") {
		t.Fatalf("call back request %+v", r)
	}
	if !h.p.followupLocked("0176123456").RetryAt.IsZero() {
		t.Error("the retry stays planned while they are on the line")
	}
	h.sess.set(live.Live)
	h.p.updateCall(time.Now())
	h.read(t) // 200
	bye, _ := Parse("BYE sip:jarvis@127.0.0.1 SIP/2.0\r\nVia: SIP/2.0/UDP 127.0.0.1:5060\r\nFrom: <sip:x@127.0.0.1>;tag=a\r\nTo: <sip:jarvis@127.0.0.1>;tag=b\r\nCall-ID: cb1\r\nCSeq: 2 BYE\r\n\r\n")
	h.p.onRequest(bye, from)
	h.read(t)
	h.sess.end()
	h.p.finished[r.Line.(*Line)] = finishedCall{ended: live.Ended{Request: r, Transcript: "Gegenüber: Montag um 8 passt."}, at: time.Now()}
	h.p.reportIfDue(time.Now())
	select {
	case run := <-h.runs:
		if run[1] != "Rückruf von 0176123456" || !strings.Contains(run[2], "Task: Fensterreinigung erfragen") ||
			!strings.Contains(run[2], "Montag um 8 passt") || run[3] != "call" {
			t.Errorf("report %q", run)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no report")
	}
	if f := h.p.followupLocked("0176123456"); f.Open || len(f.Attempts) != 2 || !f.Attempts[1].Incoming {
		t.Errorf("follow-up after the call back %+v", f)
	}
}

func TestAnsweredButCutOffIsRetried(t *testing.T) {
	h := newHarness(t, nil)
	h.p.deps.Delivery = func(string) string { return "telegram" }
	h.p.deps.Judge = func(task, transcript string) string {
		return "OUTCOME: partial\nNOTE: Nur „Ja, doch“, dann Stille."
	}
	h.p.startFollowupLocked(&callRequest{number: "0176123456", reason: "Fensterreinigung erfragen"}, time.Now())
	h.p.reports = []*pendingReport{{line: NewLine(rtpPort, nil, nil), number: "0176123456", task: "Fensterreinigung erfragen",
		followup: true, since: time.Now()}}
	h.p.report(h.p.reports[0], "Gegenüber: Ja, doch")
	select {
	case run := <-h.runs:
		if run[3] != "telegram" || !strings.Contains(run[2], "noch einmal an (Anruf 2 von 3)") {
			t.Errorf("report %q", run)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no report")
	}
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if f := h.p.followupLocked("0176123456"); f.RetryAt.IsZero() || !f.Open {
		t.Errorf("follow-up %+v", f)
	}
}

func TestReportByCallAndCancel(t *testing.T) {
	h := newHarness(t, nil)
	h.p.reg.registered = true
	var r tools.Registry
	h.p.AddTools(&r)
	ctx := tools.Context{Channel: tools.Voice}
	answer := r.Call("phone_call", `{"to":"0301234567","task":"Tisch für vier reservieren","report":"call"}`, ctx)
	if !strings.Contains(answer, "calls the user back") || h.p.wanted == nil || !h.p.wanted.reportCall {
		t.Fatalf("answer %q, wanted %+v", answer, h.p.wanted)
	}
	h.p.dialIfRequested(time.Now())
	h.read(t)
	if !h.p.call.reportCall || !h.p.followupLocked("0301234567").ReportCall {
		t.Error("report by call not kept")
	}
	if out := r.Call("call_followups", `{"action":"list","number":""}`, ctx); !strings.Contains(out, "Tisch für vier") {
		t.Errorf("list %q", out)
	}
	if out := r.Call("call_followups", `{"action":"cancel","number":"030 1234567"}`, ctx); !strings.Contains(out, "No more calls") {
		t.Errorf("cancel %q", out)
	}
	if h.p.followupLocked("0301234567").Open {
		t.Error("still open after cancel")
	}
}
