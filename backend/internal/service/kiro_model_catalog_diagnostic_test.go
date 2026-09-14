//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type kiroCatalogDiagnosticRecord struct {
	Message        string   `json:"msg"`
	Level          string   `json:"level"`
	AccountID      int64    `json:"account_id"`
	Model          string   `json:"resolved_model"`
	Reason         string   `json:"reason"`
	Age            int64    `json:"catalog_age_s"`
	Source         string   `json:"source"`
	SubjectNil     bool     `json:"subject_nil"`
	ExtraKeys      []string `json:"extra_keys"`
	CatalogRawType string   `json:"catalog_raw_type"`
	ParseOK        bool     `json:"parse_ok"`
	StoredFP       string   `json:"stored_fp"`
	ComputedFP     string   `json:"computed_fp"`
	CatalogState   string   `json:"catalog_state"`
	EffectiveState string   `json:"effective_state"`
}

func readKiroCatalogDiagnostic(t *testing.T, logs *bytes.Buffer) kiroCatalogDiagnosticRecord {
	t.Helper()
	require.NotEmpty(t, logs.String(), "missing catalog decision diagnostic")
	var record kiroCatalogDiagnosticRecord
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record))
	require.Equal(t, "INFO", record.Level)
	return record
}

func TestKiroCatalogEvaluate_DiagnosticLogsCatalogRawType(t *testing.T) {
	for _, mode := range []string{"enforce", "shadow"} {
		t.Run(mode, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(10, nil, time.Now(), mode)
			a.Extra[kiroDetectedModelCatalogKey] = "not-a-map-secret-marker"
			logs := captureKiroCatalogLogs(t)
			// When
			decision, allowed, age := (&GatewayService{}).kiroCatalogEvaluate(context.Background(), &a, "claude-opus-5")
			// Then
			require.Equal(t, kiroCatalogUnknown, decision)
			require.Equal(t, mode == "shadow", allowed)
			require.Equal(t, int64(-1), age)
			r := readKiroCatalogDiagnostic(t, logs)
			require.False(t, r.ParseOK)
			require.False(t, r.SubjectNil)
			require.Equal(t, "string", r.CatalogRawType)
			require.Equal(t, []string{kiroDetectedModelCatalogKey, kiroModelCatalogModeKey}, r.ExtraKeys)
			require.Equal(t, a.ID, r.AccountID)
			require.Equal(t, "claude-opus-5", r.Model)
			require.Equal(t, string(decision), r.Reason)
			require.Equal(t, age, r.Age)
			require.Empty(t, r.Source)
			require.NotContains(t, logs.String(), "not-a-map-secret-marker")
		})
	}
}

func TestKiroCatalogEvaluate_DiagnosticLogsMissingKey(t *testing.T) {
	// Given
	a := kiroCatalogAccount(10, nil, time.Now(), "enforce")
	delete(a.Extra, kiroDetectedModelCatalogKey)
	a.Extra["z_key"] = "secret-marker"
	a.Extra["a_key"] = true
	logs := captureKiroCatalogLogs(t)
	// When
	decision, allowed, _ := (&GatewayService{}).kiroCatalogEvaluate(context.Background(), &a, "claude-opus-5")
	// Then
	require.Equal(t, kiroCatalogUnknown, decision)
	require.False(t, allowed)
	r := readKiroCatalogDiagnostic(t, logs)
	require.Equal(t, "<nil>", r.CatalogRawType)
	require.False(t, r.ParseOK)
	require.Equal(t, []string{"a_key", kiroModelCatalogModeKey, "z_key"}, r.ExtraKeys)
	require.NotContains(t, logs.String(), "secret-marker")
}

