//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Model the repository's atomic JSONB merge without sharing loaded maps with storage.
type kiroBulkCredentialRepo struct {
	AccountRepository
	accounts map[int64]*Account
	failID   int64
}

func (r *kiroBulkCredentialRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	var accounts []*Account
	for _, id := range ids {
		if stored := r.accounts[id]; stored != nil {
			account := *stored
			account.Credentials = shallowCopyMap(stored.Credentials)
			account.Extra = shallowCopyMap(stored.Extra)
			accounts = append(accounts, &account)
		}
	}
	return accounts, nil
}

func (r *kiroBulkCredentialRepo) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	for _, id := range ids {
		if id == r.failID {
			return 0, errors.New("credential write failed")
		}
	}
	var count int64
	for _, id := range ids {
		if account := r.accounts[id]; account != nil {
			generation := account.kiroCredentialGeneration()
			account.Credentials = mergeMap(account.Credentials, updates.Credentials)
			account.Extra = mergeMap(account.Extra, updates.Extra)
			if updates.BumpKiroCredentialGeneration && account.Platform == PlatformKiro && len(updates.Credentials) > 0 {
				account.Extra[kiroCredentialGenerationKey] = generation + 1
			}
			if updates.Schedulable != nil {
				account.Schedulable = *updates.Schedulable
			}
			count++
		}
	}
	return count, nil
}

func TestBulkUpdateAccounts_KiroCredentialReplacement_InvalidatesCatalog(t *testing.T) {
	// Given: two paid catalogs bound to different generations.
	first, second := kiroPrincipalReplacementAccount(), kiroPrincipalReplacementAccount()
	second.ID++
	second.Extra[kiroCredentialGenerationKey] = int64(7)
	second.Extra[kiroDetectedModelCatalogKey].(map[string]any)["scope_fingerprint"] = second.kiroCatalogScopeFingerprint()
	repo := &kiroBulkCredentialRepo{accounts: map[int64]*Account{first.ID: first, second.ID: second}}
	gateway := &GatewayService{}
	for _, account := range repo.accounts {
		catalog, ok := account.kiroModelCatalog()
		require.True(t, ok)
		require.Len(t, catalog.ModelIDs, 19)
		decision, allowed := gateway.kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
		require.Equal(t, kiroCatalogAllowed, decision)
		require.True(t, allowed)
	}

	// When: only the principal's tokens change via the real bulk admin seam.
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{first.ID, second.ID},
		Credentials: map[string]any{"refresh_token": "FREE-REFRESH", "access_token": "FREE-ACCESS"},
	})

	// Then: neither retained paid catalog can authorize Opus.
	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	for _, account := range repo.accounts {
		catalog, ok := account.kiroModelCatalog()
		require.True(t, ok)
		decision, allowed := gateway.kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
		t.Logf("account=%d fingerprint_unchanged=%t decision=%s opus_allowed=%t", account.ID, catalog.ScopeFingerprint == account.kiroCatalogScopeFingerprint(), decision, allowed)
		require.NotEqual(t, catalog.ScopeFingerprint, account.kiroCatalogScopeFingerprint())
		require.Equal(t, kiroCatalogUnknown, decision)
		require.False(t, allowed)
	}
	require.Equal(t, int64(1), first.kiroCredentialGeneration())
	require.Equal(t, int64(8), second.kiroCredentialGeneration())
}

func TestBulkUpdateAccounts_NoCredentials_DoesNotBump(t *testing.T) {
	// Given
	account := kiroPrincipalReplacementAccount()
	account.Extra[kiroCredentialGenerationKey] = int64(7)
	fingerprint := account.kiroCatalogScopeFingerprint()
	repo := &kiroBulkCredentialRepo{accounts: map[int64]*Account{account.ID: account}}
	schedulable := false

	// When
	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{account.ID}, Schedulable: &schedulable})

	// Then
	require.NoError(t, err)
	require.Equal(t, int64(7), account.kiroCredentialGeneration())
	require.Equal(t, fingerprint, account.kiroCatalogScopeFingerprint())
	require.False(t, account.Schedulable)
}

func TestBulkUpdateAccounts_NonKiroPlatform_Unaffected(t *testing.T) {
	// Given: a mixed platform selection.
	kiroAccount := kiroPrincipalReplacementAccount()
	other := &Account{ID: 99, Platform: PlatformOpenAI, Credentials: map[string]any{"refresh_token": "old"}, Extra: map[string]any{"custom": "preserved"}}
	repo := &kiroBulkCredentialRepo{accounts: map[int64]*Account{kiroAccount.ID: kiroAccount, other.ID: other}}

	// When
	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{kiroAccount.ID, other.ID}, Credentials: map[string]any{"refresh_token": "new"}})

	// Then
	require.NoError(t, err)
	require.Equal(t, "new", other.GetCredential("refresh_token"))
	require.Equal(t, map[string]any{"custom": "preserved"}, other.Extra)
	require.Equal(t, int64(1), kiroAccount.kiroCredentialGeneration())
}

func TestBulkUpdateAccounts_KiroCredentialWriteFailure_KeepsCredentialsBound(t *testing.T) {
	// Given: the second write fails; every successful write must remain safe.
	first, second := kiroPrincipalReplacementAccount(), kiroPrincipalReplacementAccount()
	second.ID++
	repo := &kiroBulkCredentialRepo{accounts: map[int64]*Account{first.ID: first, second.ID: second}, failID: second.ID}

	// When
	_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{first.ID, second.ID}, Credentials: map[string]any{"access_token": "FREE-ACCESS"}})

	// Then
	require.Error(t, err)
	for _, account := range repo.accounts {
		if account.GetCredential("access_token") == "FREE-ACCESS" {
			decision, allowed := (&GatewayService{}).kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
			require.Equal(t, kiroCatalogUnknown, decision)
			require.False(t, allowed)
		}
	}
	require.Equal(t, "PAID-ACCESS", second.GetCredential("access_token"))
}

func TestBulkUpdateAccounts_KiroPartialCredentialWrites(t *testing.T) {
	for _, tc := range []struct {
		name        string
		credentials map[string]any
		firstWrite  bool
		generation  int64
	}{
		{name: "access_only", credentials: map[string]any{"access_token": "FREE-ACCESS"}, generation: 1},
		{name: "first_write_with_catalog", credentials: map[string]any{"access_token": "FREE-ACCESS"}, firstWrite: true, generation: 1},
		{name: "same_token", credentials: map[string]any{"refresh_token": "PAID-REFRESH"}},
		{name: "unrelated_credential", credentials: map[string]any{"expires_at": "later"}},
		{name: "empty_patch", credentials: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given
			account := kiroPrincipalReplacementAccount()
			if tc.firstWrite {
				account.Credentials = nil
			}
			catalog := account.Extra[kiroDetectedModelCatalogKey]
			repo := &kiroBulkCredentialRepo{accounts: map[int64]*Account{account.ID: account}}

			// When
			_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{account.ID}, Credentials: tc.credentials, Extra: map[string]any{"custom": "value"}})

			// Then
			require.NoError(t, err)
			require.Equal(t, tc.generation, account.kiroCredentialGeneration())
			require.Equal(t, catalog, account.Extra[kiroDetectedModelCatalogKey])
			require.Equal(t, "value", account.Extra["custom"])
		})
	}
}
