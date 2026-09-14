//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func kiroPrincipalReplacementAccount() *Account {
	account := kiroCatalogAccount(761, kiroPaidCatalogIDs, time.Now(), "enforce")
	account.Credentials = map[string]any{
		"auth_method": "social", "region": "us-east-1",
		"refresh_token": "PAID-REFRESH", "access_token": "PAID-ACCESS",
	}
	catalog := account.Extra[kiroDetectedModelCatalogKey].(map[string]any)
	catalog["scope_fingerprint"] = account.kiroCatalogScopeFingerprint()
	return &account
}

func TestBumpKiroCredentialGeneration_SameRefreshToken_NoBump(t *testing.T) {
	// Given: only surrounding whitespace changes in the refresh token.
	account := kiroPrincipalReplacementAccount()
	account.Extra[kiroCredentialGenerationKey] = int64(7)
	previous := shallowCopyMap(account.Credentials)
	previous["refresh_token"] = "  PAID-REFRESH\n"

	// When
	applyKiroPrincipalChangeForTest(t, account, previous)

	// Then
	require.Equal(t, int64(7), account.kiroCredentialGeneration())
}

func TestBumpKiroCredentialGeneration_NonKiroPlatformIgnored(t *testing.T) {
	// Given
	account := &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"refresh_token": "new"}}

	// When
	applyKiroPrincipalChangeForTest(t, account, map[string]any{"refresh_token": "old"})

	// Then
	require.Nil(t, account.Extra)
}

func TestBumpKiroCredentialGeneration_FirstWrite_NoBump(t *testing.T) {
	for _, previous := range []map[string]any{nil, {"refresh_token": " \n"}} {
		// Given
		account := &Account{Platform: PlatformKiro, Credentials: map[string]any{"refresh_token": "first"}}

		// When
		applyKiroPrincipalChangeForTest(t, account, previous)

		// Then
		require.Nil(t, account.Extra)
	}
}

func TestBumpKiroCredentialGeneration_Replacement_CreatesExtra(t *testing.T) {
	// Given
	account := &Account{Platform: PlatformKiro, Credentials: map[string]any{"refresh_token": "new"}}

	// When
	applyKiroPrincipalChangeForTest(t, account, map[string]any{"refresh_token": "old"})

	// Then
	require.Equal(t, int64(1), account.Extra[kiroCredentialGenerationKey])
}

func TestBumpKiroCredentialGeneration_NilAccount_NoPanic(t *testing.T) {
	// Given / When / Then
	require.NotPanics(t, func() { bumpKiroCredentialGenerationOnPrincipalChange(nil, nil) })
}

func TestKiroCatalogScope_SwapPaidToFreeSocialCredentials_InvalidatesCatalog(t *testing.T) {
	// Given: a paid social principal without profile ARN or client ID.
	account := kiroPrincipalReplacementAccount()
	stored, ok := account.kiroModelCatalog()
	require.True(t, ok)
	require.Len(t, stored.ModelIDs, 19)
	require.Equal(t, account.kiroCatalogScopeFingerprint(), stored.ScopeFingerprint)
	gateway := &GatewayService{}
	decision, allowed := gateway.kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
	require.Equal(t, kiroCatalogAllowed, decision)
	require.True(t, allowed)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	credentials := shallowCopyMap(account.Credentials)
	credentials["refresh_token"] = "FREE-REFRESH"
	credentials["access_token"] = "FREE-ACCESS"

	// When: the real account update replaces only the principal's tokens.
	updated, err := NewAccountService(repo, nil).Update(context.Background(), account.ID, UpdateAccountRequest{Credentials: &credentials})

	// Then: the stored paid catalog cannot authorize Opus for the free principal.
	require.NoError(t, err)
	decision, allowed = gateway.kiroCatalogDecide(context.Background(), updated, "claude-opus-4-5")
	t.Logf("fingerprint_unchanged=%t decision=%s opus_allowed=%t", stored.ScopeFingerprint == updated.kiroCatalogScopeFingerprint(), decision, allowed)
	require.NotEqual(t, stored.ScopeFingerprint, updated.kiroCatalogScopeFingerprint(), "principal replacement must change the fingerprint")
	require.Equal(t, int64(1), updated.kiroCredentialGeneration())
	require.Equal(t, kiroCatalogUnknown, decision)
	require.False(t, allowed)
}

