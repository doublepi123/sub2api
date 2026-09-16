package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScopeFingerprint(t *testing.T) {
	// Given: credential tokens are deliberately outside ScopeInputs.
	in := ScopeInputs{Region: "us-east-1", ProfileARN: "profile", AuthMethod: "social", ClientID: "client", BaseURL: "https://q.us-east-1.amazonaws.com", PrincipalGeneration: "7"}
	account := struct {
		scope                     ScopeInputs
		accessToken, refreshToken string
	}{in, "old-access-secret", "old-refresh-secret"}
	before := ScopeFingerprint(account.scope)
	account.accessToken, account.refreshToken = "rotated-access-secret", "rotated-refresh-secret"
	// When
	after := ScopeFingerprint(account.scope)
	// Then
	require.Equal(t, before, after)
	sum := sha256.Sum256([]byte("kiro|us-east-1|profile|social|client|https://q.us-east-1.amazonaws.com|7"))
	require.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), after)
	for _, secret := range []string{account.accessToken, account.refreshToken, "old-access-secret", "old-refresh-secret"} {
		require.NotContains(t, after, secret)
	}
	for _, field := range []string{"region", "profile", "auth", "client", "url", "generation"} {
		t.Run(field, func(t *testing.T) {
			changed := in
			switch field {
			case "region":
				changed.Region = "eu-central-1"
			case "profile":
				changed.ProfileARN = "other-profile"
			case "auth":
				changed.AuthMethod = "idc"
			case "client":
				changed.ClientID = "other-client"
			case "url":
				changed.BaseURL = "https://other.example"
			case "generation":
				changed.PrincipalGeneration = "8"
			}
			require.NotEqual(t, before, ScopeFingerprint(changed))
		})
	}
}

func TestScopeFingerprint_IncludesPrincipalGeneration(t *testing.T) {
	// Given: a credential swap bumps the generation while token rotation does not.
	base := ScopeInputs{Region: "us-east-1", AuthMethod: "social", BaseURL: "https://q.us-east-1.amazonaws.com", PrincipalGeneration: "0"}
	// When
	gen0 := ScopeFingerprint(base)
	bumped := base
	bumped.PrincipalGeneration = "1"
	gen1 := ScopeFingerprint(bumped)
	// Then: a generation bump changes the fingerprint.
	require.NotEqual(t, gen0, gen1)
	// And: identical inputs are deterministic.
	require.Equal(t, gen0, ScopeFingerprint(base))
	// And: no token material ever appears in the output.
	for _, token := range []string{"access-token-secret", "refresh-token-secret"} {
		require.NotContains(t, gen0, token)
		require.NotContains(t, gen1, token)
	}
}

func TestModelCatalog_EffectiveState_Expired(t *testing.T) {
	// Given
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, success string
		state, want   CatalogState
	}{
		{"25 hours", now.Add(-25 * time.Hour).Format(time.RFC3339), CatalogStateReady, CatalogStateExpired},
		{"one hour", now.Add(-time.Hour).Format(time.RFC3339), CatalogStateReady, CatalogStateReady},
		{"exact boundary", now.Add(-24 * time.Hour).Format(time.RFC3339), CatalogStateReady, CatalogStateReady},
		{"empty", "", CatalogStateReady, CatalogStateUnknown},
		{"invalid", "invalid", CatalogStateReady, CatalogStateUnknown},
		{"unknown", "", CatalogStateUnknown, CatalogStateUnknown},
		{"expired", "", CatalogStateExpired, CatalogStateExpired},
		{"zero", "", "", CatalogStateUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// When / Then
			catalog := ModelCatalog{SchemaVersion: CatalogSchemaVersion, Source: CatalogSource,
				State: tt.state, ScopeFingerprint: "sha256:scope", LastSuccessAt: tt.success}
			require.Equal(t, tt.want, catalog.EffectiveState(now))
		})
	}
}

func TestModelCatalog_EffectiveState_NonAuthoritativeIsUnknown(t *testing.T) {
	// Given: a fresh ready catalog that is otherwise valid.
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	canonical := ModelCatalog{
		SchemaVersion: CatalogSchemaVersion, Source: CatalogSource, State: CatalogStateReady,
		ModelIDs: []string{"claude-opus-5"}, ScopeFingerprint: "sha256:scope",
		LastSuccessAt: now.Format(time.RFC3339),
	}
	missingFingerprint := canonical
	missingFingerprint.ScopeFingerprint = ""
	foreignSource := canonical
	foreignSource.Source = "something_else"
	futureSchema := canonical
	futureSchema.SchemaVersion = CatalogSchemaVersion + 1
	stale := canonical
	stale.LastSuccessAt = now.Add(-25 * time.Hour).Format(time.RFC3339)
	for _, tt := range []struct {
		name    string
		catalog ModelCatalog
		want    CatalogState
	}{
		{"missing fingerprint", missingFingerprint, CatalogStateUnknown},
		{"foreign source", foreignSource, CatalogStateUnknown},
		{"future schema version", futureSchema, CatalogStateUnknown},
		{"canonical", canonical, CatalogStateReady},
		{"canonical 25h old", stale, CatalogStateExpired},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// When / Then
			require.Equal(t, tt.want, tt.catalog.EffectiveState(now))
		})
	}
}

func TestModelCatalog_Contains_CaseInsensitive(t *testing.T) {
	// Given
	c := ModelCatalog{ModelIDs: []string{" Claude-Haiku-4.5 "}}
	// When / Then
	require.True(t, c.Contains(" CLAUDE-HAIKU-4.5 "))
	require.False(t, c.Contains("claude-opus-4.5"))
	require.False(t, c.Contains("auto"))
}

func TestParseModelCatalog_FromExtraMap(t *testing.T) {
	// Given
	want := ModelCatalog{SchemaVersion: CatalogSchemaVersion, Source: CatalogSource, State: CatalogStateReady,
		ModelIDs: measuredFreeModelIDs, ScopeFingerprint: "sha256:example", LastSuccessAt: "2026-09-14T10:00:00Z",
		LastAttemptAt: "2026-09-14T11:00:00Z", LastErrorCode: "pagination_incomplete"}
	data, err := json.Marshal(want)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(data, &object))
	require.ElementsMatch(t, []string{"schema_version", "source", "state", "model_ids", "scope_fingerprint", "last_success_at", "last_attempt_at", "last_error_code"}, mapKeysForCatalogTest(object))
	extra := map[string]any{"kiro_model_catalog": object}
	// When
	got, ok := ParseModelCatalog(extra["kiro_model_catalog"])
	// Then
	require.True(t, ok)
	require.Equal(t, want, got)
	for _, v := range []any{nil, "invalid", []any{}, map[string]any(nil)} {
		_, ok := ParseModelCatalog(v)
		require.False(t, ok)
	}
	got, ok = ParseModelCatalog(map[string]any{})
	require.True(t, ok)
	require.Equal(t, ModelCatalog{}, got)
}

func mapKeysForCatalogTest(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
