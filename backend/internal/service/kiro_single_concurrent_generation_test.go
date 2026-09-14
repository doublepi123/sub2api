//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type kiroSingleInterleavingRepo struct {
	*upstreamBillingProbeAccountRepo
	beforeWrite func()
}

func (r *kiroSingleInterleavingRepo) UpdateWithKiroCredentialGeneration(ctx context.Context, account *Account) error {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	return r.upstreamBillingProbeAccountRepo.UpdateWithKiroCredentialGeneration(ctx, account)
}

func TestAccountService_ConcurrentBulkThenSinglePrincipalWrite(t *testing.T) {
	for _, adminEntry := range []bool{false, true} {
		t.Run(map[bool]string{false: "account", true: "admin"}[adminEntry], func(t *testing.T) {
			// Given: single-account B reads generation 7 before bulk A commits.
			ctx := context.Background()
			a := kiroPrincipalReplacementAccount()
			a.Extra[kiroCredentialGenerationKey] = int64(7)
			accounts := map[int64]*Account{a.ID: a}
			repo := &kiroSingleInterleavingRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: accounts}}
			bulk := &adminServiceImpl{accountRepo: &kiroBulkCredentialRepo{accounts: accounts}}
			repo.beforeWrite = func() {
				_, err := bulk.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{a.ID}, Credentials: map[string]any{"access_token": "P"}})
				require.NoError(t, err)
				require.Equal(t, int64(8), a.kiroCredentialGeneration())
				catalog := shallowCopyMap(a.Extra[kiroDetectedModelCatalogKey].(map[string]any))
				catalog["write_version"] = float64(5)
				catalog["scope_fingerprint"] = a.kiroCatalogScopeFingerprint()
				a.Extra[kiroDetectedModelCatalogKey] = catalog
			}
			credentials := mergeMap(a.Credentials, map[string]any{"access_token": "F"})
			// When: B resumes through either single-account service entry point.
			var got *Account
			var err error
			if adminEntry {
				got, err = (&adminServiceImpl{accountRepo: repo}).UpdateAccount(ctx, a.ID, &UpdateAccountInput{Credentials: credentials})
			} else {
				got, err = NewAccountService(repo, nil).Update(ctx, a.ID, UpdateAccountRequest{Credentials: &credentials})
			}
			// Then: B advances the generation and retains, but invalidates, A's catalog.
			require.NoError(t, err)
			require.Equal(t, int64(9), got.kiroCredentialGeneration())
			require.Equal(t, "F", got.GetCredential("access_token"))
			require.Equal(t, float64(5), got.Extra[kiroDetectedModelCatalogKey].(map[string]any)["write_version"])
			decision, allowed := (&GatewayService{}).kiroCatalogDecide(ctx, got, "claude-opus-4-5")
			require.Equal(t, kiroCatalogUnknown, decision)
			require.False(t, allowed)
		})
	}
}
