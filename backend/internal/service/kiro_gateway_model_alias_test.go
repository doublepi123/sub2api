//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Reuse the Kiro harness interface; its canned-response transport cannot reach
// an httptest.Server, so override only the TLS transport seam.
type kiroAliasServerUpstream struct {
	kiroForwardUpstream
	client *http.Client
	target *url.URL
}

func (u *kiroAliasServerUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	local := req.Clone(req.Context())
	local.URL.Scheme = u.target.Scheme
	local.URL.Host = u.target.Host
	return u.client.Do(local)
}

func TestForwardKiro_UsesAliasForUpstream_KeepsMappedModelForCooldown(t *testing.T) {
	// Given
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(w, kiroInvalidModelTestBody); err != nil {
			t.Errorf("write upstream response: %v", err)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := server.Client()
	client.Timeout = 5 * time.Second
	repo := &kiroInvalidModelRepo{}
	svc := &GatewayService{
		accountRepo:      repo,
		httpUpstream:     &kiroAliasServerUpstream{client: client, target: target},
		rateLimitService: &RateLimitService{accountRepo: repo},
	}
	account := &Account{ID: 503, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "valid-access", "refresh_token": "rt-kiro", "region": "us-east-1",
		"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"model_mapping": map[string]any{},
	}, Extra: map[string]any{"kiro_sched_tier": "PAID"}}
	parsed := mustParseKiroSeedRequest(t, `{"model":"claude-opus-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	// When
	result, err := svc.forwardKiro(context.Background(), c, account, parsed)

	// Then: independent assertions expose both missing alias and failover on RED.
	select {
	case body := <-captured:
		assert.Equal(t, "claude-opus-4.5", gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String())
	default:
		t.Fatal("Kiro request did not reach the test server")
	}
	var failoverErr *UpstreamFailoverError
	if assert.True(t, errors.As(err, &failoverErr), "expected account failover, got %v", err) {
		assert.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
		assert.False(t, failoverErr.RetryableOnSameAccount)
	}
	require.Nil(t, result)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "claude-opus-4-5", repo.modelRateLimitCalls[0].scope)
	require.NotEqual(t, "claude-opus-4.5", repo.modelRateLimitCalls[0].scope)
	require.Equal(t, "PAID", account.Extra["kiro_sched_tier"])
	require.Empty(t, repo.extraKeys)
}
