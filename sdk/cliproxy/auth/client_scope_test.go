package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	scopeTestOwnerKey   = "owner-key"
	scopeTestKeyA       = "key-a"
	scopeTestKeyB       = "key-b"
	scopeTestUnknownKey = "not-a-configured-key"
)

// scopeRecordingExecutor records every auth it is asked to use and can fail some of them.
type scopeRecordingExecutor struct {
	id string

	mu      sync.Mutex
	used    []string
	failIDs map[string]bool
}

func (e *scopeRecordingExecutor) Identifier() string { return e.id }

func (e *scopeRecordingExecutor) record(auth *Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.used = append(e.used, auth.ID)
	if e.failIDs[auth.ID] {
		return &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
	}
	return nil
}

func (e *scopeRecordingExecutor) usedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.used...)
}

func (e *scopeRecordingExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *scopeRecordingExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.record(auth); err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(`{}`)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *scopeRecordingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *scopeRecordingExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *scopeRecordingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type scopeTestEnv struct {
	manager *Manager
	claude  *scopeRecordingExecutor
	codex   *scopeRecordingExecutor
	// Auth IDs per owner: A owns claudeA1, claudeA2, codexA; B owns claudeB, codexB.
	claudeA1, claudeA2, claudeB, codexA, codexB *Auth
}

const (
	scopeTestClaudeModel = "claude-scope-test-model"
	scopeTestCodexModel  = "codex-scope-test-model"
)

func newScopeTestEnv(t *testing.T) *scopeTestEnv {
	t.Helper()
	return newScopeTestEnvWithSelector(t, nil)
}

// A non-built-in selector sends selection through the legacy pick functions.
func newScopeTestEnvWithSelector(t *testing.T, selector Selector) *scopeTestEnv {
	t.Helper()
	env := &scopeTestEnv{
		manager: NewManager(nil, selector, nil),
		claude:  &scopeRecordingExecutor{id: "claude", failIDs: map[string]bool{}},
		codex:   &scopeRecordingExecutor{id: "codex", failIDs: map[string]bool{}},
	}
	env.manager.RegisterExecutor(env.claude)
	env.manager.RegisterExecutor(env.codex)

	suffix := uuid.NewString()
	newAuth := func(provider, file, model string) *Auth {
		auth := &Auth{
			ID:         file + "-" + suffix,
			Provider:   provider,
			FileName:   file,
			Attributes: map[string]string{"api_key": "upstream-" + file},
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		if _, err := env.manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", file, err)
		}
		return auth
	}
	env.claudeA1 = newAuth("claude", "claude-a1@example.com.json", scopeTestClaudeModel)
	env.claudeA2 = newAuth("claude", "claude-a2@example.com.json", scopeTestClaudeModel)
	env.claudeB = newAuth("claude", "claude-b@example.com.json", scopeTestClaudeModel)
	env.codexA = newAuth("codex", "codex-a@example.com.json", scopeTestCodexModel)
	env.codexB = newAuth("codex", "codex-b@example.com.json", scopeTestCodexModel)

	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{scopeTestOwnerKey}
	cfg.ScopedAPIKeys = []internalconfig.ScopedAPIKey{
		// Key A names one account by file name and one by auth ID, plus the codex account.
		{APIKey: scopeTestKeyA, Name: "a", Accounts: []string{env.claudeA1.FileName, env.claudeA2.ID, env.codexA.FileName}},
		// Key B names its accounts by auth_index.
		{APIKey: scopeTestKeyB, Name: "b", Accounts: []string{env.claudeB.Index, env.codexB.Index}},
	}
	cfg.RequestRetry = 0
	cfg.MaxRetryInterval = 0
	env.manager.SetConfig(cfg)
	t.Cleanup(func() { setClientAccountScopes(nil) })
	return env
}

func keyContext(key string) context.Context {
	return coreusage.WithAPIKey(context.Background(), key)
}

func assertOnlyUsed(t *testing.T, used []string, allowed ...*Auth) {
	t.Helper()
	allowedIDs := make(map[string]bool, len(allowed))
	for _, auth := range allowed {
		allowedIDs[auth.ID] = true
	}
	for _, id := range used {
		if !allowedIDs[id] {
			t.Fatalf("an account outside the key's scope was used: %s (used=%v)", id, used)
		}
	}
}

func TestClientScope_ScopedKeyOnlyUsesItsAccounts(t *testing.T) {
	env := newScopeTestEnv(t)
	for i := 0; i < 30; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyA), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	used := env.claude.usedIDs()
	assertOnlyUsed(t, used, env.claudeA1, env.claudeA2)
	seen := map[string]bool{}
	for _, id := range used {
		seen[id] = true
	}
	if !seen[env.claudeA1.ID] || !seen[env.claudeA2.ID] {
		t.Fatalf("expected rotation across both of key A's accounts, used=%v", used)
	}

	for i := 0; i < 10; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyB), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("key B execute %d: %v", i, err)
		}
	}
	assertOnlyUsed(t, env.claude.usedIDs()[len(used):], env.claudeB)
}

