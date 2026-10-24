package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// The client key must survive in the handler context even after gin recycles its context
// for another request, because account scoping reads it from there.
func TestGetContextWithCancelCarriesClientKeyIndependentOfGin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginCtx.Request = req.WithContext(coreusage.WithAPIKey(req.Context(), "key-a"))
	ginCtx.Set("userApiKey", "key-a")

	handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
	ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
	defer cancel()

	// Simulate gin handing this context to the next request.
	ginCtx.Set("userApiKey", "key-b")
	if got := coreusage.APIKeyFromContext(ctx); got != "key-a" {
		t.Fatalf("client key = %q, want key-a", got)
	}
}
