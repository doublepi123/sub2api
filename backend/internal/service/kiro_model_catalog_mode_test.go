//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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

func TestGetKiroModelCatalogRuntime_DBErrorFallsBackToShadow(t *testing.T) {
	repo := &kiroCatalogRuntimeSettingRepo{err: errors.New("db down")}
	svc := &SettingService{settingRepo: repo}

	rt := svc.GetKiroModelCatalogRuntime(context.Background())
	require.Equal(t, kiroCatalogModeShadow, rt.Mode, "DB error must never resolve to enforce or off")
	require.False(t, rt.EmergencyOff)
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
