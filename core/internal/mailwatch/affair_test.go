package mailwatch

import (
	"testing"
	"time"
)

func TestAffairOf(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.Local)
	bill := sortedMail{Category: "finance", Affair: sortedAffair{Kind: "deadline", Title: "Rechnung bezahlen", Due: "2026-10-30", State: "open"}}
	if a, ok := affairOf(bill, false, now); !ok || a.Title != "Rechnung bezahlen" || a.Due != "2026-10-30" {
		t.Fatalf("bill: %+v %v", a, ok)
	}
	ad := bill
	ad.Category = "advertising"
	if _, ok := affairOf(ad, false, now); ok {
		t.Fatal("an advertisement became an affair")
	}
	none := sortedMail{Category: "shopping", Affair: sortedAffair{Kind: "none", State: "open"}}
	if _, ok := affairOf(none, false, now); ok {
		t.Fatal("kind none became an affair")
	}
	// Filing old mails: only what is still ahead.
	past := bill
	past.Affair.Due = "2026-10-01"
	if _, ok := affairOf(past, true, now); ok {
		t.Fatal("an old mail's past deadline became an affair")
	}
	if _, ok := affairOf(bill, true, now); !ok {
		t.Fatal("an old mail's coming deadline was dropped")
	}
	// A cancellation of a known matter needs no title.
	cancel := sortedMail{Category: "appointment", Affair: sortedAffair{Match: "a4", Kind: "trip", State: "obsolete"}}
	if a, ok := affairOf(cancel, false, now); !ok || a.Match != "a4" || a.State != "obsolete" {
		t.Fatalf("cancellation: %+v %v", a, ok)
	}
	// A receipt (kind none) that settles a known matter: the return is on its way back.
	receipt := sortedMail{Category: "shopping", Affair: sortedAffair{Match: "a14", Kind: "none", State: "done"}}
	if a, ok := affairOf(receipt, false, now); !ok || a.Match != "a14" || a.State != "done" {
		t.Fatalf("receipt: %+v %v", a, ok)
	}
}

func TestSenderName(t *testing.T) {
	for in, want := range map[string]string{
		`"Shop GmbH" <info@shop.example>`: "Shop GmbH",
		"<info@shop.example>":             "info@shop.example",
		"info@shop.example":               "info@shop.example",
	} {
		if got := senderName(in); got != want {
			t.Errorf("senderName(%q) = %q, want %q", in, got, want)
		}
	}
}
