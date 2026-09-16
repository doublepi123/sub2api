package service

import (
	"context"
	"errors"
	"strings"
)

// bumpKiroCredentialGenerationOnPrincipalChange decides whether to invalidate a Kiro catalog when
// a management update changes principal-identifying credentials. Automatic token
// rotation bypasses this helper; an admin token write cannot prove continuity.
func bumpKiroCredentialGenerationOnPrincipalChange(account *Account, previous map[string]any) bool {
	if account == nil || account.Platform != PlatformKiro {
		return false
	}
	_, hasCatalog := account.Extra[kiroDetectedModelCatalogKey]
	for _, key := range []string{"refresh_token", "access_token", "client_id", "profile_arn", "auth_method", "provider"} {
		before, _ := previous[key].(string)
		before = strings.TrimSpace(before)
		if before != strings.TrimSpace(account.GetCredential(key)) && (before != "" || hasCatalog) {
			return true
		}
	}
	return false
}

type KiroCredentialGenerationRepository interface {
	UpdateWithKiroCredentialGeneration(context.Context, *Account) error
}

var errKiroCredentialGenerationRepository = errors.New("account repository does not support atomic Kiro credential generation")

func updateWithKiroCredentialGeneration(ctx context.Context, repo AccountRepository, account *Account) error {
	writer, ok := repo.(KiroCredentialGenerationRepository)
	if !ok {
		return errKiroCredentialGenerationRepository
	}
	return writer.UpdateWithKiroCredentialGeneration(ctx, account)
}
