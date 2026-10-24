package configaccess

import (
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func authenticateWith(t *testing.T, header, value string) (*sdkaccess.Result, *sdkaccess.AuthError) {
	t.Helper()
	manager := sdkaccess.NewManager()
	manager.SetProviders(sdkaccess.RegisteredProviders())
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	return manager.Authenticate(req.Context(), req)
}

func TestRegisterAcceptsScopedKeys(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
	Register(&sdkconfig.SDKConfig{
		APIKeys:       []string{"owner"},
		ScopedAPIKeys: []sdkconfig.ScopedAPIKey{{APIKey: "scoped", Accounts: []string{"a.json"}}},
	})

	for _, tc := range []struct {
		header, value, principal string
	}{
		{"Authorization", "Bearer owner", "owner"},
		{"Authorization", "Bearer scoped", "scoped"},
		{"X-Api-Key", "scoped", "scoped"},
	} {
		result, err := authenticateWith(t, tc.header, tc.value)
		if err != nil || result == nil || result.Principal != tc.principal {
			t.Fatalf("%s %q: result=%+v err=%v, want principal %q", tc.header, tc.value, result, err, tc.principal)
		}
	}
	if _, err := authenticateWith(t, "Authorization", "Bearer wrong"); err == nil {
		t.Fatal("a wrong key was accepted")
	}
	if _, err := authenticateWith(t, "", ""); err == nil {
		t.Fatal("a request without a key was accepted")
	}
}

// With only scoped keys configured the provider must still be registered; an empty
// provider list would let every request through without a key.
func TestRegisterWithOnlyScopedKeysStillRequiresAKey(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
	Register(&sdkconfig.SDKConfig{
		ScopedAPIKeys: []sdkconfig.ScopedAPIKey{{APIKey: "scoped", Accounts: []string{"a.json"}}},
	})
	if _, err := authenticateWith(t, "", ""); err == nil {
		t.Fatal("a request without a key was accepted")
	}
	if result, err := authenticateWith(t, "Authorization", "Bearer scoped"); err != nil || result == nil || result.Principal != "scoped" {
		t.Fatalf("scoped key rejected: result=%+v err=%v", result, err)
	}
}

// Plugins and the Home control plane bypass per-key account limits, so scoped keys must
// then be rejected outright instead of being let in with every account.
func TestRegisterConfigRejectsScopedKeysWhenLimitsCannotBeEnforced(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
	newCfg := func() *config.Config {
		cfg := &config.Config{}
		cfg.APIKeys = []string{"owner"}
		cfg.ScopedAPIKeys = []config.ScopedAPIKey{{APIKey: "scoped", Accounts: []string{"a.json"}}}
		return cfg
	}
	for name, mutate := range map[string]func(*config.Config){
		"plugins enabled": func(c *config.Config) { c.Plugins.Enabled = true },
		"home enabled":    func(c *config.Config) { c.Home.Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := newCfg()
			mutate(cfg)
			if cfg.ScopedAPIKeysBlockedReason() == "" {
				t.Fatal("expected a blocked reason")
			}
			RegisterConfig(cfg)
			if _, err := authenticateWith(t, "Authorization", "Bearer scoped"); err == nil {
				t.Fatal("scoped key accepted although its limits cannot be enforced")
			}
			if result, err := authenticateWith(t, "Authorization", "Bearer owner"); err != nil || result == nil {
				t.Fatalf("owner key rejected: %v", err)
			}
		})
	}
	RegisterConfig(newCfg())
	if result, err := authenticateWith(t, "Authorization", "Bearer scoped"); err != nil || result == nil {
		t.Fatalf("scoped key rejected in a normal config: %v", err)
	}
}
