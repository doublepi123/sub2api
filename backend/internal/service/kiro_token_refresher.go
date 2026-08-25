package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

var errKiroHTTPUpstreamMissing = errors.New("kiro http upstream is not configured")

const kiroBackgroundRefreshWindow = 15 * time.Minute

type KiroTokenRefresher struct {
	httpUpstream        HTTPUpstream
	tlsFPProfileService *TLSFingerprintProfileService
}

func NewKiroTokenRefresher(httpUpstream HTTPUpstream, tlsFPProfileService *TLSFingerprintProfileService) *KiroTokenRefresher {
	return &KiroTokenRefresher{
		httpUpstream:        httpUpstream,
		tlsFPProfileService: tlsFPProfileService,
	}
}

func (r *KiroTokenRefresher) SetHTTPUpstream(httpUpstream HTTPUpstream) {
	if r != nil {
		r.httpUpstream = httpUpstream
	}
}

func (r *KiroTokenRefresher) SetTLSFingerprintProfileService(tlsFPProfileService *TLSFingerprintProfileService) {
	if r != nil {
		r.tlsFPProfileService = tlsFPProfileService
	}
}

func (r *KiroTokenRefresher) CacheKey(account *Account) string {
	return KiroTokenCacheKey(account)
}

func (r *KiroTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformKiro &&
		account.Type == AccountTypeOAuth &&
		strings.TrimSpace(account.GetCredential("refresh_token")) != ""
}

func (r *KiroTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential("access_token")) == "" {
		return true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	if refreshWindow < kiroBackgroundRefreshWindow {
		refreshWindow = kiroBackgroundRefreshWindow
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *KiroTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil {
		return nil, errKiroHTTPUpstreamMissing
	}
	return refreshKiroCredentials(ctx, account, r.httpUpstream, r.tlsFPProfileService)
}

func KiroTokenCacheKey(account *Account) string {
	if account == nil {
		return "kiro:account:0"
	}
	return "kiro:account:" + strconv.FormatInt(account.ID, 10)
}
