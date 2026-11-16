package feature

import "testing"

// Pins the gating decisions to the API-level table so a future edit can't
// quietly let atomic batch "work" on 2.11. No server needed.
func TestFeatureGating(t *testing.T) {
	cases := []struct {
		name          string
		level         int // api.level the server reports
		wantTTL       bool
		wantAtomic    bool
	}{
		{"2.10-or-older", 0, false, false},
		{"2.11 (user's prod)", 1, true, false}, // TTL yes, atomic NO
		{"2.12", 2, true, true},
		{"2.15", 5, true, true},
	}
	for _, c := range cases {
		if got := SupportsPerMsgTTL(c.level); got != c.wantTTL {
			t.Errorf("%s: SupportsPerMsgTTL(%d)=%v, want %v", c.name, c.level, got, c.wantTTL)
		}
		if got := SupportsAtomicBatch(c.level); got != c.wantAtomic {
			t.Errorf("%s: SupportsAtomicBatch(%d)=%v, want %v", c.name, c.level, got, c.wantAtomic)
		}
	}
}
