//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func kiroCatalogAccountWithMode(mode string) *Account {
	return &Account{Extra: map[string]any{kiroModelCatalogModeKey: mode}}
}

func TestKiroCatalogMode_Precedence(t *testing.T) {
	cases := []struct {
		name string
		rt   KiroModelCatalogRuntime
		acct *Account
		want kiroCatalogMode
	}{
		{
			name: "emergency off beats account enforce",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeShadow, EmergencyOff: true},
			acct: kiroCatalogAccountWithMode("enforce"),
			want: kiroCatalogModeOff,
		},
		{
			name: "account off beats platform enforce",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeEnforce},
			acct: kiroCatalogAccountWithMode("off"),
			want: kiroCatalogModeOff,
		},
		{
			name: "empty override inherits platform enforce",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeEnforce},
			acct: kiroCatalogAccountWithMode(""),
			want: kiroCatalogModeEnforce,
		},
		{
			name: "garbage override falls back to platform shadow",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeShadow},
			acct: kiroCatalogAccountWithMode("bogus"),
			want: kiroCatalogModeShadow,
		},
		{
			name: "account enforce beats platform off",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeOff},
			acct: kiroCatalogAccountWithMode("enforce"),
			want: kiroCatalogModeEnforce,
		},
		{
			name: "nil account uses platform shadow",
			rt:   KiroModelCatalogRuntime{Mode: kiroCatalogModeShadow},
			acct: nil,
			want: kiroCatalogModeShadow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, resolveKiroCatalogMode(tc.rt, tc.acct))
		})
	}
}

func TestParseKiroCatalogMode_RejectsUnknown(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   kiroCatalogMode
		wantOK bool
	}{
		{name: "empty string rejected", raw: "", want: "", wantOK: false},
		{name: "trimmed and lowered enforce parses", raw: "ENFORCE ", want: kiroCatalogModeEnforce, wantOK: true},
		{name: "on rejected", raw: "on", want: "", wantOK: false},
		{name: "true rejected", raw: "true", want: "", wantOK: false},
		{name: "mixed case shadow parses", raw: "Shadow", want: kiroCatalogModeShadow, wantOK: true},
		{name: "plain off parses", raw: "off", want: kiroCatalogModeOff, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseKiroCatalogMode(tc.raw)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// kiroCatalogRuntimeSettingRepo is a minimal SettingRepository stub for the
// catalog-runtime accessor tests. fakeSettingRepo (openai_cyber_session_block_test.go)
// does not fit: its GetMultiple panics and it supports neither error injection
// nor call counting. Embedding the interface keeps every other method nil-panicking
// so accidental calls are caught immediately.
type kiroCatalogRuntimeSettingRepo struct {
	SettingRepository
	vals  map[string]string
	err   error
	calls atomic.Int32
}

func (r *kiroCatalogRuntimeSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.calls.Add(1)
	if r.err != nil {
		return nil, r.err
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func TestGetKiroModelCatalogRuntime_Defaults(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{vals: map[string]string{}}
	svc := &SettingService{settingRepo: repo}

	rt := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeShadow, rt.Mode)
	require.False(t, rt.EmergencyOff)
}

func TestGetKiroModelCatalogRuntime_ReadsSettings(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{vals: map[string]string{
		SettingKeyKiroModelCatalogEnforcementMode: "enforce",
		SettingKeyKiroModelCatalogEmergencyOff:    "true",
	}}
	svc := &SettingService{settingRepo: repo}

	rt := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeEnforce, rt.Mode)
	require.True(t, rt.EmergencyOff)
}

// TestGetKiroModelCatalogRuntime_DBErrorOnFirstBoot_FallsBackToShadow covers the
// first-boot case: no successful read has EVER happened (cache empty), so a DB
// error has no known-good policy to retain and must resolve to the documented
// default {shadow, false}. A first-boot outage must NOT resolve to enforce
// (that would turn a DB blip into a hard block).
func TestGetKiroModelCatalogRuntime_DBErrorOnFirstBoot_FallsBackToShadow(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{err: errors.New("db down")}
	svc := &SettingService{settingRepo: repo}

	rt := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeShadow, rt.Mode, "DB error must never resolve to enforce or off")
	require.False(t, rt.EmergencyOff)
}

// expireKiroRuntimeCache forces the cached entry to be expired without sleeping,
// by moving its expiresAt into the past in place. Same-package test, no clock
// injection exists in the production code, so direct cache manipulation is the
// deterministic expiry mechanism (mirrors how _CachesWithinTTL observes the cache).
func expireKiroRuntimeCache(t *testing.T, svc *SettingService) {
	t.Helper()
	entry, ok := svc.kiroModelCatalogRuntimeCache.Load().(*cachedKiroModelCatalogRuntime)
	require.True(t, ok, "expected a cached runtime entry to expire")
	require.NotNil(t, entry)
	entry.expiresAt = time.Now().Add(-time.Second).UnixNano()
}

// TestGetKiroModelCatalogRuntime_DBError_KeepsLastKnownGood is the headline
// regression: an operator has enabled enforce, the 60s cache expires, the next
// settings read hits a DB blip. The gate MUST keep the last known-good enforce;
// disabling a safety control must never be a side effect of a transient DB error.
func TestGetKiroModelCatalogRuntime_DBError_KeepsLastKnownGood(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{vals: map[string]string{
		SettingKeyKiroModelCatalogEnforcementMode: "enforce",
	}}
	svc := &SettingService{settingRepo: repo}

	// Given: a successful read primes the cache with enforce (known-good).
	primed := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeEnforce, primed.Mode)
	require.Equal(t, int32(1), repo.calls.Load())

	// And: the cache entry has expired and the DB now errors.
	expireKiroRuntimeCache(t, svc)
	repo.err = errors.New("db down")

	// When: the runtime is resolved again.
	rt := svc.GetKiroModelCatalogRuntime(context.Background())

	// Then: the last known-good enforce is retained, not downgraded to shadow.
	require.Equal(t, kiroCatalogModeEnforce, rt.Mode,
		"transient DB error must not revoke an active enforce policy")
	require.Equal(t, int32(2), repo.calls.Load())
}

