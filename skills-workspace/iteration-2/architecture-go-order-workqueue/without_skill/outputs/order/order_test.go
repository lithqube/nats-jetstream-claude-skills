package order

import "testing"

// Self-check: validation is the only logic in this package that runs without a server.
func TestValidate(t *testing.T) {
	if err := (Order{ID: "o1", AmountCents: 100}).Validate(); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	if err := (Order{AmountCents: 100}).Validate(); err == nil {
		t.Fatal("empty id accepted")
	}
	if err := (Order{ID: "o1", AmountCents: 0}).Validate(); err == nil {
		t.Fatal("non-positive amount accepted")
	}
}