func TestClientScope_ExhaustedScopeFailsInsteadOfBorrowing(t *testing.T) {
	env := newScopeTestEnv(t)
	env.claude.failIDs[env.claudeA1.ID] = true
	env.claude.failIDs[env.claudeA2.ID] = true
	for i := 0; i < 5; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyA), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err == nil {
			t.Fatalf("execute %d succeeded although every account of key A is exhausted", i)
		}
	}
	assertOnlyUsed(t, env.claude.usedIDs(), env.claudeA1, env.claudeA2)
}

func TestClientScope_StreamAndCountTokensStayInScope(t *testing.T) {
	env := newScopeTestEnv(t)
	for i := 0; i < 10; i++ {
		result, err := env.manager.ExecuteStream(keyContext(scopeTestKeyB), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		for range result.Chunks {
		}
		if _, err = env.manager.ExecuteCount(keyContext(scopeTestKeyB), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("count %d: %v", i, err)
		}
	}
	assertOnlyUsed(t, env.claude.usedIDs(), env.claudeB)
}

func TestClientScope_MixedProvidersStayInScope(t *testing.T) {
	env := newScopeTestEnv(t)
	for i := 0; i < 20; i++ {
		_, _ = env.manager.Execute(keyContext(scopeTestKeyB), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: scopeTestCodexModel}, cliproxyexecutor.Options{})
		_, _ = env.manager.Execute(keyContext(scopeTestKeyB), []string{"codex", "claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{})
	}
	assertOnlyUsed(t, env.codex.usedIDs(), env.codexB)
	assertOnlyUsed(t, env.claude.usedIDs(), env.claudeB)
	if len(env.codex.usedIDs()) == 0 || len(env.claude.usedIDs()) == 0 {
		t.Fatalf("expected key B to reach its own accounts, codex=%v claude=%v", env.codex.usedIDs(), env.claude.usedIDs())
	}
}

func TestClientScope_UnrestrictedKeyReachesEveryAccount(t *testing.T) {
	env := newScopeTestEnv(t)
	for i := 0; i < 30; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestOwnerKey), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for _, id := range env.claude.usedIDs() {
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Fatalf("owner key should rotate across all three claude accounts, used=%v", env.claude.usedIDs())
	}
}

func TestClientScope_MissingOrUnknownKeyGetsNoAccount(t *testing.T) {
	env := newScopeTestEnv(t)
	for name, ctx := range map[string]context.Context{
		"no key":      context.Background(),
		"unknown key": keyContext(scopeTestUnknownKey),
	} {
		if _, err := env.manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err == nil {
			t.Fatalf("%s: execute succeeded while scoped keys are configured", name)
		}
		if _, err := env.manager.ExecuteStream(ctx, []string{"claude", "codex"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{Stream: true}); err == nil {
			t.Fatalf("%s: stream succeeded while scoped keys are configured", name)
		}
	}
	if used := append(env.claude.usedIDs(), env.codex.usedIDs()...); len(used) != 0 {
		t.Fatalf("no account may be used without a known client key, used=%v", used)
	}
}

func TestClientScope_PinnedAuthOutsideScopeIsRefused(t *testing.T) {
	env := newScopeTestEnv(t)
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: env.claudeB.ID}}
	if _, err := env.manager.Execute(keyContext(scopeTestKeyA), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, opts); err == nil {
		t.Fatal("key A reached key B's account through a pinned auth ID")
	}
	if used := env.claude.usedIDs(); len(used) != 0 {
		t.Fatalf("no account may be used, used=%v", used)
	}
}

// scopeGreedyPlugin always asks for a fixed auth, and records the candidates it was offered.
type scopeGreedyPlugin struct {
	want     string
	delegate string

	mu      sync.Mutex
	offered []string
}

