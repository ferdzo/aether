package internal

import (
	"testing"
	"time"
)

func TestShouldScaleToZeroRespectsWarmWindow(t *testing.T) {
	w := &Worker{lastInvoked: make(map[string]time.Time)}
	cfg := &ScalingConfig{ScaleToZeroAfter: 5 * time.Minute, WarmWindow: 10 * time.Minute}
	s := NewScaler(w, cfg)

	cases := []struct {
		name    string
		fn      string
		active  int64
		minIdle time.Duration
		want    bool
	}{
		{"cold and idle enough scales to zero", "fn-a", 0, 6 * time.Minute, true},
		{"active requests block to-zero", "fn-b", 2, 6 * time.Minute, false},
		{"below idle threshold blocks to-zero", "fn-c", 0, 4 * time.Minute, false},
	}
	for _, tc := range cases {
		if got := s.shouldScaleToZero(tc.fn, tc.active, tc.minIdle); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	w.lastInvoked["fn-warm"] = time.Now().Add(-2 * time.Minute)
	if s.shouldScaleToZero("fn-warm", 0, 6*time.Minute) {
		t.Fatal("recently invoked function must stay warm")
	}

	w.lastInvoked["fn-stale"] = time.Now().Add(-11 * time.Minute)
	if !s.shouldScaleToZero("fn-stale", 0, 6*time.Minute) {
		t.Fatal("invocation outside warm window must allow scale-to-zero")
	}

	disabled := NewScaler(w, &ScalingConfig{})
	if disabled.shouldScaleToZero("any", 0, time.Hour) {
		t.Fatal("ScaleToZeroAfter<=0 must disable scale-to-zero entirely")
	}
}
