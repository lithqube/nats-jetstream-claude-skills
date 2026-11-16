package orders

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOrderRoundTrip(t *testing.T) {
	in := Order{ID: "ord-1", Customer: "acme", Amount: 42.5}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Order
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch: got %+v want %+v", out, in)
	}
}

// The DLQ router must filter to the two advisories scoped to our stream, never
// the $JS.EVENT.ADVISORY.> firehose (which also carries a per-API-call audit
// advisory). Guard that intent.
func TestAdvisorySubjectsAreScoped(t *testing.T) {
	for _, subj := range []string{advMaxDeliveries, advMsgTerminated} {
		if !strings.Contains(subj, "."+StreamName+".") {
			t.Errorf("advisory %q is not scoped to stream %q", subj, StreamName)
		}
		if !strings.HasSuffix(subj, ".>") {
			t.Errorf("advisory %q should end with the consumer wildcard .>", subj)
		}
		if subj == "$JS.EVENT.ADVISORY.>" {
			t.Errorf("advisory %q is the whole-account firehose", subj)
		}
	}
	if !strings.Contains(advMaxDeliveries, "MAX_DELIVERIES") {
		t.Error("max-deliveries advisory subject is wrong")
	}
	if !strings.Contains(advMsgTerminated, "MSG_TERMINATED") {
		t.Error("terminated advisory subject is wrong")
	}
}
