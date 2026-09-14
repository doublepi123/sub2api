//go:build kirolive

package kiro

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListAvailableModels_Live(t *testing.T) {
	for _, tt := range []struct {
		name, env string
		count     int
		paid      bool
	}{
		{"free", "KIRO_LIVE_FREE_TOKEN", 9, false},
		{"paid", "KIRO_LIVE_PAID_TOKEN", 19, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Given
			token := os.Getenv(tt.env)
			if token == "" {
				t.Skip("live credential is not configured")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// When
			ids, err := CollectAvailableModels(ctx, func(ctx context.Context, nextToken string) (body []byte, status int, headers http.Header, err error) {
				req, err := NewListAvailableModelsRequest(ctx, DefaultRegion, token, nextToken)
				if err != nil {
					return nil, 0, nil, errors.New("live request construction failed")
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return nil, 0, nil, errors.New("live request failed")
				}
				defer func() {
					if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
						err = errors.New("live response close failed")
					}
				}()
				body, err = io.ReadAll(resp.Body)
				if err != nil {
					return nil, resp.StatusCode, resp.Header, errors.New("live response read failed")
				}
				return body, resp.StatusCode, resp.Header, nil
			})
			// Then: never render upstream content or credential-bearing errors.
			if err != nil {
				t.Fatal("live catalog collection failed")
			}
			require.Len(t, ids, tt.count)
			require.Contains(t, ids, "claude-haiku-4.5")
			if tt.paid {
				require.Contains(t, ids, "claude-opus-4.5")
			} else {
				require.NotContains(t, ids, "claude-opus-4.5")
			}
		})
	}
}
