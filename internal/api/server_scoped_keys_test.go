package api

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestIsUnrestrictedAPIKey(t *testing.T) {
	cfg := &config.Config{}
	cfg.APIKeys = []string{" owner ", "both"}
	cfg.ScopedAPIKeys = []config.ScopedAPIKey{{APIKey: "scoped"}, {APIKey: "both"}}
	for key, want := range map[string]bool{
		"owner":  true,
		"scoped": false,
		"both":   false,
		"other":  false,
		"":       false,
	} {
		if got := isUnrestrictedAPIKey(cfg, key); got != want {
			t.Fatalf("isUnrestrictedAPIKey(%q) = %v, want %v", key, got, want)
		}
	}
	if isUnrestrictedAPIKey(nil, "owner") {
		t.Fatal("nil config must not allow any key")
	}
}
