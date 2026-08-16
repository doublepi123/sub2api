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
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

var kiroRefreshLocks sync.Map

func kiroAccountLock(accountID int64) *sync.Mutex {
	value, _ := kiroRefreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	lock, ok := value.(*sync.Mutex)
	if !ok {
		panic("kiro refresh lock has unexpected type")
	}
	return lock
}

func (s *GatewayService) forwardKiro(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest) (*ForwardResult, error) {
	startTime := time.Now()
	originalModel := parsed.Model
	mappedModel := account.GetMappedModel(originalModel)
	resp, err := s.forwardKiroAnthropicResponse(ctx, account, parsed.Body.Bytes(), mappedModel, parsed.Stream)
	if err != nil {
		if c != nil {
			c.JSON(http.StatusBadGateway, gin.H{"type": "error", "error": gin.H{"type": "upstream_error", "message": "Kiro upstream request failed"}})
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return s.handleErrorResponse(ctx, resp, c, account, mappedModel)
	}
	if parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	if parsed.Stream {
		streamResult, streamErr := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, mappedModel, false)
		if streamErr != nil {
			if partial := partialStreamUsageResult(c, resp, streamResult, originalModel, mappedModel, startTime, streamErr); partial != nil {
				return partial, streamErr
			}
			return nil, streamErr
		}
		return &ForwardResult{
			RequestID: resp.Header.Get("x-request-id"), Usage: *streamResult.usage,
			Model: originalModel, UpstreamModel: mappedModel, Stream: true,
			Duration: time.Since(startTime), FirstTokenMs: streamResult.firstTokenMs,
			ClientDisconnect: streamResult.clientDisconnect,
		}, nil
	}

	usage, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, mappedModel)
	if err != nil {
		return nil, err
	}
	return &ForwardResult{
		RequestID: resp.Header.Get("x-request-id"), Usage: *usage,
		Model: originalModel, UpstreamModel: mappedModel, Stream: false,
		Duration: time.Since(startTime),
	}, nil
}

// forwardKiroAnthropicResponse sends an Anthropic request to Kiro and exposes
// the result as an Anthropic-compatible HTTP response. Kiro always streams its
// binary response; stream controls only the synthetic response format.
func (s *GatewayService) forwardKiroAnthropicResponse(ctx context.Context, account *Account, anthropicBody []byte, mappedModel string, stream bool) (*http.Response, error) {
	profileARN := strings.TrimSpace(account.GetCredential("profile_arn"))
	payload, inputTokens, err := kiro.BuildRequest(anthropicBody, mappedModel, profileARN)
	if err != nil {
		return nil, err
	}

	accessToken, err := s.kiroAccessToken(ctx, account, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.sendKiroRequest(ctx, account, payload, accessToken)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		accessToken, err = s.kiroAccessToken(ctx, account, true)
		if err != nil {
			return nil, err
		}
		resp, err = s.sendKiroRequest(ctx, account, payload, accessToken)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return resp, nil
	}

	header := resp.Header.Clone()
	header.Set("Content-Type", map[bool]string{true: "text/event-stream", false: "application/json"}[stream])
	requestID := header.Get("x-amzn-requestid")
	if requestID == "" {
		requestID = header.Get("x-amz-request-id")
	}
	if requestID != "" {
		header.Set("x-request-id", requestID)
	}
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = resp.Body.Close() }()
		err := kiro.TransformResponse(resp.Body, writer, mappedModel, inputTokens, stream)
		_ = writer.CloseWithError(err)
	}()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: reader, Request: resp.Request}, nil
}

func (s *GatewayService) sendKiroRequest(ctx context.Context, account *Account, payload []byte, accessToken string) (*http.Response, error) {
	region := strings.TrimSpace(account.GetCredential("region"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiro.RuntimeURL(region), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	req.Header.Set("x-amz-target", kiro.RuntimeTarget)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	return s.httpUpstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, s.kiroTLSProfile(account))
}

// FetchKiroUsageLimits queries the same read-only credits endpoint used by the
// Kiro client. It shares the gateway token refresh and proxy/TLS path so quota
// display cannot drift from the credentials used for model requests.
func (s *GatewayService) FetchKiroUsageLimits(ctx context.Context, account *Account) (*kiro.UsageLimitsResponse, error) {
	if account == nil || account.Platform != PlatformKiro {
		return nil, errors.New("kiro account is required")
	}
	accessToken, err := s.kiroAccessToken(ctx, account, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.sendKiroUsageLimitsRequest(ctx, account, accessToken)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		accessToken, err = s.kiroAccessToken(ctx, account, true)
		if err != nil {
			return nil, err
		}
		resp, err = s.sendKiroUsageLimitsRequest(ctx, account, accessToken)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()

	const maxUsageResponseSize = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read kiro usage limits: %w", err)
	}
	if len(body) > maxUsageResponseSize {
		return nil, errors.New("kiro usage limits response is too large")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &kiro.UsageLimitsHTTPError{StatusCode: resp.StatusCode}
	}
	var result kiro.UsageLimitsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode kiro usage limits: %w", err)
	}
	return &result, nil
}

func (s *GatewayService) sendKiroUsageLimitsRequest(ctx context.Context, account *Account, accessToken string) (*http.Response, error) {
	req, err := kiro.NewUsageLimitsRequest(
		ctx,
		account.GetCredential("region"),
		account.GetCredential("profile_arn"),
		accessToken,
	)
	if err != nil {
		return nil, err
	}
	return s.httpUpstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, s.kiroTLSProfile(account))
}

func (s *GatewayService) kiroAccessToken(ctx context.Context, account *Account, force bool) (string, error) {
	lock := kiroAccountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()

	token := strings.TrimSpace(account.GetCredential("access_token"))
	if !force && token != "" && !kiroTokenNeedsRefresh(account.GetCredential("expires_at"), time.Now()) {
		return token, nil
	}
	refreshToken := strings.TrimSpace(account.GetCredential("refresh_token"))
	clientID := strings.TrimSpace(account.GetCredential("client_id"))
	clientSecret := strings.TrimSpace(account.GetCredential("client_secret"))
	if refreshToken == "" || clientID == "" || clientSecret == "" {
		if !force && token != "" {
			return token, nil
		}
		return "", errors.New("kiro Builder ID refresh credentials are incomplete")
	}

	body, _ := json.Marshal(map[string]string{
		"grantType": "refresh_token", "clientId": clientID,
		"clientSecret": clientSecret, "refreshToken": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiro.OIDCTokenURL(account.GetCredential("region")), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpUpstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, s.kiroTLSProfile(account))
	if err != nil {
		return "", fmt.Errorf("refresh kiro token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("refresh kiro token: upstream status %d", resp.StatusCode)
	}
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode kiro refresh response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return "", errors.New("refresh kiro token: response omitted accessToken")
	}
	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = result.AccessToken
	if result.RefreshToken != "" {
		credentials["refresh_token"] = result.RefreshToken
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 3600
	}
	credentials["expires_at"] = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	if err := persistAccountCredentials(ctx, s.accountRepo, account, credentials); err != nil {
		return "", fmt.Errorf("persist refreshed kiro token: %w", err)
	}
	return result.AccessToken, nil
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

func accountProxyURL(account *Account) string {
	if account != nil && account.ProxyID != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func (s *GatewayService) kiroTLSProfile(account *Account) *tlsfingerprint.Profile {
	if s.tlsFPProfileService == nil {
		return nil
	}
	return s.tlsFPProfileService.ResolveTLSProfile(account)
}
