package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
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
