//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The hook suspends B at its write boundary, after it has loaded its snapshot.
// Nested calls execute A and C explicitly, without goroutines or timing assumptions.
type kiroInterleavingRepo struct {
	*kiroBulkCredentialRepo
	beforeBulk func(AccountBulkUpdate) (int64, error)
}

func (r *kiroInterleavingRepo) BulkUpdate(ctx context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	if hook := r.beforeBulk; hook != nil {
		r.beforeBulk = nil
		return hook(updates)
	}
	return r.kiroBulkCredentialRepo.BulkUpdate(ctx, ids, updates)
}

func TestBulkUpdateAccounts_ConcurrentPrincipalWrites_DoNotReuseGeneration(t *testing.T) {
	// Given: B is suspended after reading generation 7; A reads the same row.
	ctx := context.Background()
	a := kiroPrincipalReplacementAccount()
	a.Extra[kiroCredentialGenerationKey] = int64(7)
	repo := &kiroInterleavingRepo{kiroBulkCredentialRepo: &kiroBulkCredentialRepo{accounts: map[int64]*Account{a.ID: a}}}
	admin := &adminServiceImpl{accountRepo: repo}
	repo.beforeBulk = func(b AccountBulkUpdate) (int64, error) {
		_, err := admin.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{a.ID}, Credentials: map[string]any{"access_token": "PRINCIPAL-P"}})
		require.NoError(t, err)
		require.Equal(t, int64(8), a.kiroCredentialGeneration())
		return repo.kiroBulkCredentialRepo.BulkUpdate(ctx, []int64{a.ID}, b)
	}
	// When: A commits, then B resumes with its already prepared write.
	_, err := admin.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{a.ID}, Credentials: map[string]any{"access_token": "PRINCIPAL-F"}})
	// Then: each successful principal write consumed a distinct generation.
	require.NoError(t, err)
	require.Equal(t, "PRINCIPAL-F", a.GetCredential("access_token"))
	require.Equal(t, int64(9), a.kiroCredentialGeneration(), "two writes from generation 7 must end at 9")
}

func TestRefreshKiroModelCatalog_PaidCatalogNotBoundToFreePrincipalAfterConcurrentSwap(t *testing.T) {
	testKiroConcurrentCatalogSwap(t, true)
}

func TestRefreshKiroModelCatalog_CommittedPaidCatalogInvalidAfterConcurrentSwap(t *testing.T) {
	testKiroConcurrentCatalogSwap(t, false)
}

func testKiroConcurrentCatalogSwap(t *testing.T, pauseBeforeCAS bool) {
	t.Helper()
	// Given: B has read generation 7 before A installs paid principal P.
	ctx := context.Background()
	a := kiroPrincipalReplacementAccount()
	a.Extra[kiroCredentialGenerationKey] = int64(7)
	a.Extra[kiroDetectedModelCatalogKey].(map[string]any)["write_version"] = float64(4)
	repo := &kiroInterleavingRepo{kiroBulkCredentialRepo: &kiroBulkCredentialRepo{accounts: map[int64]*Account{a.ID: a}}}
	admin := &adminServiceImpl{accountRepo: repo}
	var probeRepo *catalogRepoStub
	repo.beforeBulk = func(b AccountBulkUpdate) (int64, error) {
		_, err := admin.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{a.ID}, Credentials: map[string]any{"access_token": "PRINCIPAL-P"}})
		require.NoError(t, err)
		require.Equal(t, int64(8), a.kiroCredentialGeneration())
		loaded, err := repo.GetByIDs(ctx, []int64{a.ID})
		require.NoError(t, err)
		probeRepo = &catalogRepoStub{fresh: a}
		probe := &AccountUsageService{accountRepo: probeRepo}
		probe.SetKiroModelCatalogFetcher(catalogProbeStub{ids: kiroPaidCatalogIDs})
		commitB := func() {
			_, writeErr := repo.kiroBulkCredentialRepo.BulkUpdate(ctx, []int64{a.ID}, b)
			require.NoError(t, writeErr)
		}
		if pauseBeforeCAS {
			probeRepo.beforeWrite = commitB
		}
		_, err = probe.RefreshKiroModelCatalog(ctx, loaded[0], time.Now())
		require.NoError(t, err)
		if !pauseBeforeCAS {
			require.NotNil(t, probeRepo.updates, "C must commit before B")
			commitB()
		}
		return 1, nil
	}
	// When: C either pauses immediately before CAS, or commits before B resumes.
	_, err := admin.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{a.ID}, Credentials: map[string]any{"access_token": "PRINCIPAL-F"}})
	// Then: F cannot use P's paid catalog in either interleaving.
	require.NoError(t, err)
	require.Equal(t, 1, probeRepo.writeCalls)
	decision, allowed := (&GatewayService{}).kiroCatalogDecide(ctx, a, "claude-opus-4-5")
	t.Logf("generation=%d decision=%s opus_allowed=%t cas_applied=%t", a.kiroCredentialGeneration(), decision, allowed, probeRepo.updates != nil)
	if pauseBeforeCAS {
		require.Nil(t, probeRepo.updates, "C's CAS must reject P's catalog after B installed F")
	}
	require.Equal(t, kiroCatalogUnknown, decision)
	require.False(t, allowed)
	catalog, ok := a.kiroModelCatalog()
	require.True(t, ok)
	require.NotEqual(t, catalog.ScopeFingerprint, a.kiroCatalogScopeFingerprint())
	require.Equal(t, int64(9), a.kiroCredentialGeneration())
}