func TestKiroCatalogEvaluate_DiagnosticLogsFingerprintMismatch(t *testing.T) {
	// Given
	a := kiroCatalogAccount(10, kiroPaidCatalogIDs, time.Now(), "enforce")
	catalog, ok := a.kiroModelCatalog()
	require.True(t, ok)
	a.Credentials = map[string]any{"region": "eu-west-1"}
	logs := captureKiroCatalogLogs(t)
	// When
	decision, allowed, _ := (&GatewayService{}).kiroCatalogEvaluate(context.Background(), &a, "claude-opus-5")
	// Then
	require.Equal(t, kiroCatalogUnknown, decision)
	require.False(t, allowed)
	r := readKiroCatalogDiagnostic(t, logs)
	require.Equal(t, "kiro_catalog_decision_detail", r.Message)
	require.True(t, r.ParseOK)
	require.Equal(t, catalog.ScopeFingerprint, r.StoredFP)
	require.Equal(t, a.kiroCatalogScopeFingerprint(), r.ComputedFP)
	require.NotEmpty(t, r.StoredFP)
	require.NotEmpty(t, r.ComputedFP)
	require.NotEqual(t, r.StoredFP, r.ComputedFP)
	require.Equal(t, "ready", r.CatalogState)
	require.Equal(t, "ready", r.EffectiveState)
}

func TestKiroCatalogEvaluate_NoDiagnosticOnAllowed(t *testing.T) {
	for _, mode := range []string{"enforce", "shadow", "off"} {
		t.Run(mode, func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(10, kiroPaidCatalogIDs, time.Now(), mode)
			logs := captureKiroCatalogLogs(t)
			// When
			decision, allowed, _ := (&GatewayService{}).kiroCatalogEvaluate(context.Background(), &a, "claude-opus-5")
			// Then
			want := kiroCatalogAllowed
			if mode == "off" {
				want = kiroCatalogModeOffResult
			}
			require.Equal(t, want, decision)
			require.True(t, allowed)
			require.Empty(t, logs.String())
		})
	}
}

func TestKiroCatalogEvaluate_DiagnosticReportsActualSource(t *testing.T) {
	// Given
	a := kiroCatalogAccount(10, kiroPaidCatalogIDs, time.Now(), "shadow")
	catalog, ok := a.kiroModelCatalog()
	require.True(t, ok)
	catalog.Source = "something_else"
	a.Extra[kiroDetectedModelCatalogKey] = catalogTestMap(t, catalog)
	logs := captureKiroCatalogLogs(t)
	// When
	allowed := (&GatewayService{}).isModelSupportedByAccountWithContext(context.Background(), &a, "claude-opus-5")
	// Then
	require.True(t, allowed)
	r := readKiroCatalogDiagnostic(t, logs)
	require.Equal(t, "kiro_catalog_shadow_would_reject", r.Message)
	require.Equal(t, "catalog_unknown", r.Reason)
	require.True(t, r.ParseOK)
	require.Equal(t, "something_else", r.Source)
	require.NotEqual(t, kiro.CatalogSource, r.Source)
	require.Equal(t, "ready", r.CatalogState)
	require.Equal(t, "unknown", r.EffectiveState)
}

func TestKiroCatalogEvaluate_DiagnosticHandlesNilSubjectAndExtra(t *testing.T) {
	for _, parentMissing := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil_extra", true: "nil_subject"}[parentMissing], func(t *testing.T) {
			// Given
			a := kiroCatalogAccount(10, nil, time.Now(), "shadow")
			a.Extra = nil
			if parentMissing {
				parentID := int64(19)
				a.ParentAccountID = &parentID
			}
			logs := captureKiroCatalogLogs(t)
			// When
			decision, allowed, age := (&GatewayService{}).kiroCatalogEvaluate(context.Background(), &a, "claude-opus-5")
			// Then
			require.Equal(t, kiroCatalogUnknown, decision)
			require.True(t, allowed)
			require.Equal(t, int64(-1), age)
			r := readKiroCatalogDiagnostic(t, logs)
			require.Equal(t, parentMissing, r.SubjectNil)
			require.Equal(t, "<nil>", r.CatalogRawType)
			require.Empty(t, r.ExtraKeys)
			require.False(t, r.ParseOK)
		})
	}
}
