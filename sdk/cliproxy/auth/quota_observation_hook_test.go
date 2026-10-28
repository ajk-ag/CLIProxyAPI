package auth

import (
	"context"
	"net/http"
	"testing"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestMarkResultSendsQuotaObservationToHook(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "hook-auth",
		Provider: "codex",
		FileName: "codex-a.json",
	})
	if errRegister != nil || auth == nil {
		t.Fatalf("Register() auth=%#v err=%v", auth, errRegister)
	}
	var got []QuotaObservation
	manager.SetQuotaObservationHook(func(observation QuotaObservation) {
		got = append(got, observation)
	})

	mark := func(headers http.Header) {
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, headers)
		manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Model: "gpt-5", Success: true})
	}
	mark(http.Header{"X-Codex-Primary-Used-Percent": []string{"7"}})
	mark(http.Header{"Content-Type": []string{"application/json"}})

	if len(got) != 1 {
		t.Fatalf("observations = %#v, want exactly one (the response without quota headers is skipped)", got)
	}
	if got[0].AuthID != "hook-auth" || got[0].Name != "codex-a.json" || got[0].Provider != "codex" ||
		got[0].Signals["X-Codex-Primary-Used-Percent"] != "7" || got[0].ObservedAt.IsZero() {
		t.Fatalf("observation = %#v", got[0])
	}
	got[0].Signals["X-Codex-Primary-Used-Percent"] = "changed"
	if stored, _ := manager.GetByID(auth.ID); stored.Quota.Signals["X-Codex-Primary-Used-Percent"] != "7" {
		t.Fatal("hook received the auth's own signals map instead of a copy")
	}

	manager.SetQuotaObservationHook(nil)
	mark(http.Header{"X-Codex-Primary-Used-Percent": []string{"8"}})
	if len(got) != 1 {
		t.Fatalf("observations after removing the hook = %d, want 1", len(got))
	}
}