func TestKiroCatalogScope_TokenRotation_KeepsIdentity(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "forced"}[force], func(t *testing.T) {
			// Given: an authoritative catalog and credentials requiring refresh.
			account := kiroPrincipalReplacementAccount()
			account.Credentials["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			fingerprint := account.kiroCatalogScopeFingerprint()
			repo := &kiroRefreshRepo{}
			gateway := &GatewayService{accountRepo: repo, httpUpstream: &kiroRefreshUpstream{}}

			// When: the refresh path rotates both tokens and persists them.
			token, err := gateway.kiroAccessToken(context.Background(), account, force)

			// Then: rotation retains catalog authority and the generation.
			require.NoError(t, err)
			require.Equal(t, "new-access", token)
			require.Equal(t, "rotated-refresh", repo.saved["refresh_token"])
			require.NotEmpty(t, repo.saved["expires_at"])
			require.Equal(t, fingerprint, account.kiroCatalogScopeFingerprint())
			require.Equal(t, int64(0), account.kiroCredentialGeneration())
			catalog, ok := account.kiroModelCatalog()
			require.True(t, ok)
			require.Equal(t, kiro.CatalogStateReady, catalog.EffectiveState(time.Now()))
			decision, allowed := gateway.kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
			require.Equal(t, kiroCatalogAllowed, decision)
			require.True(t, allowed)
		})
	}
}

func TestAdminUpdateAccount_PreservesDetectedCatalogAndGeneration(t *testing.T) {
	// Given: server-managed catalog metadata and a nonzero generation.
	account := kiroPrincipalReplacementAccount()
	account.Extra[kiroCredentialGenerationKey] = int64(7)
	catalog := account.Extra[kiroDetectedModelCatalogKey]
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}

	// When: an admin submits extra without either managed key.
	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Extra: map[string]any{"custom": "value"}})

	// Then: both managed values survive the replacement of extra.
	require.NoError(t, err)
	require.Equal(t, catalog, updated.Extra[kiroDetectedModelCatalogKey])
	require.Equal(t, int64(7), updated.kiroCredentialGeneration())
}

func TestKiroCatalogScope_PrincipalReplacement_PreservesGenerationWithExtra(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, withExtra := range []bool{false, true} {
			name := map[bool]string{false: "account", true: "admin"}[admin] + map[bool]string{false: "/nil_extra", true: "/with_extra"}[withExtra]
			t.Run(name, func(t *testing.T) {
				// Given: a principal that has already been replaced seven times.
				account := kiroPrincipalReplacementAccount()
				account.Extra[kiroCredentialGenerationKey] = int64(7)
				catalog := account.Extra[kiroDetectedModelCatalogKey]
				repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{account.ID: account}}
				credentials := shallowCopyMap(account.Credentials)
				credentials["refresh_token"] = "FREE-REFRESH"
				var extra map[string]any
				if withExtra {
					extra = map[string]any{"custom": "value"}
				}

				// When: either management seam replaces credentials with optional extra.
				var updated *Account
				var err error
				if admin {
					updated, err = (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Credentials: credentials, Extra: extra})
				} else {
					req := UpdateAccountRequest{Credentials: &credentials}
					if withExtra {
						req.Extra = &extra
					}
					updated, err = NewAccountService(repo, nil).Update(context.Background(), account.ID, req)
				}

				// Then: generation advances from the stored value, retaining the catalog.
				require.NoError(t, err)
				require.Equal(t, int64(8), updated.kiroCredentialGeneration())
				require.Equal(t, catalog, updated.Extra[kiroDetectedModelCatalogKey])
			})
		}
	}
}
