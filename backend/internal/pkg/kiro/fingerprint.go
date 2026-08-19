package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	DefaultKiroVersion   = "0.11.107"
	DefaultNodeVersion   = "22.22.0"
	darwinSystemVersion  = "darwin#24.6.0"
	windowsSystemVersion = "win32#10.0.22631"
)

var (
	fallbackMachineIDs   = map[string]string{}
	fallbackMachineIDsMu sync.Mutex
)

// MachineIDInput is the credential material used to derive a stable IDE fingerprint.
// Account-level machine_id wins when it normalizes; otherwise the value is hashed
// from the credential type, matching kiro.rs generate_from_credentials.
type MachineIDInput struct {
	Configured   string
	APIKey       string
	RefreshToken string
	AccountID    int64
}

func NormalizeMachineID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) == 64 && isASCIIHex(trimmed) {
		return trimmed
	}
	withoutDashes := strings.Map(func(r rune) rune {
		if r == '-' {
			return -1
		}
		return r
	}, trimmed)
	if len(withoutDashes) == 32 && isASCIIHex(withoutDashes) {
		return withoutDashes + withoutDashes
	}
	return ""
}

func MachineID(in MachineIDInput) string {
	if id := NormalizeMachineID(in.Configured); id != "" {
		return id
	}
	if apiKey := strings.TrimSpace(in.APIKey); apiKey != "" {
		return sha256Hex("KiroAPIKey/" + apiKey)
	}
	if refresh := strings.TrimSpace(in.RefreshToken); refresh != "" {
		return sha256Hex("KotlinNativeAPI/" + refresh)
	}
	return fallbackMachineID(in.AccountID)
}

func fallbackMachineID(accountID int64) string {
	key := strconv.FormatInt(accountID, 10)
	fallbackMachineIDsMu.Lock()
	defer fallbackMachineIDsMu.Unlock()
	if existing, ok := fallbackMachineIDs[key]; ok {
		return existing
	}
	derived := sha256Hex("KiroFallback/" + uuid.NewString())
	fallbackMachineIDs[key] = derived
	return derived
}

func sha256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func isASCIIHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > unicode.MaxASCII || !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
		i += size
	}
	return true
}

func systemVersionForMachineID(machineID string) string {
	if machineID == "" || machineID[0] < '8' {
		return darwinSystemVersion
	}
	return windowsSystemVersion
}

// UserAgent is the Kiro IDE aws-sdk-js User-Agent.
func UserAgent(machineID string) string {
	return "aws-sdk-js/1.0.34 ua/2.1 os/" + systemVersionForMachineID(machineID) +
		" lang/js md/nodejs#" + DefaultNodeVersion +
		" api/codewhispererstreaming#1.0.34 m/E KiroIDE-" + DefaultKiroVersion + "-" + machineID
}

// AMZUserAgent is the shorter x-amz-user-agent used by the IDE client.
func AMZUserAgent(machineID string) string {
	return "aws-sdk-js/1.0.34 KiroIDE-" + DefaultKiroVersion + "-" + machineID
}

// ApplyIDEHeaders stamps the IDE identity headers onto an outbound Kiro request.
func ApplyIDEHeaders(set func(key, value string), machineID string) {
	if set == nil || strings.TrimSpace(machineID) == "" {
		return
	}
	set("User-Agent", UserAgent(machineID))
	set("x-amz-user-agent", AMZUserAgent(machineID))
}
