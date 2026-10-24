package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const scopedKeysV8Config = `config-version: 8
server:
  port: 8317
access:
  api-keys:
    - owner-key
  # Keys limited to some accounts.
  scoped-api-keys:
    - api-key: friend-key
      name: friend
      accounts:
        - claude-b@example.com.json
        - 0123456789abcdef
`

func loadScopedTestConfig(t *testing.T, raw string) (string, *Config) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return file, cfg
}

func assertScopedKeysSavedInV8Layout(t *testing.T, file string, want []ScopedAPIKey) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(data); err != nil {
		t.Fatalf("saved config is not valid v8: %v\n%s", err, data)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if yamlPath(root, "scoped-api-keys") != nil {
		t.Fatalf("scoped keys were saved under the legacy root name:\n%s", data)
	}
	if strings.Contains(string(data), "# access.scoped-api-keys") {
		t.Fatalf("scoped keys were commented out as unknown:\n%s", data)
	}
	node := yamlPath(root, "access.scoped-api-keys")
	if node == nil {
		t.Fatalf("access.scoped-api-keys missing after save:\n%s", data)
	}
	var got []ScopedAPIKey
	if err = node.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved scoped keys = %+v, want %+v", got, want)
	}
	reloaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(reloaded.ScopedAPIKeys, want) {
		t.Fatalf("reloaded scoped keys = %+v, want %+v", reloaded.ScopedAPIKeys, want)
	}
}

func TestScopedAPIKeysLoadFromV8Layout(t *testing.T) {
	_, cfg := loadScopedTestConfig(t, scopedKeysV8Config)
	want := []ScopedAPIKey{{APIKey: "friend-key", Name: "friend", Accounts: []string{"claude-b@example.com.json", "0123456789abcdef"}}}
	if !reflect.DeepEqual(cfg.ScopedAPIKeys, want) {
		t.Fatalf("scoped keys = %+v, want %+v", cfg.ScopedAPIKeys, want)
	}
	if !reflect.DeepEqual(cfg.APIKeys, []string{"owner-key"}) {
		t.Fatalf("api-keys = %v; scoped keys must not be merged into api-keys", cfg.APIKeys)
	}
	if got := cfg.ClientAPIKeys(); !reflect.DeepEqual(got, []string{"owner-key", "friend-key"}) {
		t.Fatalf("ClientAPIKeys = %v", got)
	}
}

func TestScopedAPIKeysSurviveUnrelatedSave(t *testing.T) {
	file, cfg := loadScopedTestConfig(t, scopedKeysV8Config)
	cfg.Port = 8318
	if err := SaveConfigPreserveComments(file, cfg, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	assertScopedKeysSavedInV8Layout(t, file, cfg.ScopedAPIKeys)
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "# Keys limited to some accounts.") {
		t.Fatalf("save dropped the scoped keys comment:\n%s", data)
	}
}

func TestScopedAPIKeysAddedBySaveUseV8Layout(t *testing.T) {
	file, cfg := loadScopedTestConfig(t, "config-version: 8\nserver:\n  port: 8317\naccess:\n  api-keys:\n    - owner-key\n")
	cfg.ScopedAPIKeys = []ScopedAPIKey{{APIKey: "new-key", Accounts: []string{"codex-a@example.com.json"}}}
	if err := SaveConfigPreserveComments(file, cfg, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	assertScopedKeysSavedInV8Layout(t, file, cfg.ScopedAPIKeys)
}

func TestScopedAPIKeysCloneIsDeep(t *testing.T) {
	_, cfg := loadScopedTestConfig(t, scopedKeysV8Config)
	clone := cfg.CloneForRuntime()
	clone.ScopedAPIKeys[0].Accounts[0] = "changed.json"
	clone.ScopedAPIKeys[0].APIKey = "changed"
	if cfg.ScopedAPIKeys[0].Accounts[0] != "claude-b@example.com.json" || cfg.ScopedAPIKeys[0].APIKey != "friend-key" {
		t.Fatal("CloneForRuntime shares scoped key slices with the original")
	}
}
