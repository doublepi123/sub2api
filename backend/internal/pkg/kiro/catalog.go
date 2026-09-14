package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const CatalogSource = "kiro_list_available_models"
const CatalogSchemaVersion = 1
const CatalogMaxAge = 24 * time.Hour

type CatalogState string

const (
	CatalogStateUnknown CatalogState = "unknown"
	CatalogStateReady   CatalogState = "ready"
	CatalogStateExpired CatalogState = "expired"
)

type ModelCatalog struct {
	SchemaVersion    int          `json:"schema_version"`
	Source           string       `json:"source"`
	State            CatalogState `json:"state"`
	ModelIDs         []string     `json:"model_ids"`
	ScopeFingerprint string       `json:"scope_fingerprint"`
	LastSuccessAt    string       `json:"last_success_at"`
	LastAttemptAt    string       `json:"last_attempt_at"`
	LastErrorCode    string       `json:"last_error_code"`
}

// Authoritative reports whether this catalog was written by the current
// detector at a schema this build understands, for a known scope.
func (c ModelCatalog) Authoritative() bool {
	return c.SchemaVersion == CatalogSchemaVersion && c.Source == CatalogSource && strings.TrimSpace(c.ScopeFingerprint) != ""
}

func (c ModelCatalog) EffectiveState(now time.Time) CatalogState {
	switch c.State {
	case CatalogStateReady:
		if !c.Authoritative() {
			return CatalogStateUnknown
		}
		lastSuccess, err := time.Parse(time.RFC3339, c.LastSuccessAt)
		if err != nil {
			return CatalogStateUnknown
		}
		if now.Sub(lastSuccess) > CatalogMaxAge {
			return CatalogStateExpired
		}
		return CatalogStateReady
	case CatalogStateExpired:
		return CatalogStateExpired
	case CatalogStateUnknown:
		return CatalogStateUnknown
	default:
		return CatalogStateUnknown
	}
}

func (c ModelCatalog) Contains(modelID string) bool {
	wanted := strings.ToLower(strings.TrimSpace(modelID))
	if wanted == "" {
		return false
	}
	for _, id := range c.ModelIDs {
		if strings.ToLower(strings.TrimSpace(id)) == wanted {
			return true
		}
	}
	return false
}

type ScopeInputs struct {
	Region, ProfileARN, AuthMethod, ClientID, BaseURL, PrincipalGeneration string
}

func ScopeFingerprint(in ScopeInputs) string {
	sum := sha256.Sum256([]byte("kiro|" + in.Region + "|" + in.ProfileARN + "|" + in.AuthMethod + "|" + in.ClientID + "|" + in.BaseURL + "|" + in.PrincipalGeneration))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ParseModelCatalog(v any) (ModelCatalog, bool) {
	object, ok := v.(map[string]any)
	if !ok || object == nil {
		return ModelCatalog{}, false
	}
	body, err := json.Marshal(object)
	if err != nil {
		return ModelCatalog{}, false
	}
	var catalog ModelCatalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return ModelCatalog{}, false
	}
	return catalog, true
}
