package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

func refreshKiroCredentials(ctx context.Context, account *Account, upstream HTTPUpstream, tlsFP *TLSFingerprintProfileService) (map[string]any, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	if refreshToken == "" {
		return nil, errors.New("kiro refresh token is required")
	}
	social := kiroUsesSocialRefresh(account)
	clientID := strings.TrimSpace(account.GetCredential("client_id"))
	clientSecret := strings.TrimSpace(account.GetCredential("client_secret"))
	if !social && (clientID == "" || clientSecret == "") {
		return nil, errors.New("kiro Builder ID refresh credentials are incomplete")
	}
	if upstream == nil {
		return nil, errors.New("kiro http upstream is not configured")
	}

	var (
		body     []byte
		tokenURL string
	)
	if social {
		body, _ = json.Marshal(map[string]string{"refreshToken": refreshToken})
		tokenURL = kiro.SocialRefreshURL(account.GetCredential("region"))
	} else {
		body, _ = json.Marshal(map[string]string{
			"grantType":    "refresh_token",
			"clientId":     clientID,
			"clientSecret": clientSecret,
			"refreshToken": refreshToken,
		})
		tokenURL = kiro.OIDCTokenURL(account.GetCredential("region"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyKiroIDEHeaders(req, account)
	resp, err := upstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, resolveKiroTLSProfile(tlsFP, account))
	if err != nil {
		return nil, fmt.Errorf("refresh kiro token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("refresh kiro token: upstream status %d", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileArn   string `json:"profileArn"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode kiro refresh response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, errors.New("refresh kiro token: response omitted accessToken")
	}

	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = result.AccessToken
	if result.RefreshToken != "" {
		credentials["refresh_token"] = result.RefreshToken
	}
	if profileARN := strings.TrimSpace(result.ProfileArn); profileARN != "" {
		credentials["profile_arn"] = profileARN
	}
	if social {
		credentials["auth_method"] = "social"
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	credentials["expires_at"] = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	return credentials, nil
}

func applyKiroIDEHeaders(req *http.Request, account *Account) {
	if req == nil || account == nil {
		return
	}
	machineID := kiro.MachineID(kiro.MachineIDInput{
		Configured:   account.GetCredential("machine_id"),
		APIKey:       firstNonEmpty(account.GetCredential("kiro_api_key"), account.GetCredential("api_key")),
		RefreshToken: account.GetCredential("refresh_token"),
		AccountID:    account.ID,
	})
	kiro.ApplyIDEHeaders(req.Header.Set, machineID)
}

func resolveKiroTLSProfile(tlsFP *TLSFingerprintProfileService, account *Account) *tlsfingerprint.Profile {
	if tlsFP == nil {
		return nil
	}
	return tlsFP.ResolveTLSProfile(account)
}

func kiroUsesSocialRefresh(account *Account) bool {
	if account == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(account.GetCredential("auth_method"))) {
	case "social", "google", "github":
		return true
	case "idc", "builder_id", "builder-id", "builderid":
		return false
	}
	return strings.TrimSpace(account.GetCredential("client_id")) == "" &&
		strings.TrimSpace(account.GetCredential("client_secret")) == ""
}

func kiroTokenNeedsRefresh(raw string, now time.Time) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if expiresAt, err := time.Parse(time.RFC3339, raw); err == nil {
		return !expiresAt.After(now.Add(2 * time.Minute))
	}
	if unix, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return !time.Unix(unix, 0).After(now.Add(2 * time.Minute))
	}
	return true
}
