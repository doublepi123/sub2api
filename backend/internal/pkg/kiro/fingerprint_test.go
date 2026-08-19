package kiro

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeMachineID(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	require.Equal(t, hex64, NormalizeMachineID(hex64))
	require.Equal(t,
		"2582956ecc884669b54607adbffcb8942582956ecc884669b54607adbffcb894",
		NormalizeMachineID("2582956e-cc88-4669-b546-07adbffcb894"),
	)
	require.Empty(t, NormalizeMachineID("invalid"))
	require.Empty(t, NormalizeMachineID(strings.Repeat("g", 64)))
}

func TestMachineIDPrefersConfiguredThenAPIKeyThenRefresh(t *testing.T) {
	configured := strings.Repeat("cd", 32)
	require.Equal(t, configured, MachineID(MachineIDInput{
		Configured:   configured,
		APIKey:       "ksk_test",
		RefreshToken: "refresh",
	}))
	require.Equal(t, sha256Hex("KiroAPIKey/ksk_test"), MachineID(MachineIDInput{
		APIKey:       "ksk_test",
		RefreshToken: "should-not-be-used",
	}))
	require.Equal(t, sha256Hex("KotlinNativeAPI/refresh"), MachineID(MachineIDInput{
		RefreshToken: "refresh",
	}))
}

func TestMachineIDFallbackIsStablePerAccount(t *testing.T) {
	first := MachineID(MachineIDInput{AccountID: 9001})
	second := MachineID(MachineIDInput{AccountID: 9001})
	other := MachineID(MachineIDInput{AccountID: 9002})
	require.Equal(t, first, second)
	require.NotEqual(t, first, other)
	require.Len(t, first, 64)
	require.True(t, isASCIIHex(first))
}

func TestIDEUserAgentsEmbedVersionAndMachineID(t *testing.T) {
	id := strings.Repeat("11", 32)
	require.Equal(t, "aws-sdk-js/1.0.34 KiroIDE-0.11.107-"+id, AMZUserAgent(id))
	ua := UserAgent(id)
	require.Contains(t, ua, "aws-sdk-js/1.0.34 ua/2.1 os/")
	require.Contains(t, ua, "md/nodejs#22.22.0")
	require.Contains(t, ua, "KiroIDE-0.11.107-"+id)
	require.True(t, strings.Contains(ua, darwinSystemVersion) || strings.Contains(ua, windowsSystemVersion))
}

func TestApplyIDEHeaders(t *testing.T) {
	headers := map[string]string{}
	ApplyIDEHeaders(func(k, v string) { headers[k] = v }, strings.Repeat("22", 32))
	require.Contains(t, headers["User-Agent"], "KiroIDE-0.11.107-")
	require.Contains(t, headers["x-amz-user-agent"], "KiroIDE-0.11.107-")

	ApplyIDEHeaders(func(k, v string) { headers[k] = v }, "")
	require.NotEmpty(t, headers["User-Agent"])
}
