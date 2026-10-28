package auth

import (
	"strings"
	"time"
)

// QuotaObservation is one passive quota snapshot taken from an upstream response
// (see QuotaState.ObserveResponseHeadersForProvider).
type QuotaObservation struct {
	AuthID    string
	AuthIndex string
	// Name is the backing file name as the management API lists it, or the auth ID.
	Name       string
	Provider   string
	Signals    map[string]string
	ObservedAt time.Time
}

// QuotaObservationHook receives passive quota snapshots.
type QuotaObservationHook func(QuotaObservation)

// SetQuotaObservationHook registers fn to receive every passive quota snapshot
// right after it is stored on an auth. fn runs on the request path after the
// manager lock is released, so it must return quickly. nil removes the hook.
func (m *Manager) SetQuotaObservationHook(fn QuotaObservationHook) {
	if m == nil {
		return
	}
	if fn == nil {
		m.quotaObservationHook.Store(nil)
		return
	}
	m.quotaObservationHook.Store(&fn)
}

func (m *Manager) notifyQuotaObservation(observation *QuotaObservation) {
	if m == nil || observation == nil {
		return
	}
	if fn := m.quotaObservationHook.Load(); fn != nil && *fn != nil {
		(*fn)(*observation)
	}
}

// newQuotaObservation copies the snapshot just stored on auth. Call it with the
// manager lock held; the copy is safe to use after the lock is released.
func newQuotaObservation(auth *Auth, provider string) *QuotaObservation {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return nil
	}
	signals := make(map[string]string, len(auth.Quota.Signals))
	for key, value := range auth.Quota.Signals {
		signals[key] = value
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	if strings.TrimSpace(provider) == "" {
		provider = auth.Provider
	}
	return &QuotaObservation{
		AuthID:     auth.ID,
		AuthIndex:  auth.Index,
		Name:       name,
		Provider:   provider,
		Signals:    signals,
		ObservedAt: auth.Quota.ObservedAt,
	}
}
