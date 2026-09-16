//go:build unit

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

type kiroTokenRefreshUpstream struct {
	requestURL  string
	requestBody map[string]any
	status      int
	response    string
}

func (u *kiroTokenRefreshUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, nil
}

func (u *kiroTokenRefreshUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.requestURL = req.URL.String()
	_ = json.NewDecoder(req.Body).Decode(&u.requestBody)
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	body := u.response
	if body == "" {
		body = `{"accessToken":"new-access","refreshToken":"rotated-refresh","expiresIn":3600}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestKiroTokenRefresher_CanRefresh_when_oauth_has_refresh_token(t *testing.T) {
	refresher := NewKiroTokenRefresher(nil, nil)

	require.True(t, refresher.CanRefresh(&Account{
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt"},
	}))
	require.False(t, refresher.CanRefresh(&Account{
		Platform:    PlatformKiro,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"refresh_token": "rt"},
	}))
	require.False(t, refresher.CanRefresh(&Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "rt"},
	}))
	require.False(t, refresher.CanRefresh(&Account{
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	}))
}

func TestKiroTokenRefresher_NeedsRefresh_when_expires_inside_window(t *testing.T) {
	refresher := NewKiroTokenRefresher(nil, nil)
	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "at",
			"refresh_token": "rt",
			"expires_at":    time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		},
	}

	require.True(t, refresher.NeedsRefresh(account, 15*time.Minute))
	account.Credentials["expires_at"] = time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	require.False(t, refresher.NeedsRefresh(account, 15*time.Minute))
}

func TestKiroTokenRefresher_NeedsRefresh_when_access_token_missing(t *testing.T) {
	refresher := NewKiroTokenRefresher(nil, nil)
	require.True(t, refresher.NeedsRefresh(&Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt",
			"expires_at":    time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339),
		},
	}, time.Minute))
}

func TestKiroTokenRefresher_Refresh_uses_social_endpoint(t *testing.T) {
	upstream := &kiroTokenRefreshUpstream{response: `{
          "accessToken":"social-access",
          "refreshToken":"rotated-social",
          "expiresIn":1800,
          "profileArn":"arn:aws:codewhisperer:us-east-1:123:profile/google"
        }`}
	refresher := NewKiroTokenRefresher(upstream, nil)
	account := &Account{
		ID:       19,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "social-refresh",
			"auth_method":   "social",
			"region":        "us-east-1",
		},
	}

	creds, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken", upstream.requestURL)
	require.Equal(t, map[string]any{"refreshToken": "social-refresh"}, upstream.requestBody)
	require.Equal(t, "social-access", creds["access_token"])
	require.Equal(t, "rotated-social", creds["refresh_token"])
	require.Equal(t, "social", creds["auth_method"])
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:123:profile/google", creds["profile_arn"])
	require.NotEmpty(t, creds["expires_at"])
}

func TestKiroTokenRefresher_Refresh_uses_builder_id_oidc(t *testing.T) {
	upstream := &kiroTokenRefreshUpstream{}
	refresher := NewKiroTokenRefresher(upstream, nil)
	account := &Account{
		ID:       10,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"client_id":     "client-id",
			"client_secret": "client-secret",
			"region":        "us-east-1",
		},
	}

	creds, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Contains(t, upstream.requestURL, "oidc.us-east-1.amazonaws.com/token")
	require.Equal(t, "refresh_token", upstream.requestBody["grantType"])
	require.Equal(t, "client-id", upstream.requestBody["clientId"])
	require.Equal(t, "old-refresh", upstream.requestBody["refreshToken"])
	require.Equal(t, "new-access", creds["access_token"])
	require.Equal(t, "rotated-refresh", creds["refresh_token"])
}

func TestKiroTokenRefresher_Refresh_returns_error_when_upstream_rejects(t *testing.T) {
	upstream := &kiroTokenRefreshUpstream{status: http.StatusUnauthorized, response: `{"message":"Bad credentials"}`}
	refresher := NewKiroTokenRefresher(upstream, nil)
	_, err := refresher.Refresh(context.Background(), &Account{
		Platform:    PlatformKiro,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"refresh_token": "dead", "auth_method": "social"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}
