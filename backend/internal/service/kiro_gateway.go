package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
)

var kiroRefreshLocks sync.Map

// kiroConversationSeed derives a stable seed for Kiro conversationId /
// agentContinuationId. Empty seed is safe: kiro.BuildRequest falls back to a
// random UUID per request. Cache-control prefixes are intentionally excluded
// so unrelated conversations that share a prompt prefix do not merge.
//
// Priority: explicit session header > metadata.user_id session > content
// fallback (sess:<apiKeyID>:<hash>) > empty/random UUID.
func kiroConversationSeed(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}
	if parsed.SessionContext != nil {
		if sid := strings.TrimSpace(parsed.SessionContext.ClientSessionID); sid != "" {
			return isolateClientSessionStickySeed(parsed.SessionContext.APIKeyID, sid)
		}
	}
	if parsed.MetadataUserID != "" {
		if uid := ParseMetadataUserID(parsed.MetadataUserID); uid != nil {
			if sid := strings.TrimSpace(uid.SessionID); sid != "" {
				return sid
			}
		}
	}
	return kiroConversationFallbackSeed(parsed)
}

// kiroConversationFallbackSeed pins a conversation when the client omitted
// session headers. The hash is built from IP + UA + turn-stable request
// anchors (model / tools / system / first user) so later turns keep the same
// conversationId. The raw API key is never mixed in; only apiKeyID is used.
func kiroConversationFallbackSeed(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}
	var combined strings.Builder
	var apiKeyID int64
	if parsed.SessionContext != nil {
		apiKeyID = parsed.SessionContext.APIKeyID
		_, _ = combined.WriteString(strings.TrimSpace(parsed.SessionContext.ClientIP))
		_, _ = combined.WriteString(":")
		_, _ = combined.WriteString(NormalizeSessionUserAgent(parsed.SessionContext.UserAgent))
		_, _ = combined.WriteString("|")
	}
	contentStart := combined.Len()
	appendStickyContentAnchor(&combined, parsed)
	if combined.Len() == contentStart {
		return ""
	}
	return isolateClientSessionStickySeed(apiKeyID, hashStickyContent(combined.String()))
}

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
	resp, err := s.forwardKiroAnthropicResponse(ctx, account, parsed.Body.Bytes(), mappedModel, parsed.Stream, kiroConversationSeed(parsed))
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
func (s *GatewayService) forwardKiroAnthropicResponse(ctx context.Context, account *Account, anthropicBody []byte, mappedModel string, stream bool, conversationSeed string) (*http.Response, error) {
	profileARN := strings.TrimSpace(account.GetCredential("profile_arn"))
	built, err := kiro.BuildRequestResult(anthropicBody, mappedModel, profileARN, conversationSeed)
	if err != nil {
		return nil, err
	}

	if built.WebSearch {
		return s.forwardKiroWebSearch(ctx, account, built, mappedModel, stream)
	}

	accessToken, err := s.kiroAccessToken(ctx, account, false)
	if err != nil {
		return nil, err
	}
	resp, err := s.sendKiroRequest(ctx, account, built.Payload, accessToken)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		accessToken, err = s.kiroAccessToken(ctx, account, true)
		if err != nil {
			return nil, err
		}
		resp, err = s.sendKiroRequest(ctx, account, built.Payload, accessToken)
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
		err := kiro.TransformResponseWithOptions(resp.Body, writer, kiro.TransformOptions{
			Model: mappedModel, InputTokens: built.InputTokens, Stream: stream, ToolNameMap: built.ToolNameMap,
		})
		_ = writer.CloseWithError(err)
	}()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: reader, Request: resp.Request}, nil
}

func (s *GatewayService) forwardKiroWebSearch(ctx context.Context, account *Account, built *kiro.RequestBuild, mappedModel string, stream bool) (*http.Response, error) {
	if strings.TrimSpace(built.SearchQuery) == "" {
		return nil, errors.New("kiro web search query is required")
	}
	accessToken, err := s.kiroAccessToken(ctx, account, false)
	if err != nil {
		return nil, err
	}
	results, err := s.callKiroWebSearch(ctx, account, accessToken, built.SearchQuery)
	if err != nil {
		return nil, err
	}
	header := make(http.Header)
	header.Set("Content-Type", map[bool]string{true: "text/event-stream", false: "application/json"}[stream])
	reader, writer := io.Pipe()
	go func() {
		err := kiro.WriteWebSearchResponse(writer, mappedModel, built.SearchQuery, results, built.InputTokens, stream)
		_ = writer.CloseWithError(err)
	}()
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: reader}, nil
}

func (s *GatewayService) callKiroWebSearch(ctx context.Context, account *Account, accessToken, query string) ([]kiro.WebSearchResult, error) {
	resp, err := s.sendKiroWebSearchRequest(ctx, account, accessToken, query)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		accessToken, err = s.kiroAccessToken(ctx, account, true)
		if err != nil {
			return nil, err
		}
		resp, err = s.sendKiroWebSearchRequest(ctx, account, accessToken, query)
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read kiro web search: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		// MCP failures degrade to an empty result so Claude Code still gets a
		// well-formed web_search_tool_result instead of a hard 502.
		return nil, nil
	}
	return kiro.ParseWebSearchMCPResponse(body), nil
}

func (s *GatewayService) sendKiroWebSearchRequest(ctx context.Context, account *Account, accessToken, query string) (*http.Response, error) {
	req, err := kiro.NewWebSearchMCPRequest(
		ctx,
		account.GetCredential("region"),
		account.GetCredential("profile_arn"),
		accessToken,
		query,
	)
	if err != nil {
		return nil, err
	}
	applyKiroIDEHeaders(req, account)
	return s.httpUpstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, s.kiroTLSProfile(account))
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
	applyKiroIDEHeaders(req, account)
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
	applyKiroIDEHeaders(req, account)
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
	if !force && token != "" && strings.TrimSpace(account.GetCredential("refresh_token")) == "" {
		return token, nil
	}
	if !force && token != "" && !kiroUsesSocialRefresh(account) &&
		(strings.TrimSpace(account.GetCredential("client_id")) == "" || strings.TrimSpace(account.GetCredential("client_secret")) == "") {
		return token, nil
	}
	credentials, err := refreshKiroCredentials(ctx, account, s.httpUpstream, s.tlsFPProfileService)
	if err != nil {
		return "", err
	}
	if err := persistAccountCredentials(ctx, s.accountRepo, account, credentials); err != nil {
		return "", fmt.Errorf("persist refreshed kiro token: %w", err)
	}
	return strings.TrimSpace(account.GetCredential("access_token")), nil
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
