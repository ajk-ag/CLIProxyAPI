package config

// ScopedAPIKey is a client key limited to a fixed set of upstream accounts.
type ScopedAPIKey struct {
	// APIKey is the client key value.
	APIKey string `yaml:"api-key" json:"api-key"`
	// Name is an optional label for people; it is never used for matching.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Accounts lists the upstream accounts this key may use. Each entry matches an
	// auth file name (e.g. "claude-me@example.com.json"), an auth ID, or an auth_index.
	// An empty list allows nothing.
	Accounts []string `yaml:"accounts" json:"accounts"`
}

// ClientAPIKeys returns every client key the server should accept, unrestricted and scoped.
func (c *SDKConfig) ClientAPIKeys() []string {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(c.APIKeys)+len(c.ScopedAPIKeys))
	keys = append(keys, c.APIKeys...)
	for _, scoped := range c.ScopedAPIKeys {
		keys = append(keys, scoped.APIKey)
	}
	return keys
}

// ScopedAPIKeysBlockedReason reports why scoped keys cannot be enforced with this config,
// or "" when they can. Plugins can read any account's credentials and the Home control
// plane picks accounts remotely, so neither honours per-key account limits; scoped keys
// are then rejected instead of being allowed every account.
func (c *Config) ScopedAPIKeysBlockedReason() string {
	if c == nil || len(c.ScopedAPIKeys) == 0 {
		return ""
	}
	switch {
	case c.Home.Enabled:
		return "the Home control plane is enabled"
	case c.Plugins.Enabled:
		return "plugins are enabled"
	}
	return ""
}
