package repository

import (
	"context"
	"encoding/json"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.KiroCredentialGenerationRepository = (*accountRepository)(nil)

func (r *accountRepository) UpdateWithKiroCredentialGeneration(ctx context.Context, account *service.Account) error {
	return r.updateAccount(ctx, account, nil, nil, account.RateMultiplier, true)
}

func stripKiroManagedExtra(extra map[string]any) map[string]any {
	clean := copyJSONMap(extra)
	delete(clean, "kiro_credential_generation")
	delete(clean, "detected_model_catalog")
	return clean
}

func bumpKiroGenerationSQL(extraExpression string) string {
	return "jsonb_set((" + extraExpression + "), '{kiro_credential_generation}', to_jsonb(COALESCE((extra->>'kiro_credential_generation')::bigint, 0) + 1))"
}

func kiroStoredPrincipalChangedSQL(credentialsExpression string) string {
	return `EXISTS (SELECT 1 FROM unnest(ARRAY['refresh_token', 'access_token', 'client_id', 'profile_arn', 'auth_method', 'provider']) AS principal(key)
		WHERE btrim(COALESCE(credentials->>key, ''), E' \t\n\r') <> btrim(COALESCE((` + credentialsExpression + `)->>key, ''), E' \t\n\r')
		AND (btrim(COALESCE(credentials->>key, ''), E' \t\n\r') <> '' OR COALESCE(extra, '{}'::jsonb) ? 'detected_model_catalog'))`
}

// The caller holds the row lock and transaction. Credentials and the generation
// share one UPDATE; managed catalog state always comes from the stored row.
func writeKiroAccountCredentials(ctx context.Context, client *dbent.Client, account *service.Account, bump bool) (map[string]any, error) {
	credentials, err := json.Marshal(normalizeJSONMap(account.Credentials))
	if err != nil {
		return nil, err
	}
	extra, err := json.Marshal(normalizeJSONMap(stripKiroManagedExtra(account.Extra)))
	if err != nil {
		return nil, err
	}
	expression := "$2::jsonb || COALESCE((SELECT jsonb_object_agg(key, value) FROM jsonb_each(COALESCE(extra, '{}'::jsonb)) WHERE key IN ('kiro_credential_generation', 'detected_model_catalog')), '{}'::jsonb)"
	if bump {
		expression = bumpKiroGenerationSQL(expression)
	} else {
		expression = "CASE WHEN " + kiroStoredPrincipalChangedSQL("$1::jsonb") + " THEN " + bumpKiroGenerationSQL(expression) + " ELSE (" + expression + ") END"
	}
	rows, err := client.QueryContext(ctx, "UPDATE accounts SET credentials = $1::jsonb, extra = "+expression+", updated_at = NOW() WHERE id = $3 AND deleted_at IS NULL RETURNING extra", string(credentials), string(extra), account.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAccountNotFound
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	return stored, nil
}
