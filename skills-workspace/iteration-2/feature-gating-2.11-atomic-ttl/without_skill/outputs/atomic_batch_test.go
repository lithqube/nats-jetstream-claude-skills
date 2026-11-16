package natsfeatures

import (
	"encoding/json"
	"testing"
)

// Round-trip: packing then unpacking must preserve subjects and payloads.
func TestEnvelopeRoundTrip(t *testing.T) {
	in := []EnvMsg{
		{Subject: "orders.created", Data: json.RawMessage(`{"id":1}`)},
		{Subject: "orders.line", Data: json.RawMessage(`{"sku":"abc"}`)},
	}
	payload, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnpackEnvelope(payload)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Subject != in[i].Subject || string(out[i].Data) != string(in[i].Data) {
			t.Errorf("msg %d = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestUnpackRejectsGarbage(t *testing.T) {
	if _, err := UnpackEnvelope([]byte("not json")); err == nil {
		t.Fatal("expected error on garbage input")
	}
}
