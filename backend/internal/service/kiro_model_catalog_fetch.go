package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

type KiroModelCatalogFetcher interface {
	FetchKiroAvailableModels(ctx context.Context, account *Account) ([]string, error)
}

func (s *GatewayService) FetchKiroAvailableModels(ctx context.Context, account *Account) ([]string, error) {
	if account == nil || account.Platform != PlatformKiro {
		return nil, errors.New("kiro account is required")
	}
	accessToken, err := s.kiroAccessToken(ctx, account, false)
	if err != nil {
		return nil, err
	}
	doPage := func(ctx context.Context, nextToken string) ([]byte, int, http.Header, error) {
		req, err := kiro.NewListAvailableModelsRequest(ctx, account.GetCredential("region"), accessToken, nextToken)
		if err != nil {
			return nil, 0, nil, err
		}
		applyKiroIDEHeaders(req, account)
		resp, err := s.httpUpstream.DoWithTLS(req, accountProxyURL(account), account.ID, account.Concurrency, s.kiroTLSProfile(account))
		if err != nil {
			return nil, 0, nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		// Let the collector classify HTTP errors even if their bodies are malformed or oversized.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, resp.StatusCode, resp.Header, nil
		}
		const maxCatalogResponseSize = 1 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogResponseSize+1))
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read kiro model catalog: %w", err)
		}
		if len(body) > maxCatalogResponseSize {
			return nil, 0, nil, &kiro.CatalogFetchError{Code: "decode"}
		}
		return body, resp.StatusCode, resp.Header, nil
	}
	ids, err := kiro.CollectAvailableModels(ctx, doPage)
	var httpErr *kiro.CatalogHTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized {
		accessToken, err = s.kiroAccessToken(ctx, account, true)
		if err != nil {
			return nil, err
		}
		// A refreshed token starts a new catalog snapshot; discard every old page and cursor.
		return kiro.CollectAvailableModels(ctx, doPage)
	}
	return ids, err
}
