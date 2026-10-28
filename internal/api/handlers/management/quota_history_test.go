package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

type quotaHistoryResponse struct {
	Enabled       bool                  `json:"enabled"`
	RetentionDays int                   `json:"retention_days"`
	Samples       []quotahistory.Sample `json:"samples"`
	Error         string                `json:"error"`
}

func requestQuotaHistory(t *testing.T, h *Handler, query string) (int, quotaHistoryResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/quota-history"+query, nil)
	h.GetQuotaHistory(ctx)
	var payload quotaHistoryResponse
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), errDecode)
	}
	return rec.Code, payload
}

func TestGetQuotaHistoryWithoutRecorderReportsDisabled(t *testing.T) {
	code, payload := requestQuotaHistory(t, &Handler{}, "")
	if code != http.StatusOK || payload.Enabled || payload.Samples == nil || len(payload.Samples) != 0 {
		t.Fatalf("code=%d payload=%#v", code, payload)
	}
}

func TestGetQuotaHistoryFiltersByProviderAndRange(t *testing.T) {
	recorder, err := quotahistory.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	recorder.Observe("claude-a", "", "claude-a.json", "claude", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
	}, now.Add(-time.Hour))
	recorder.Observe("codex-a", "", "codex-a.json", "codex", map[string]string{
		"X-Codex-Primary-Used-Percent":   "12",
		"X-Codex-Primary-Window-Minutes": "10080",
	}, now.Add(-time.Hour))
	recorder.Flush()
	h := &Handler{}
	h.SetQuotaHistory(recorder)

	code, payload := requestQuotaHistory(t, h, "?provider=codex")
	if code != http.StatusOK || !payload.Enabled || payload.RetentionDays != 35 || len(payload.Samples) != 1 ||
		payload.Samples[0].Auth != "codex-a" || payload.Samples[0].Windows[0].Used != 12 {
		t.Fatalf("code=%d payload=%#v", code, payload)
	}
	since := strconv.FormatInt(now.Add(-30*time.Minute).Unix(), 10)
	if _, recent := requestQuotaHistory(t, h, "?since="+since); len(recent.Samples) != 0 {
		t.Fatalf("samples after since = %#v, want none", recent.Samples)
	}
	if code, bad := requestQuotaHistory(t, h, "?since=abc"); code != http.StatusBadRequest || bad.Error == "" {
		t.Fatalf("invalid since: code=%d payload=%#v", code, bad)
	}
}
