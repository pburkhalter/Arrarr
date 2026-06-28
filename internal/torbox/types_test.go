package torbox

import "testing"

func TestIsFailureMatchesAnnotatedStates(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		// Real-world failure observed 2026-06-28 (par2 insufficient on usenet).
		{"failed (Aborted, cannot be completed - https://sabnzbd.org/not-complete)", true},
		{"failed", true},
		{"FAILED", true},
		{"error (timed out)", true},
		{"missing_files", true},
		{"missing_files (3 of 12)", true},
		{"downloading", false},
		{"completed", false},
		{"queued", false},
		{"", false},
	}
	for _, c := range cases {
		got := (&MyListItem{DownloadState: c.state}).IsFailure()
		if got != c.want {
			t.Errorf("IsFailure(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestIsTerminalMatchesAnnotatedStates(t *testing.T) {
	cases := []struct {
		state    string
		finished bool
		want     bool
	}{
		{"completed", false, true},
		{"completed (verified)", false, true},
		{"cached", false, true},
		{"uploading (3/5)", false, true},
		{"downloading", false, false},
		{"failed (par2)", false, false}, // failure ≠ terminal
		{"", true, true},                // download_finished short-circuits
	}
	for _, c := range cases {
		got := (&MyListItem{DownloadState: c.state, DownloadFinished: c.finished}).IsTerminal()
		if got != c.want {
			t.Errorf("IsTerminal(state=%q,finished=%v) = %v, want %v", c.state, c.finished, got, c.want)
		}
	}
}
