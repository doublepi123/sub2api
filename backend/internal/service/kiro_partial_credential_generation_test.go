//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBumpKiroCredentialGeneration_AccessTokenOnlyReplacement_Bumps(t *testing.T) {
	// Given
	account := kiroPrincipalReplacementAccount()
	previous := shallowCopyMap(account.Credentials)
	account.Credentials["access_token"] = "FREE-ACCESS"

	// When
	applyKiroPrincipalChangeForTest(t, account, previous)

	// Then
	require.Equal(t, int64(1), account.kiroCredentialGeneration())
}

func TestBumpKiroCredentialGeneration_FirstWriteWithExistingCatalog_Bumps(t *testing.T) {
	// Given: a catalog predates the first stored credential.
	account := kiroPrincipalReplacementAccount()

	// When
	applyKiroPrincipalChangeForTest(t, account, nil)

	// Then
	require.Equal(t, int64(1), account.kiroCredentialGeneration())
	decision, allowed := (&GatewayService{}).kiroCatalogDecide(context.Background(), account, "claude-opus-4-5")
	require.Equal(t, kiroCatalogUnknown, decision)
	require.False(t, allowed)
}

func TestBumpKiroCredentialGeneration_PrincipalFields_Bumps(t *testing.T) {
	for _, key := range []string{"refresh_token", "access_token", "client_id", "profile_arn", "auth_method", "provider"} {
		for _, value := range []string{"new-principal", ""} {
			t.Run(key+"/"+value, func(t *testing.T) {
				// Given
				account := kiroPrincipalReplacementAccount()
				account.Credentials[key] = "previous-principal"
				previous := shallowCopyMap(account.Credentials)
				account.Credentials[key] = value

				// When
				applyKiroPrincipalChangeForTest(t, account, previous)

				// Then
				require.Equal(t, int64(1), account.kiroCredentialGeneration())
			})
		}
	}
}

func TestBumpKiroCredentialGeneration_TokenRefreshPath_NeverBumps(t *testing.T) {
	// Given: nonzero generation and an expired token.
	account := kiroPrincipalReplacementAccount()
	account.Extra[kiroCredentialGenerationKey] = int64(7)
	account.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	fingerprint := account.kiroCatalogScopeFingerprint()
	repo := &kiroRefreshRepo{}
	gateway := &GatewayService{accountRepo: repo, httpUpstream: &kiroRefreshUpstream{}}

	// When: the gateway refreshes and persists both rotated tokens.
	token, err := gateway.kiroAccessToken(context.Background(), account, false)

	// Then
	require.NoError(t, err)
	require.Equal(t, "new-access", token)
	require.Equal(t, "new-access", repo.saved["access_token"])
	require.Equal(t, "rotated-refresh", repo.saved["refresh_token"])
	require.Equal(t, int64(7), account.kiroCredentialGeneration())
	require.Equal(t, fingerprint, account.kiroCatalogScopeFingerprint())
}
