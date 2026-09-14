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

const (
	CatalogFailureBackoffBase = 15 * time.Minute
	CatalogFailureBackoffMax  = 4 * time.Hour
	CatalogRetryAfterCap      = 4 * time.Hour
)

type CatalogState string

const (
	CatalogStateUnknown CatalogState = "unknown"
	CatalogStateReady   CatalogState = "ready"
	CatalogStateExpired CatalogState = "expired"
)

type ModelCatalog struct {
	WriteVersion        int64        `json:"write_version,omitempty"`
	SchemaVersion       int          `json:"schema_version"`
	Source              string       `json:"source"`
	State               CatalogState `json:"state"`
	ModelIDs            []string     `json:"model_ids"`
	ScopeFingerprint    string       `json:"scope_fingerprint"`
	LastSuccessAt       string       `json:"last_success_at"`
	LastAttemptAt       string       `json:"last_attempt_at"`
	LastErrorCode       string       `json:"last_error_code"`
	ConsecutiveFailures int          `json:"consecutive_failures,omitempty"`
	NextAttemptAt       string       `json:"next_attempt_at,omitempty"`
}

// CatalogNextAttempt returns the earliest time a new probe may run, and whether
// an upstream Retry-After had to be capped.
func CatalogNextAttempt(now time.Time, failures int, retryAfter time.Duration) (next time.Time, capped bool) {
	wait := CatalogFailureBackoffBase
	for n := 1; n < failures && wait < CatalogFailureBackoffMax; n++ {
		wait = min(wait*2, CatalogFailureBackoffMax)
	}
	capped = retryAfter > CatalogRetryAfterCap
	return now.Add(max(wait, min(retryAfter, CatalogRetryAfterCap))), capped
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