// TestGetKiroModelCatalogRuntime_MissingKeysAreKnownGood: a successful read in
// which BOTH keys are absent legitimately means "configured to defaults"
// {shadow, false} and counts as known-good. A later DB error must therefore go
// through the known-good retention path (still shadow), not the first-boot
// fallback. The repo call count proves the second call actually hit the DB
// error branch rather than serving an unexpired cache entry.
func TestGetKiroModelCatalogRuntime_MissingKeysAreKnownGood(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{vals: map[string]string{}}
	svc := &SettingService{settingRepo: repo}

	// Given: a successful read with both keys absent => defaults, known-good.
	primed := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeShadow, primed.Mode)
	require.False(t, primed.EmergencyOff)
	require.Equal(t, int32(1), repo.calls.Load())

	// And: the cache entry expired and the DB now errors.
	expireKiroRuntimeCache(t, svc)
	repo.err = errors.New("db down")

	// When: the runtime is resolved again.
	rt := svc.GetKiroModelCatalogRuntime(context.Background())

	// Then: still the defaults via the known-good path, and the second call
	// really did reach the repo (expired cache -> DB attempt -> known-good keep).
	require.Equal(t, kiroCatalogModeShadow, rt.Mode)
	require.False(t, rt.EmergencyOff)
	require.Equal(t, int32(2), repo.calls.Load())
}

func TestGetKiroModelCatalogRuntime_NilServiceIsSafe(t *testing.T) {
	var svc *SettingService
	rt := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeShadow, rt.Mode)
	require.False(t, rt.EmergencyOff)
}

func TestGetKiroModelCatalogRuntime_CachesWithinTTL(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{vals: map[string]string{
		SettingKeyKiroModelCatalogEnforcementMode: "enforce",
	}}
	svc := &SettingService{settingRepo: repo}

	first := svc.GetKiroModelCatalogRuntime(context.Background())
	second := svc.GetKiroModelCatalogRuntime(context.Background())

	require.Equal(t, int32(1), repo.calls.Load(), "second call within TTL must hit the cache")
	require.Equal(t, first, second)
	require.Equal(t, kiroCatalogModeEnforce, first.Mode)
}
