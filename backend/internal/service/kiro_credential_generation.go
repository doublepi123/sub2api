package service

import (
	"log/slog"
	"strings"
)

// bumpKiroCredentialGenerationOnPrincipalChange invalidates a Kiro catalog when
// a management update changes principal-identifying credentials. Automatic token
// rotation bypasses this helper; an admin token write cannot prove continuity.
func bumpKiroCredentialGenerationOnPrincipalChange(account *Account, previous map[string]any) {
	if account == nil || account.Platform != PlatformKiro {
		return
	}
	_, hasCatalog := account.Extra[kiroDetectedModelCatalogKey]
	changed := false
	for _, key := range []string{"refresh_token", "access_token", "client_id", "profile_arn", "auth_method", "provider"} {
		before, _ := previous[key].(string)
		before = strings.TrimSpace(before)
		if before != strings.TrimSpace(account.GetCredential(key)) && (before != "" || hasCatalog) {
			changed = true
			break
		}
	}
	if !changed {
		return
	}
	generation := account.kiroCredentialGeneration() + 1
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[kiroCredentialGenerationKey] = generation
	slog.Info("kiro_credential_principal_changed", "account_id", account.ID, "generation", generation)
}
