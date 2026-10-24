package auth

import (
	"context"
	"strings"
	"sync/atomic"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Client key account scoping (access.scoped-api-keys).
//
// When at least one scoped key is configured, every account pick is checked against the
// client key that made the request:
//   - a scoped key may only use the accounts listed for it;
//   - a key from access.api-keys may use every account;
//   - any other pick (no client key in the context, or a key in neither list) gets no account.
//
// The check lives in authSelectionEligibility, which builds every candidate list, so
// selectors, plugin schedulers, retries and pinned auths only ever see allowed accounts.
// With no scoped keys configured, selection is unchanged.

type clientAccountScopes struct {
	unrestricted map[string]struct{}
	scoped       map[string]map[string]struct{}
}

// clientAccountScope is the scoping decision for one request.
type clientAccountScope struct {
	// enforced is false when no scoping applies to the request.
	enforced bool
	// allowed holds the account identifiers the request may use. Empty while enforced
	// means no account is allowed.
	allowed map[string]struct{}
}

// activeClientAccountScopes holds the compiled scopes of the latest runtime config.
// It is process-wide because selection helpers run without a Manager reference.
var activeClientAccountScopes atomic.Pointer[clientAccountScopes]

// setClientAccountScopes compiles the scoping table from cfg and makes it active.
func setClientAccountScopes(cfg *internalconfig.Config) {
	activeClientAccountScopes.Store(compileClientAccountScopes(cfg))
}

func compileClientAccountScopes(cfg *internalconfig.Config) *clientAccountScopes {
	if cfg == nil || len(cfg.ScopedAPIKeys) == 0 {
		return nil
	}
	scopes := &clientAccountScopes{
		unrestricted: make(map[string]struct{}, len(cfg.APIKeys)),
		scoped:       make(map[string]map[string]struct{}, len(cfg.ScopedAPIKeys)),
	}
	for _, key := range cfg.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			scopes.unrestricted[key] = struct{}{}
		}
	}
	for _, entry := range cfg.ScopedAPIKeys {
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		allowed := scopes.scoped[key]
		if allowed == nil {
			allowed = make(map[string]struct{}, len(entry.Accounts))
			scopes.scoped[key] = allowed
		}
		for _, account := range entry.Accounts {
			if account = strings.TrimSpace(account); account != "" {
				allowed[account] = struct{}{}
			}
		}
		// A key listed in both places gets the narrower, scoped access.
		delete(scopes.unrestricted, key)
	}
	if len(scopes.scoped) == 0 {
		return nil
	}
	return scopes
}

// clientAccountScopeForRequest resolves the scoping decision for the client key in ctx.
func clientAccountScopeForRequest(ctx context.Context) clientAccountScope {
	scopes := activeClientAccountScopes.Load()
	if scopes == nil {
		return clientAccountScope{}
	}
	key := coreusage.APIKeyFromContext(ctx)
	if key != "" {
		if _, ok := scopes.unrestricted[key]; ok {
			return clientAccountScope{}
		}
		if allowed, ok := scopes.scoped[key]; ok {
			return clientAccountScope{enforced: true, allowed: allowed}
		}
	}
	return clientAccountScope{enforced: true}
}

// allows reports whether the scope permits auth. An account matches by auth ID,
// auth file name, or auth_index.
func (s clientAccountScope) allows(auth *Auth) bool {
	if !s.enforced {
		return true
	}
	if auth == nil || len(s.allowed) == 0 {
		return false
	}
	index := auth.Index
	if strings.TrimSpace(index) == "" {
		// Same value EnsureIndex would assign, computed without mutating the auth.
		index = stableAuthIndex(auth.indexSeed())
	}
	for _, id := range []string{auth.ID, auth.FileName, index} {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, ok := s.allowed[id]; ok {
			return true
		}
	}
	return false
}

// clientScopeViolation re-checks a finished pick against the client key scope. Candidate
// lists are already filtered, so this only fires if a selector returns an account it was
// not offered.
func clientScopeViolation(ctx context.Context, selected *Auth) *Error {
	if clientAccountScopeForRequest(ctx).allows(selected) {
		return nil
	}
	return &Error{Code: "auth_not_found", Message: "selector returned an account outside the client key scope"}
}
