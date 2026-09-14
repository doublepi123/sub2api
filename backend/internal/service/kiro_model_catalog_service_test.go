//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type catalogProbeStub struct {
	ids []string
	err error
}

func (f catalogProbeStub) FetchKiroAvailableModels(context.Context, *Account) ([]string, error) {
	return f.ids, f.err
}

type catalogRepoStub struct {
	AccountRepository
	fresh   *Account
	updates map[string]any
	err     error
	readErr error
}

func (r *catalogRepoStub) GetByID(ctx context.Context, _ int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.fresh, r.readErr
}
func (r *catalogRepoStub) UpdateExtra(ctx context.Context, _ int64, updates map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.updates = updates
	return r.err
}

func catalogTestMap(t *testing.T, c kiro.ModelCatalog) map[string]any {
	t.Helper()
	b, err := json.Marshal(c)
	require.NoError(t, err)
	var v map[string]any
	require.NoError(t, json.Unmarshal(b, &v))
	return v
}
func catalogFixture(t *testing.T) (*AccountUsageService, *Account, *catalogRepoStub) {
	t.Helper()
	ids := make([]string, 19)
	for i := range ids {
		ids[i] = fmt.Sprintf("model-%02d", i)
	}
	c := kiro.ModelCatalog{SchemaVersion: kiro.CatalogSchemaVersion, Source: kiro.CatalogSource, State: kiro.CatalogStateReady, ModelIDs: ids, ScopeFingerprint: kiro.ScopeFingerprint(kiro.ScopeInputs{Region: "us-east-1"}), LastSuccessAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
	a := &Account{ID: 91, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: map[string]any{"region": "us-east-1"}, Extra: map[string]any{"detected_model_catalog": catalogTestMap(t, c), "model_rate_limits": map[string]any{"model-01": "keep"}}}
	fresh := *a
	fresh.Extra = shallowCopyMap(a.Extra)
	fresh.Credentials = shallowCopyMap(a.Credentials)
	r := &catalogRepoStub{fresh: &fresh}
	s := &AccountUsageService{accountRepo: r}
	s.SetKiroModelCatalogFetcher(catalogProbeStub{ids: []string{}})
	return s, a, r
}

func TestRefreshKiroModelCatalog_EmptyList_Overwrites(t *testing.T) {
	// Given
	s, a, r := catalogFixture(t)
	before, _ := kiro.ParseModelCatalog(a.Extra["detected_model_catalog"])
	// When
	c, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	require.Empty(t, c.ModelIDs)
	require.NotNil(t, c.ModelIDs)
	require.Equal(t, kiro.CatalogStateReady, c.State)
	require.NotEqual(t, before.LastSuccessAt, c.LastSuccessAt)
	persisted, ok := kiro.ParseModelCatalog(r.updates["detected_model_catalog"])
	require.True(t, ok)
	require.Equal(t, c, persisted)
}

func TestRefreshKiroModelCatalog_Failure_PreservesPreviousList(t *testing.T) {
	cases := map[string]error{"http_401": &kiro.CatalogHTTPError{StatusCode: 401}, "http_429": &kiro.CatalogHTTPError{StatusCode: 429}, "http_5xx": &kiro.CatalogHTTPError{StatusCode: 503}, "timeout": context.DeadlineExceeded, "network": errors.New("offline")}
	for _, code := range []string{"decode", "missing_models_field", "pagination_incomplete", "pagination_limit"} {
		cases[code] = &kiro.CatalogFetchError{Code: code}
	}
	for code, probeErr := range cases {
		t.Run(code, func(t *testing.T) {
			// Given
			s, a, r := catalogFixture(t)
			before := a.Extra["detected_model_catalog"].(map[string]any)
			s.SetKiroModelCatalogFetcher(catalogProbeStub{ids: []string{"partial"}, err: probeErr})
			// When
			c, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
			// Then
			require.ErrorIs(t, err, probeErr)
			require.Equal(t, code, c.LastErrorCode)
			require.NotEmpty(t, c.LastAttemptAt)
			after := r.updates["detected_model_catalog"].(map[string]any)
			for _, key := range []string{"model_ids", "last_success_at"} {
				b, _ := json.Marshal(before[key])
				v, _ := json.Marshal(after[key])
				require.Equal(t, b, v)
			}
			require.Equal(t, kiro.CatalogStateReady, c.State)
		})
	}
}

func TestRefreshKiroModelCatalog_Failure_MarksExpiredWhenOldSuccess(t *testing.T) {
	// Given
	s, a, _ := catalogFixture(t)
	a.Extra["detected_model_catalog"].(map[string]any)["last_success_at"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	s.SetKiroModelCatalogFetcher(catalogProbeStub{err: context.DeadlineExceeded})
	// When
	c, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.Error(t, err)
	require.Equal(t, kiro.CatalogStateExpired, c.State)
	require.Len(t, c.ModelIDs, 19)
}

func TestRefreshKiroModelCatalog_StaleWrite_Rejected(t *testing.T) {
	// Given
	s, a, r := catalogFixture(t)
	start := time.Now().Add(-time.Minute)
	c, _ := kiro.ParseModelCatalog(r.fresh.Extra["detected_model_catalog"])
	c.LastSuccessAt = time.Now().UTC().Format(time.RFC3339Nano)
	r.fresh.Extra["detected_model_catalog"] = catalogTestMap(t, c)
	// When
	got, err := s.RefreshKiroModelCatalog(context.Background(), a, start)
	// Then
	require.NoError(t, err)
	require.Nil(t, r.updates)
	require.Equal(t, c, got)
}

func TestRefreshKiroModelCatalog_FingerprintChange_ReplacesCatalog(t *testing.T) {
	// Given: credentials already changed before this probe; catalog still has old scope.
	s, a, r := catalogFixture(t)
	a.Credentials["region"] = "eu-west-1"
	r.fresh.Credentials["region"] = "eu-west-1"
	s.SetKiroModelCatalogFetcher(catalogProbeStub{ids: []string{"new-model"}})
	// When
	c, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	require.Equal(t, []string{"new-model"}, c.ModelIDs)
	require.Equal(t, a.kiroCatalogScopeFingerprint(), c.ScopeFingerprint)
	require.NotNil(t, r.updates)
}

func TestRefreshKiroModelCatalog_DoesNotTouchModelRateLimits(t *testing.T) {
	// Given
	s, a, r := catalogFixture(t)
	limits := a.Extra["model_rate_limits"]
	// When
	_, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	require.Len(t, r.updates, 1)
	require.Contains(t, r.updates, "detected_model_catalog")
	require.Equal(t, limits, a.Extra["model_rate_limits"])
}

func TestRefreshKiroModelCatalog_PersistenceFailureIsReported(t *testing.T) {
	// Given
	s, a, r := catalogFixture(t)
	r.err = errors.New("db down")
	before := shallowCopyMap(a.Extra)
	// When
	_, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.ErrorIs(t, err, r.err)
	require.Equal(t, before, a.Extra)
}

func TestRefreshKiroModelCatalog_ScopeChangedDuringProbe_Skips(t *testing.T) {
	for _, credentialChange := range []bool{true, false} {
		t.Run(fmt.Sprint(credentialChange), func(t *testing.T) {
			// Given
			s, a, r := catalogFixture(t)
			if credentialChange {
				r.fresh.Credentials["region"] = "eu-west-1"
			} else {
				c, _ := kiro.ParseModelCatalog(r.fresh.Extra["detected_model_catalog"])
				c.ScopeFingerprint = "changed"
				r.fresh.Extra["detected_model_catalog"] = catalogTestMap(t, c)
			}
			// When
			_, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
			// Then
			require.NoError(t, err)
			require.Nil(t, r.updates)
		})
	}
}

func TestRefreshKiroModelCatalog_TimedOutContext_PersistsFailure(t *testing.T) {
	// Given
	s, a, r := catalogFixture(t)
	s.SetKiroModelCatalogFetcher(catalogProbeStub{err: context.DeadlineExceeded})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	// When
	c, err := s.RefreshKiroModelCatalog(ctx, a, time.Now())
	// Then
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, "timeout", c.LastErrorCode)
	require.NotNil(t, r.updates)
}
