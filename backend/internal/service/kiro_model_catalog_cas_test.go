//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func (r *catalogRepoStub) UpdateKiroModelCatalogIfCurrent(ctx context.Context, _ int64, catalog map[string]any, version, generation int64) (bool, error) {
	r.writeCalls++
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	stored, _ := r.fresh.Extra[kiroDetectedModelCatalogKey].(map[string]any)
	versionAccount := &Account{Extra: stored}
	if int64(versionAccount.getExtraInt("write_version")) != version || r.fresh.kiroCredentialGeneration() != generation {
		return false, nil
	}
	if r.err != nil {
		return false, r.err
	}
	r.updates = map[string]any{kiroDetectedModelCatalogKey: catalog}
	r.fresh.Extra = shallowCopyMap(r.fresh.Extra)
	r.fresh.Extra[kiroDetectedModelCatalogKey] = catalog
	return true, nil
}

func TestRefreshKiroModelCatalog_StalePauseAfterCheck_DoesNotClobberNewerWrite(t *testing.T) {
	// Given: B commits at the repository entry, after A's last lease check.
	s, a, repo := catalogFixture(t)
	a.Extra[kiroDetectedModelCatalogKey].(map[string]any)["write_version"] = float64(7)
	previous, _ := a.kiroModelCatalog()
	before := shallowCopyMap(a.Extra)
	newer := shallowCopyMap(a.Extra[kiroDetectedModelCatalogKey].(map[string]any))
	newer["write_version"] = float64(8)
	newer["model_ids"] = []string{"replica-b"}
	newer["consecutive_failures"] = float64(3)
	newer["next_attempt_at"] = "2099-01-01T00:00:00Z"
	repo.beforeWrite = func() { repo.fresh.Extra[kiroDetectedModelCatalogKey] = newer }
	// When: A resumes with a context whose cancellation has not run.
	got, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then: one attempt, no retry, and neither B's row nor A's memory is overwritten.
	require.NoError(t, err)
	require.Equal(t, newer, repo.fresh.Extra[kiroDetectedModelCatalogKey], "stale A clobbered B after passing the lease check")
	require.Equal(t, previous, got)
	require.Equal(t, before, a.Extra)
	require.Nil(t, repo.updates)
	require.Equal(t, 1, repo.writeCalls)
}

func TestRefreshKiroModelCatalog_CredentialGenerationChangedMidProbe_DiscardsResult(t *testing.T) {
	// Given: replacement commits after the reload and before the conditional write.
	s, a, repo := catalogFixture(t)
	previous, _ := a.kiroModelCatalog()
	before := shallowCopyMap(a.Extra)
	repo.beforeWrite = func() { repo.fresh.Extra[kiroCredentialGenerationKey] = "1" }
	// When
	got, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	require.Equal(t, previous, got)
	require.Equal(t, before, a.Extra)
	require.Nil(t, repo.updates)
	require.Equal(t, 1, repo.writeCalls)
}

func TestRefreshKiroModelCatalog_Success_AdvancesWriteVersion(t *testing.T) {
	// Given
	s, a, repo := catalogFixture(t)
	a.Extra[kiroDetectedModelCatalogKey].(map[string]any)["write_version"] = float64(7)
	// When
	got, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.NoError(t, err)
	require.Equal(t, float64(8), catalogTestMap(t, got)["write_version"])
	require.Equal(t, repo.updates[kiroDetectedModelCatalogKey], a.Extra[kiroDetectedModelCatalogKey])
}

func TestRefreshKiroModelCatalog_Failure_AdvancesWriteVersion(t *testing.T) {
	// Given
	s, a, repo := catalogFixture(t)
	a.Extra[kiroDetectedModelCatalogKey].(map[string]any)["write_version"] = float64(7)
	s.SetKiroModelCatalogFetcher(catalogProbeStub{err: context.DeadlineExceeded})
	// When
	got, err := s.RefreshKiroModelCatalog(context.Background(), a, time.Now())
	// Then
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, float64(8), catalogTestMap(t, got)["write_version"])
	persisted, ok := kiro.ParseModelCatalog(repo.updates[kiroDetectedModelCatalogKey])
	require.True(t, ok)
	require.Equal(t, got, persisted)
	require.Equal(t, 1, persisted.ConsecutiveFailures)
}
