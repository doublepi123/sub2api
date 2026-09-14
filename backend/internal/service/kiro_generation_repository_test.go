//go:build unit

package service

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func (r *upstreamBillingProbeAccountRepo) UpdateWithKiroCredentialGeneration(_ context.Context, account *Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := r.accounts[account.ID]
	if stored == nil {
		return ErrAccountNotFound
	}
	extra := shallowCopyMap(account.Extra)
	if extra == nil {
		extra = make(map[string]any)
	}
	delete(extra, kiroDetectedModelCatalogKey)
	if catalog, ok := stored.Extra[kiroDetectedModelCatalogKey]; ok {
		extra[kiroDetectedModelCatalogKey] = catalog
	}
	extra[kiroCredentialGenerationKey] = stored.kiroCredentialGeneration() + 1
	account.Extra = extra
	r.accounts[account.ID] = account
	return nil
}

func applyKiroPrincipalChangeForTest(t *testing.T, account *Account, previous map[string]any) {
	t.Helper()
	if account == nil {
		require.False(t, bumpKiroCredentialGenerationOnPrincipalChange(account, previous))
		return
	}
	before := maps.Clone(account.Extra)
	bump := bumpKiroCredentialGenerationOnPrincipalChange(account, previous)
	require.Equal(t, before, account.Extra, "decision must not mutate the generation")
	if bump {
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
		require.NoError(t, repo.UpdateWithKiroCredentialGeneration(context.Background(), account))
	}
}