func (p *scopeGreedyPlugin) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	p.mu.Lock()
	for _, candidate := range req.Candidates {
		p.offered = append(p.offered, candidate.ID)
	}
	p.mu.Unlock()
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: p.want, DelegateBuiltin: p.delegate}, true, nil
}

func TestClientScope_PluginSchedulerOnlySeesAllowedAccounts(t *testing.T) {
	for _, delegate := range []string{"", pluginapi.SchedulerBuiltinRoundRobin} {
		t.Run("delegate="+delegate, func(t *testing.T) {
			env := newScopeTestEnv(t)
			plugin := &scopeGreedyPlugin{want: env.claudeB.ID, delegate: delegate}
			env.manager.SetPluginScheduler(plugin)
			for i := 0; i < 10; i++ {
				_, _ = env.manager.Execute(keyContext(scopeTestKeyA), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{})
			}
			assertOnlyUsed(t, env.claude.usedIDs(), env.claudeA1, env.claudeA2)
			plugin.mu.Lock()
			offered := append([]string(nil), plugin.offered...)
			plugin.mu.Unlock()
			for _, id := range offered {
				if id == env.claudeB.ID {
					t.Fatalf("plugin scheduler was offered an account outside the key's scope: %v", offered)
				}
			}
		})
	}
}

func TestClientScope_ConfigReloadChangesScope(t *testing.T) {
	env := newScopeTestEnv(t)
	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{scopeTestOwnerKey}
	cfg.ScopedAPIKeys = []internalconfig.ScopedAPIKey{{APIKey: scopeTestKeyA, Accounts: []string{env.claudeB.FileName}}}
	env.manager.SetConfig(cfg)
	for i := 0; i < 5; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyA), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	assertOnlyUsed(t, env.claude.usedIDs(), env.claudeB)

	// Removing every scoped key turns scoping off entirely.
	env.manager.SetConfig(&internalconfig.Config{})
	if _, err := env.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute without scoping: %v", err)
	}
}

func TestClientAccountScopeForRequest(t *testing.T) {
	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{" owner ", "both"}
	cfg.ScopedAPIKeys = []internalconfig.ScopedAPIKey{
		{APIKey: "scoped", Accounts: []string{" a.json ", ""}},
		{APIKey: "both", Accounts: []string{"b.json"}},
		{APIKey: "empty"},
		{APIKey: "  ", Accounts: []string{"ignored.json"}},
	}
	setClientAccountScopes(cfg)
	t.Cleanup(func() { setClientAccountScopes(nil) })

	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Set("userApiKey", "scoped")
	viaGin := context.WithValue(context.Background(), "gin", ginCtx)

	fileA := &Auth{ID: "id-a", FileName: "a.json"}
	fileB := &Auth{ID: "id-b", FileName: "b.json"}
	for _, tc := range []struct {
		name         string
		ctx          context.Context
		enforced     bool
		allowA, allB bool
	}{
		{"owner key", keyContext("owner"), false, true, true},
		{"scoped key", keyContext("scoped"), true, true, false},
		{"scoped key via gin context", viaGin, true, true, false},
		{"key in both lists is scoped", keyContext("both"), true, false, true},
		{"scoped key with no accounts", keyContext("empty"), true, false, false},
		{"unknown key", keyContext("nope"), true, false, false},
		{"no key", context.Background(), true, false, false},
		{"nil context", nil, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := clientAccountScopeForRequest(tc.ctx)
			if scope.enforced != tc.enforced {
				t.Fatalf("enforced = %v, want %v", scope.enforced, tc.enforced)
			}
			if got := scope.allows(fileA); got != tc.allowA {
				t.Fatalf("allows(a.json) = %v, want %v", got, tc.allowA)
			}
			if got := scope.allows(fileB); got != tc.allB {
				t.Fatalf("allows(b.json) = %v, want %v", got, tc.allB)
			}
			if scope.enforced && scope.allows(nil) {
				t.Fatal("an enforced scope must not allow a nil auth")
			}
		})
	}

	setClientAccountScopes(&internalconfig.Config{})
	if scope := clientAccountScopeForRequest(context.Background()); scope.enforced {
		t.Fatal("scoping must be off when no scoped keys are configured")
	}
}

// scopeFirstSelector is a custom (non-built-in) selector: it takes the first offered auth.
type scopeFirstSelector struct{}

