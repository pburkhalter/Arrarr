package job

import "testing"

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to State
		ok       bool
	}{
		{StateNew, StateSubmitted, true},
		{StateNew, StateFailed, true},
		{StateNew, StateCanceled, true},
		{StateNew, StateReady, false},
		{StateNew, StateDownloading, false},
		{StateSubmitted, StateDownloading, true},
		{StateSubmitted, StateCompletedTorbox, true},
		{StateSubmitted, StateFailed, true},
		{StateSubmitted, StateNew, false},
		{StateDownloading, StateCompletedTorbox, true},
		{StateCompletedTorbox, StateReady, true},
		{StateCompletedTorbox, StateNew, false},
		{StateReady, StateNew, false},
		{StateFailed, StateNew, false},
		{StateNew, StateNew, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s,%s)=%v want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestTerminal(t *testing.T) {
	terminal := []State{StateReady, StateFailed, StateCanceled}
	nonTerminal := []State{StateNew, StateSubmitted, StateDownloading, StateCompletedTorbox}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}
