package service

import (
	"log/slog"
	"strings"
)

// bumpKiroCredentialGenerationOnPrincipalChange invalidates a Kiro catalog when
// a management update replaces an existing principal's refresh token. Token
// refresh can rotate that token too, so refresh persistence must not call this.
func bumpKiroCredentialGenerationOnPrincipalChange(account *Account, previous map[string]any) {
	if account == nil || account.Platform != PlatformKiro {
		return
	}
	previousToken, _ := previous["refresh_token"].(string)
	previousToken = strings.TrimSpace(previousToken)
	if previousToken == "" || previousToken == strings.TrimSpace(account.GetCredential("refresh_token")) {
		return
	}
	generation := account.kiroCredentialGeneration() + 1
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[kiroCredentialGenerationKey] = generation
	slog.Info("kiro_credential_principal_changed", "account_id", account.ID, "generation", generation)
}
