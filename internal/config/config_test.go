package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("ARRARR_API_KEY", "sab-key")
	t.Setenv("TORBOX_API_KEY", "tb-key")
}

func TestLoadAcceptsMinimalEnv(t *testing.T) {
	setRequired(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.URLBase != "/sabnzbd" || c.APIKey != "sab-key" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

// `required` only checks presence: ARRARR_API_KEY="" used to load fine and
// switch SAB authentication off (the compose file's ${ARRARR_API_KEY} expands
// to empty when the host variable is unset).
func TestLoadRejectsEmptyKeys(t *testing.T) {
	for _, name := range []string{"ARRARR_API_KEY", "TORBOX_API_KEY"} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(name, "  ")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load with empty %s: err = %v, want a validation error naming it", name, err)
			}
		})
	}
}
