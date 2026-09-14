//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestRefreshKiroModelCatalog_Failure_PersistsBackoffState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Given
		s, a, repo := catalogFixture(t)
		before := a.Extra[kiroDetectedModelCatalogKey].(map[string]any)
		s.SetKiroModelCatalogFetcher(catalogProbeStub{err: fmt.Errorf("wrapped: %w", &kiro.CatalogHTTPError{StatusCode: 429, RetryAfter: 7200 * time.Second})})
		now := time.Now()
		// When
		_, err := s.RefreshKiroModelCatalog(context.Background(), a, now)
		// Then
		require.Error(t, err)
		after := repo.updates[kiroDetectedModelCatalogKey].(map[string]any)
		require.Equal(t, float64(1), after["consecutive_failures"])
		require.Equal(t, now.Add(2*time.Hour).UTC().Format(time.RFC3339Nano), after["next_attempt_at"])
		for _, key := range []string{"model_ids", "last_success_at"} {
			old, err := json.Marshal(before[key])
			require.NoError(t, err)
			got, err := json.Marshal(after[key])
			require.NoError(t, err)
			require.Equal(t, old, got)
		}
	})
}

func TestRefreshKiroModelCatalog_RetryAfterAboveCap_IsCappedAndLogged(t *testing.T) {
	// Given
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	synctest.Test(t, func(t *testing.T) {
		s, a, repo := catalogFixture(t)
		s.SetKiroModelCatalogFetcher(catalogProbeStub{err: &kiro.CatalogHTTPError{StatusCode: 429, RetryAfter: 99999 * time.Second}})
		now := time.Now()
		// When
		_, err := s.RefreshKiroModelCatalog(context.Background(), a, now)
		// Then
		require.Error(t, err)
		after := repo.updates[kiroDetectedModelCatalogKey].(map[string]any)
		require.Equal(t, now.Add(4*time.Hour).UTC().Format(time.RFC3339Nano), after["next_attempt_at"])
		var event struct {
			Level      string  `json:"level"`
			Message    string  `json:"msg"`
			AccountID  int64   `json:"account_id"`
			RetryAfter float64 `json:"retry_after_s"`
			Cap        float64 `json:"cap_s"`
		}
		require.NoError(t, json.Unmarshal(logs.Bytes(), &event))
		require.Equal(t, "WARN", event.Level)
		require.Equal(t, "kiro_model_catalog_retry_after_capped", event.Message)
		require.Equal(t, a.ID, event.AccountID)
		require.Equal(t, float64(99999), event.RetryAfter)
		require.Equal(t, float64(14400), event.Cap)
	})
}

func TestRefreshKiroModelCatalog_Success_ClearsBackoff(t *testing.T) {
	// Given
	s, a, repo := catalogFixture(t)
	catalog := repo.fresh.Extra[kiroDetectedModelCatalogKey].(map[string]any)
	catalog["consecutive_failures"] = 4
	catalog["next_attempt_at"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	// When
	_, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	after := repo.updates[kiroDetectedModelCatalogKey].(map[string]any)
	require.NotContains(t, after, "consecutive_failures")
	require.NotContains(t, after, "next_attempt_at")
}
