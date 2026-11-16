package main

import "testing"

// The move is only idempotent if the DLQ dedup id is stable per (stream, seq):
// a redelivered advisory must produce the SAME id so the DLQ DuplicateWindow
// collapses the second write. This is the one piece of logic that, if wrong,
// silently creates duplicate dead letters — so it gets a test.
func TestDlqMsgIDStableAndUnique(t *testing.T) {
	if got := dlqMsgID("TASKS", 42); got != "TASKS-42" {
		t.Fatalf("dlqMsgID = %q, want TASKS-42", got)
	}
	if dlqMsgID("TASKS", 42) != dlqMsgID("TASKS", 42) {
		t.Fatal("dlqMsgID must be stable for the same stream+seq (idempotent move)")
	}
	if dlqMsgID("TASKS", 42) == dlqMsgID("TASKS", 43) {
		t.Fatal("dlqMsgID must differ per sequence (no collisions between dead letters)")
	}
}

// buildDLQHeader must preserve enough provenance to (a) dedup the move and
// (b) replay the message back to where it came from.
func TestBuildDLQHeaderCarriesProvenance(t *testing.T) {
	adv := advisory{
		Type:      "max_deliveries",
		Stream:    "TASKS",
		Consumer:  "processor",
		StreamSeq: 7,
	}
	h := buildDLQHeader(adv, "tasks.resize", "job-123")

	cases := map[string]string{
		"Nats-Msg-Id":      "TASKS-7",      // dedup key for the DLQ write
		"Dlq-Id":           "TASKS-7",      // same id, stable
		"Original-Stream":  "TASKS",
		"Original-Seq":     "7",
		"Original-Subject": "tasks.resize", // without this, replay can't route
		"Failure-Reason":   "max_deliveries",
		"Original-Msg-Id":  "job-123",      // app dedup id survives into replay
	}
	for k, want := range cases {
		if got := h.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	// Term-Reason only appears on terminated advisories, never on max_deliveries.
	if h.Get("Term-Reason") != "" {
		t.Errorf("Term-Reason should be unset for max_deliveries, got %q", h.Get("Term-Reason"))
	}
}