func (scopeFirstSelector) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth"}
	}
	return auths[0], nil
}

// scopeRogueSelector ignores what it is offered and always returns one fixed auth.
type scopeRogueSelector struct{ target **Auth }

func (s scopeRogueSelector) Pick(context.Context, string, string, cliproxyexecutor.Options, []*Auth) (*Auth, error) {
	return *s.target, nil
}

func TestClientScope_LegacySelectionPathStaysInScope(t *testing.T) {
	env := newScopeTestEnvWithSelector(t, scopeFirstSelector{})
	for i := 0; i < 10; i++ {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyB), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("single provider %d: %v", i, err)
		}
		if _, err := env.manager.Execute(keyContext(scopeTestKeyB), []string{"codex", "claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("mixed providers %d: %v", i, err)
		}
	}
	assertOnlyUsed(t, env.claude.usedIDs(), env.claudeB)
	if _, err := env.manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("legacy path served a request without a client key")
	}
}

func TestClientScope_SelectorReturningUnofferedAccountIsRefused(t *testing.T) {
	var target *Auth
	env := newScopeTestEnvWithSelector(t, scopeRogueSelector{target: &target})
	target = env.claudeB.Clone()
	for _, providers := range [][]string{{"claude"}, {"codex", "claude"}} {
		if _, err := env.manager.Execute(keyContext(scopeTestKeyA), providers, cliproxyexecutor.Request{Model: scopeTestClaudeModel}, cliproxyexecutor.Options{}); err == nil {
			t.Fatalf("providers %v: a selector smuggled in an account outside key A's scope", providers)
		}
	}
	if used := env.claude.usedIDs(); len(used) != 0 {
		t.Fatalf("no account may be used, used=%v", used)
	}
}

func TestClientScope_AntigravityCreditsFallbackStaysInScope(t *testing.T) {
	suffix := uuid.NewString()
	agA := &Auth{ID: "ag-a-" + suffix, Provider: "antigravity", FileName: "antigravity-a.json"}
	agB := &Auth{ID: "ag-b-" + suffix, Provider: "antigravity", FileName: "antigravity-b.json"}
	m := &Manager{
		auths:     map[string]*Auth{agA.ID: agA, agB.ID: agB},
		executors: map[string]ProviderExecutor{"antigravity": schedulerTestExecutor{}},
	}
	cfg := &internalconfig.Config{}
	cfg.APIKeys = []string{scopeTestOwnerKey}
	cfg.ScopedAPIKeys = []internalconfig.ScopedAPIKey{{APIKey: scopeTestKeyA, Accounts: []string{agA.FileName}}}
	setClientAccountScopes(cfg)
	t.Cleanup(func() { setClientAccountScopes(nil) })

	ids := func(ctx context.Context) map[string]bool {
		t.Helper()
		candidates, err := m.findAllAntigravityCreditsCandidateAuths(ctx, "claude-sonnet-4-6", cliproxyexecutor.Options{})
		if err != nil {
			t.Fatalf("find credits candidates: %v", err)
		}
		out := map[string]bool{}
		for _, candidate := range candidates {
			out[candidate.auth.ID] = true
		}
		return out
	}
	if got := ids(keyContext(scopeTestKeyA)); len(got) != 1 || !got[agA.ID] {
		t.Fatalf("key A credits candidates = %v, want only %s", got, agA.ID)
	}
	if got := ids(keyContext(scopeTestOwnerKey)); len(got) != 2 {
		t.Fatalf("owner credits candidates = %v, want both", got)
	}
	if got := ids(context.Background()); len(got) != 0 {
		t.Fatalf("credits candidates without a client key = %v, want none", got)
	}
}

func TestClientScope_MatchesAuthIndexBeforeItIsAssigned(t *testing.T) {
	auth := &Auth{ID: "cfg-key", Provider: "claude", Attributes: map[string]string{"api_key": "k", "base_url": "https://example.invalid"}}
	index := stableAuthIndex(auth.indexSeed())
	scope := clientAccountScope{enforced: true, allowed: map[string]struct{}{index: {}}}
	if auth.Index != "" || !scope.allows(auth) {
		t.Fatalf("scope listing auth_index %s must match an auth whose index is not stored yet", index)
	}
	if auth.Index != "" {
		t.Fatal("allows must not mutate the auth")
	}
}
