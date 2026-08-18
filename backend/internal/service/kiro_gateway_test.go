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
	requestURL  string
	requestBody map[string]any
	response    string
}

func (u *kiroRefreshUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, nil
}

func (u *kiroRefreshUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.requestURL = req.URL.String()
	_ = json.NewDecoder(req.Body).Decode(&u.requestBody)
	body := u.response
	if body == "" {
		body = `{
          "accessToken":"new-access",
          "refreshToken":"rotated-refresh",
          "expiresIn":3600
        }`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
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
	require.Contains(t, upstream.requestURL, "oidc.us-east-1.amazonaws.com/token")
	require.Equal(t, "refresh_token", upstream.requestBody["grantType"])
	require.Equal(t, "client-id", upstream.requestBody["clientId"])
	require.Equal(t, "client-secret", upstream.requestBody["clientSecret"])
	require.Equal(t, "old-refresh", upstream.requestBody["refreshToken"])
	require.Equal(t, "new-access", repo.saved["access_token"])
	require.Equal(t, "rotated-refresh", repo.saved["refresh_token"])
	require.NotEmpty(t, repo.saved["expires_at"])
}

func TestKiroAccessTokenRefreshesSocialGoogleCredentials(t *testing.T) {
	repo := &kiroRefreshRepo{}
	upstream := &kiroRefreshUpstream{response: `{
          "accessToken":"social-access",
          "refreshToken":"rotated-social",
          "expiresIn":1800,
          "profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/google"
        }`}
	svc := &GatewayService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{ID: 74, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "expired-access", "refresh_token": "social-refresh",
		"auth_method": "social", "expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}}

	token, err := svc.kiroAccessToken(context.Background(), account, false)
	require.NoError(t, err)
	require.Equal(t, "social-access", token)
	require.Equal(t, "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken", upstream.requestURL)
	require.Equal(t, map[string]any{"refreshToken": "social-refresh"}, upstream.requestBody)
	require.NotContains(t, upstream.requestBody, "clientId")
	require.Equal(t, "social-access", repo.saved["access_token"])
	require.Equal(t, "rotated-social", repo.saved["refresh_token"])
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123:profile/google", repo.saved["profile_arn"])
	require.Equal(t, "social", repo.saved["auth_method"])
	require.NotEmpty(t, repo.saved["expires_at"])
}

func TestKiroUsesSocialRefreshInfersMissingClientPair(t *testing.T) {
	require.True(t, kiroUsesSocialRefresh(&Account{Credentials: map[string]any{
		"refresh_token": "rt", "auth_method": "google",
	}}))
	require.True(t, kiroUsesSocialRefresh(&Account{Credentials: map[string]any{
		"refresh_token": "rt",
	}}))
	require.False(t, kiroUsesSocialRefresh(&Account{Credentials: map[string]any{
		"refresh_token": "rt", "client_id": "id", "client_secret": "secret",
	}}))
	require.False(t, kiroUsesSocialRefresh(&Account{Credentials: map[string]any{
		"refresh_token": "rt", "client_id": "id", "client_secret": "secret", "auth_method": "idc",
	}}))
}

func TestKiroSecretsAreSensitiveCredentials(t *testing.T) {
	require.True(t, IsSensitiveCredentialKey("client_id"))
	require.True(t, IsSensitiveCredentialKey("client_secret"))
}

type kiroUsageUpstream struct {
	request *http.Request
}

func (u *kiroUsageUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, nil
}

func (u *kiroUsageUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.request = req.Clone(req.Context())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body: io.NopCloser(strings.NewReader(`{
          "usageBreakdownList":[{
            "resourceType":"CREDIT",
            "currentUsageWithPrecision":22.97,
            "usageLimitWithPrecision":1000.0
          }],
          "nextDateReset":1788220800,
          "subscriptionInfo":{"subscriptionTitle":"KIRO PRO","type":"Q_DEVELOPER_STANDALONE_PRO"}
        }`)),
	}, nil
}

func TestFetchKiroUsageLimitsUsesOfficialReadOnlyEndpoint(t *testing.T) {
	upstream := &kiroUsageUpstream{}
	svc := &GatewayService{httpUpstream: upstream}
	account := &Account{ID: 73, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{
		"access_token": "valid-access",
		"region":       "us-east-1",
		"profile_arn":  "arn:aws:codewhisperer:us-east-1:638616132270:profile/AAAACCCCXXXX",
	}}

	response, err := svc.FetchKiroUsageLimits(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, response.UsageBreakdownList, 1)
	require.NotNil(t, upstream.request)
	require.Equal(t, http.MethodPost, upstream.request.Method)
	require.Equal(t, "codewhisperer.us-east-1.amazonaws.com", upstream.request.URL.Host)
	require.Equal(t, "false", upstream.request.URL.Query().Get("isEmailRequired"))
	require.Empty(t, upstream.request.URL.Query().Get("profileArn"))
	require.Equal(t, "AmazonCodeWhispererService.GetUsageLimits", upstream.request.Header.Get("x-amz-target"))
	require.Equal(t, "Bearer valid-access", upstream.request.Header.Get("Authorization"))
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
