package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type kiroRefreshRepo struct {
	AccountRepository
	saved map[string]any
}

func (r *kiroRefreshRepo) UpdateCredentials(_ context.Context, _ int64, credentials map[string]any) error {
	r.saved = shallowCopyMap(credentials)
	return nil
}

type kiroRefreshUpstream struct {
	requestBody map[string]any
}

func (u *kiroRefreshUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, nil
}

func (u *kiroRefreshUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	_ = json.NewDecoder(req.Body).Decode(&u.requestBody)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body: io.NopCloser(strings.NewReader(`{
          "accessToken":"new-access",
          "refreshToken":"rotated-refresh",
          "expiresIn":3600
        }`)),
	}, nil
}

func TestKiroAccessTokenRefreshesAndPersistsRotatedCredentials(t *testing.T) {
	repo := &kiroRefreshRepo{}
	upstream := &kiroRefreshUpstream{}
	svc := &GatewayService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{ID: 71, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "expired-access", "refresh_token": "old-refresh",
		"client_id": "client-id", "client_secret": "client-secret",
		"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}}

	token, err := svc.kiroAccessToken(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, "refresh_token", upstream.requestBody["grantType"])
	require.Equal(t, "client-id", upstream.requestBody["clientId"])
	require.Equal(t, "client-secret", upstream.requestBody["clientSecret"])
	require.Equal(t, "old-refresh", upstream.requestBody["refreshToken"])
	require.Equal(t, "new-access", repo.saved["access_token"])
	require.Equal(t, "rotated-refresh", repo.saved["refresh_token"])
	require.NotEmpty(t, repo.saved["expires_at"])
}

func TestKiroSecretsAreSensitiveCredentials(t *testing.T) {
	require.True(t, IsSensitiveCredentialKey("client_id"))
	require.True(t, IsSensitiveCredentialKey("client_secret"))
}

type kiroAccountTestRepo struct {
	AccountRepository
	account *Account
}

func (r *kiroAccountTestRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.account, nil
}

type kiroAccountTestGatewayStub struct {
	body   []byte
	model  string
	stream bool
}

func (g *kiroAccountTestGatewayStub) forwardKiroAnthropicResponse(_ context.Context, _ *Account, body []byte, model string, stream bool) (*http.Response, error) {
	g.body = append([]byte(nil), body...)
	g.model = model
	g.stream = stream
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"KIRO_TEST_OK\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
		)),
	}, nil
}

func TestAccountTestServiceRoutesKiroThroughKiroGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{ID: 72, Platform: PlatformKiro, Type: AccountTypeOAuth}
	gateway := &kiroAccountTestGatewayStub{}
	svc := &AccountTestService{
		accountRepo: &kiroAccountTestRepo{account: account},
		kiroGateway: gateway,
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/72/test", nil)

	err := svc.TestAccountConnection(c, account.ID, "claude-sonnet-4.5", "say test ok", AccountTestModeDefault)
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4.5", gateway.model)
	require.True(t, gateway.stream)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(gateway.body, &payload))
	require.Equal(t, "claude-sonnet-4.5", payload["model"])
	messages, ok := payload["messages"].([]any)
	require.True(t, ok)
	message, ok := messages[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "say test ok", message["content"])
	require.Contains(t, recorder.Body.String(), "KIRO_TEST_OK")
	require.Contains(t, recorder.Body.String(), `"success":true`)
}
