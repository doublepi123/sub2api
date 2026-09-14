package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const ListAvailableModelsOrigin = "AI_EDITOR"

func ListAvailableModelsHost(region string) string {
	return "q." + normalizedRegion(region) + ".amazonaws.com"
}

func ListAvailableModelsURL(region, nextToken string) string {
	query := url.Values{"origin": []string{ListAvailableModelsOrigin}}
	if nextToken != "" {
		query.Set("nextToken", nextToken)
	}
	return "https://" + ListAvailableModelsHost(region) + "/ListAvailableModels?" + query.Encode()
}

func NewListAvailableModelsRequest(ctx context.Context, region, accessToken, nextToken string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ListAvailableModelsURL(region, nextToken), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	return req, nil
}

type AvailableModel struct {
	ModelID          string   `json:"modelId"`
	ModelName        string   `json:"modelName"`
	Description      string   `json:"description"`
	RateMultiplier   float64  `json:"rateMultiplier"`
	RateUnit         string   `json:"rateUnit"`
	AvailableOrigins []string `json:"availableOrigins"`
}

type ListAvailableModelsResponse struct {
	DefaultModel *AvailableModel   `json:"defaultModel"`
	Models       *[]AvailableModel `json:"models"`
	NextToken    *string           `json:"nextToken"`
}

type CatalogFetchError struct{ Code string }

func (e *CatalogFetchError) Error() string {
	return "kiro model catalog: " + e.Code
}

type CatalogHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *CatalogHTTPError) Error() string {
	return fmt.Sprintf("kiro model catalog returned HTTP %d", e.StatusCode)
}

var catalogNow = time.Now

// ParseRetryAfter parses RFC 9110 §10.2.3: Retry-After = HTTP-date / delay-seconds.
func ParseRetryAfter(headerValue string, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(headerValue)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(raw, 10, 64); err == nil {
		if seconds > uint64((1<<63-1)/time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if t, err := http.ParseTime(raw); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func ParseListAvailableModelsPage(body []byte) (ListAvailableModelsResponse, error) {
	var page ListAvailableModelsResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return ListAvailableModelsResponse{}, &CatalogFetchError{Code: "decode"}
	}
	if page.Models == nil {
		return ListAvailableModelsResponse{}, &CatalogFetchError{Code: "missing_models_field"}
	}
	for _, model := range *page.Models {
		if strings.TrimSpace(model.ModelID) == "" {
			return ListAvailableModelsResponse{}, &CatalogFetchError{Code: "malformed_model"}
		}
	}
	return page, nil
}

func CollectAvailableModels(ctx context.Context, doPage func(ctx context.Context, nextToken string) ([]byte, int, http.Header, error)) (ids []string, err error) {
	completedPages := 0
	defer func() {
		if err == nil || completedPages == 0 {
			return
		}
		var fetchErr *CatalogFetchError
		if errors.As(err, &fetchErr) && fetchErr.Code == "pagination_limit" {
			return
		}
		err = errors.Join(&CatalogFetchError{Code: "pagination_incomplete"}, err)
	}()
	seenTokens := make(map[string]struct{})
	seenIDs := make(map[string]struct{})
	nextToken := ""
	for completedPages < 10 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, status, headers, err := doPage(ctx, nextToken)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			httpErr := &CatalogHTTPError{StatusCode: status}
			if status == http.StatusTooManyRequests {
				if retryAfter, ok := ParseRetryAfter(headers.Get("Retry-After"), catalogNow()); ok {
					httpErr.RetryAfter = retryAfter
				}
			}
			return nil, httpErr
		}
		page, err := ParseListAvailableModelsPage(body)
		if err != nil {
			return nil, err
		}
		for _, model := range *page.Models {
			id := strings.ToLower(strings.TrimSpace(model.ModelID))
			seenIDs[id] = struct{}{}
			if len(seenIDs) > 500 {
				return nil, &CatalogFetchError{Code: "pagination_limit"}
			}
		}
		completedPages++
		if page.NextToken == nil || *page.NextToken == "" {
			ids := make([]string, 0, len(seenIDs))
			for id := range seenIDs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return ids, nil
		}
		nextToken = *page.NextToken
		if _, exists := seenTokens[nextToken]; exists {
			return nil, &CatalogFetchError{Code: "pagination_limit"}
		}
		seenTokens[nextToken] = struct{}{}
	}
	return nil, &CatalogFetchError{Code: "pagination_limit"}
}
