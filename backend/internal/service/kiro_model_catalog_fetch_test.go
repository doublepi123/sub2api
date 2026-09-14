//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type catalogHTTPStub struct {
	HTTPUpstream
	server      *httptest.Server
	proxy       string
	accountID   int64
	concurrency int
	profile     *tlsfingerprint.Profile
	requests    []*http.Request
}

func (u *catalogHTTPStub) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.proxy, u.accountID, u.concurrency, u.profile = proxy, id, concurrency, profile
	u.requests = append(u.requests, req.Clone(req.Context()))
	forward := req.Clone(req.Context())
	target, err := url.Parse(u.server.URL)
	if err != nil {
		return nil, err
	}
	forward.URL.Scheme = target.Scheme
	forward.URL.Host = target.Host
	return u.server.Client().Do(forward)
}
func catalogHTTPFixture(t *testing.T, handler http.HandlerFunc) (*GatewayService, *Account, *catalogHTTPStub) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u := &catalogHTTPStub{server: server}
	a := &Account{ID: 92, Platform: PlatformKiro, Type: AccountTypeOAuth, Concurrency: 3, Credentials: map[string]any{"access_token": "old-access", "region": "us-east-1", "refresh_token": "refresh", "client_id": "client", "client_secret": "secret", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	return &GatewayService{httpUpstream: u, accountRepo: &kiroRefreshRepo{}}, a, u
}
func testCatalogShape(t *testing.T, count int) {
	t.Helper()
	// Given: synthetic IDs with the measured response envelope and cardinality.
	models := make([]kiro.AvailableModel, count)
	want := make([]string, count)
	for i := range models {
		want[i] = fmt.Sprintf("model-%02d", i)
		models[i].ModelID = want[i]
	}
	s, a, u := catalogHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"defaultModel": map[string]string{"modelId": "auto"}, "models": models, "nextToken": nil})
	})
	// When
	ids, err := s.FetchKiroAvailableModels(context.Background(), a)
	// Then
	require.NoError(t, err)
	require.Equal(t, want, ids)
	require.Len(t, u.requests, 1)
	require.Equal(t, "q.us-east-1.amazonaws.com", u.requests[0].URL.Host)
	require.Equal(t, "AI_EDITOR", u.requests[0].URL.Query().Get("origin"))
}
func TestFetchKiroAvailableModels_RealFreeShape_Returns9(t *testing.T)  { testCatalogShape(t, 9) }
func TestFetchKiroAvailableModels_RealPaidShape_Returns19(t *testing.T) { testCatalogShape(t, 19) }

func TestFetchKiroAvailableModels_401_RefreshesOnceAndRestartsPagination(t *testing.T) {
	for _, second401 := range []bool{false, true} {
		t.Run(fmt.Sprint(second401), func(t *testing.T) {
			// Given
			s, a, u := catalogHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					fmt.Fprint(w, `{"accessToken":"new-access","refreshToken":"rotated","expiresIn":3600}`)
					return
				}
				if r.Header.Get("Authorization") == "Bearer old-access" {
					if r.URL.Query().Get("nextToken") == "" {
						fmt.Fprint(w, `{"models":[{"modelId":"discard"}],"nextToken":"old-cursor"}`)
					} else {
						w.WriteHeader(401)
					}
					return
				}
				if second401 {
					w.WriteHeader(401)
					return
				}
				fmt.Fprint(w, `{"models":[{"modelId":"fresh"}],"nextToken":null}`)
			})
			// When
			ids, err := s.FetchKiroAvailableModels(context.Background(), a)
			// Then
			if second401 {
				var httpErr *kiro.CatalogHTTPError
				require.ErrorAs(t, err, &httpErr)
				require.Equal(t, 401, httpErr.StatusCode)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"fresh"}, ids)
			}
			require.Len(t, u.requests, 4)
			require.Equal(t, "/token", u.requests[2].URL.Path)
			require.Empty(t, u.requests[3].URL.Query().Get("nextToken"))
			require.Equal(t, "Bearer new-access", u.requests[3].Header.Get("Authorization"))
		})
	}
}

func TestFetchKiroAvailableModels_TwoPages_Union(t *testing.T) {
	// Given
	s, a, _ := catalogHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("nextToken") == "" {
			fmt.Fprint(w, `{"models":[{"modelId":"B"}],"nextToken":"page2"}`)
			return
		}
		fmt.Fprint(w, `{"models":[{"modelId":"a"},{"modelId":"b"}]}`)
	})
	// When
	ids, err := s.FetchKiroAvailableModels(context.Background(), a)
	// Then
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, ids)
}
func TestFetchKiroAvailableModels_429_ReturnsRetryAfter(t *testing.T) {
	// Given
	s, a, _ := catalogHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "37"); w.WriteHeader(429) })
	// When
	_, err := s.FetchKiroAvailableModels(context.Background(), a)
	// Then
	var httpErr *kiro.CatalogHTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, 429, httpErr.StatusCode)
	require.Equal(t, 37*time.Second, httpErr.RetryAfter)
}
func TestFetchKiroAvailableModels_UsesProxyAndTLSProfile(t *testing.T) {
	// Given
	s, a, u := catalogHTTPFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"models":[]}`) })
	id := int64(7)
	a.ProxyID = &id
	a.Proxy = &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 8888}
	// When
	_, err := s.FetchKiroAvailableModels(context.Background(), a)
	// Then
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8888", u.proxy)
	require.Equal(t, a.ID, u.accountID)
	require.Equal(t, a.Concurrency, u.concurrency)
	require.Equal(t, s.kiroTLSProfile(a), u.profile)
}
