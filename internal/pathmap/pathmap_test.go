package pathmap

import "testing"

func TestVisibleAndLocal(t *testing.T) {
	m := New("/mnt/torbox", "/torbox")
	got, err := m.Visible("Evil.S03E01.GERMAN.DUBBED.DL.1080p.BDRiP.x264.V2-TSCC")
	if err != nil {
		t.Fatal(err)
	}
	want := "/torbox/Evil.S03E01.GERMAN.DUBBED.DL.1080p.BDRiP.x264.V2-TSCC"
	if got != want {
		t.Errorf("Visible=%q want %q", got, want)
	}
	gotL, err := m.Local("Evil.S03E01.GERMAN.DUBBED.DL.1080p.BDRiP.x264.V2-TSCC")
	if err != nil {
		t.Fatal(err)
	}
	wantL := "/mnt/torbox/Evil.S03E01.GERMAN.DUBBED.DL.1080p.BDRiP.x264.V2-TSCC"
	if gotL != wantL {
		t.Errorf("Local=%q want %q", gotL, wantL)
	}
}

func TestRejectsTraversal(t *testing.T) {
	m := New("/mnt/torbox", "/torbox")
	bad := []string{"", "..", ".", "../etc", "evil/../passwd", "x\\y", "/abs"}
	for _, name := range bad {
		if _, err := m.Local(name); err == nil {
			t.Errorf("Local(%q) should error", name)
		}
		if _, err := m.Visible(name); err == nil {
			t.Errorf("Visible(%q) should error", name)
		}
	}
}

func TestTrimsTrailingSlashOnBases(t *testing.T) {
	m := New("/mnt/torbox/", "/torbox/")
	got, _ := m.Visible("foo")
	if got != "/torbox/foo" {
		t.Errorf("Visible=%q want /torbox/foo", got)
	}
}
