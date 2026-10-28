package cliproxy

import (
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// quotaHistoryDirName is created next to the config file.
const quotaHistoryDirName = "quota-history"

// startQuotaHistory starts recording passive quota readings next to the config
// file and returns the recorder, or nil when there is no config file or manager.
func (s *Service) startQuotaHistory() *quotahistory.Recorder {
	if s == nil {
		return nil
	}
	if s.quotaHistory != nil {
		return s.quotaHistory
	}
	if strings.TrimSpace(s.configPath) == "" || s.coreManager == nil {
		return nil
	}
	dir := filepath.Join(filepath.Dir(s.configPath), quotaHistoryDirName)
	recorder, err := quotahistory.New(dir)
	if err != nil {
		log.WithError(err).Warn("quota history disabled")
		return nil
	}
	recorder.Start()
	s.coreManager.SetQuotaObservationHook(func(observation coreauth.QuotaObservation) {
		recorder.Observe(observation.AuthID, observation.AuthIndex, observation.Name, observation.Provider, observation.Signals, observation.ObservedAt)
	})
	s.quotaHistory = recorder
	return recorder
}

func (s *Service) stopQuotaHistory() {
	if s == nil || s.quotaHistory == nil {
		return
	}
	if s.coreManager != nil {
		s.coreManager.SetQuotaObservationHook(nil)
	}
	s.quotaHistory.Close()
}
