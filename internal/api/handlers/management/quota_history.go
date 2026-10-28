package management

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

const (
	quotaHistoryDefaultRange = 14 * 24 * time.Hour
	quotaHistoryMaxRange     = 40 * 24 * time.Hour
)

// SetQuotaHistory sets the recorder that GetQuotaHistory reads; nil disables it.
func (h *Handler) SetQuotaHistory(recorder *quotahistory.Recorder) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.quotaHistory = recorder
	h.mu.Unlock()
}

// GetQuotaHistory returns recorded passive quota readings.
//
// Query: since and until are Unix seconds (default: the last 14 days, at most
// 40 days); provider limits the result to one provider (e.g. claude, codex).
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	h.mu.Lock()
	recorder := h.quotaHistory
	h.mu.Unlock()
	if recorder == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "samples": []quotahistory.Sample{}})
		return
	}
	now := time.Now()
	until := now
	if raw := strings.TrimSpace(c.Query("until")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid until"})
			return
		}
		until = time.Unix(parsed, 0)
	}
	since := until.Add(-quotaHistoryDefaultRange)
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since"})
			return
		}
		since = time.Unix(parsed, 0)
	}
	if until.Before(since) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since is after until"})
		return
	}
	if until.Sub(since) > quotaHistoryMaxRange {
		since = until.Add(-quotaHistoryMaxRange)
	}
	samples, err := recorder.Read(since, until, c.Query("provider"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read quota history"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":        true,
		"retention_days": recorder.RetentionDays(),
		"since":          since.Unix(),
		"until":          until.Unix(),
		"samples":        samples,
	})
}
