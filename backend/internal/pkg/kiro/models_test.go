package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var measuredFreeModelIDs = []string{
	"auto", "claude-haiku-4.5", "claude-sonnet-4", "claude-sonnet-4.5",
	"deepseek-3.2", "glm-5", "minimax-m2.1", "minimax-m2.5", "qwen3-coder-next",
}

var measuredPaidExtraModelIDs = []string{
	"claude-opus-4.5", "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8",
	"claude-opus-5", "claude-sonnet-4.6", "claude-sonnet-5",
	"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra",
}

func modelPageFixture(t *testing.T, ids []string, token string) []byte {
	t.Helper()
	models := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]string{"modelId": id})
	}
	body, err := json.Marshal(map[string]any{"models": models, "nextToken": token})
	require.NoError(t, err)
	return body
}

func TestNewListAvailableModelsRequest_Headers(t *testing.T) {
	// Given / When
	req, err := NewListAvailableModelsRequest(context.Background(), "", " test-token ", "a+/=& ?")
	// Then
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, req.Method)
	require.Nil(t, req.Body)
	require.Equal(t, "q.us-east-1.amazonaws.com", req.URL.Host)
	require.Equal(t, "/ListAvailableModels", req.URL.Path)
	require.Equal(t, "AI_EDITOR", req.URL.Query().Get("origin"))
	require.Equal(t, "a+/=& ?", req.URL.Query().Get("nextToken"))
	for key, value := range map[string]string{
		"Authorization": "Bearer test-token", "Accept": "application/json",
		"x-amzn-codewhisperer-optout": "true", "x-amzn-kiro-agent-mode": "vibe",
		"amz-sdk-request": "attempt=1; max=1",
	} {
		require.Equal(t, value, req.Header.Get(key))
	}
	_, hasTarget := req.Header[http.CanonicalHeaderKey("x-amz-target")]
	require.False(t, hasTarget)
	_, err = uuid.Parse(req.Header.Get("amz-sdk-invocation-id"))
	require.NoError(t, err)
	require.Equal(t, "https://q.us-east-1.amazonaws.com/ListAvailableModels?origin=AI_EDITOR", ListAvailableModelsURL("", ""))
}

func TestListAvailableModelsHost_UsesQHost(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-central-1", "ap-northeast-1", "", " us-west-2 "} {
		t.Run(region, func(t *testing.T) {
			// Given / When / Then
			require.Equal(t, "q."+normalizedRegion(region)+".amazonaws.com", ListAvailableModelsHost(region))
		})
	}
}

func TestParseListAvailableModelsPage_MissingModelsField_IsFailure(t *testing.T) {
	for _, body := range []string{`{}`, `{"defaultModel":{"modelId":"auto"}}`, `{"models":null}`} {
		t.Run(body, func(t *testing.T) {
			// Given / When
			_, err := ParseListAvailableModelsPage([]byte(body))
			// Then
			var fetchErr *CatalogFetchError
			require.ErrorAs(t, err, &fetchErr)
			require.Equal(t, "missing_models_field", fetchErr.Code)
		})
	}
}

func TestParseListAvailableModelsPage_EmptyModels_IsSuccess(t *testing.T) {
	// Given / When
	page, err := ParseListAvailableModelsPage([]byte(`{"models":[],"nextToken":null}`))
	// Then
	require.NoError(t, err)
	require.NotNil(t, page.Models)
	require.Empty(t, *page.Models)
}

func TestParseListAvailableModelsPage_BadJSON(t *testing.T) {
	for _, body := range []string{`{`, `[]`, `{"models":{}}`, `{"models":[]} {}`} {
		t.Run(body, func(t *testing.T) {
			// Given / When
			_, err := ParseListAvailableModelsPage([]byte(body))
			// Then
			var fetchErr *CatalogFetchError
			require.ErrorAs(t, err, &fetchErr)
			require.Equal(t, "decode", fetchErr.Code)
		})
	}
}

func TestCollectAvailableModels_Pagination(t *testing.T) {
	// Given
	paid := append(append([]string{}, measuredFreeModelIDs...), measuredPaidExtraModelIDs...)
	sort.Strings(paid)
	large := make([]string, 501)
	for i := range large {
		large[i] = fmt.Sprintf("model-%03d", i)
	}
	first := modelPageFixture(t, measuredFreeModelIDs, "page2")
	tests := []struct {
		name    string
		pages   [][]byte
		status  int
		pageErr error
		code    string
		want    []string
		calls   int
	}{
		{name: "two good pages", pages: [][]byte{first, modelPageFixture(t, append(measuredPaidExtraModelIDs, " AUTO "), "")}, want: paid, calls: 2},
		{name: "page2 500", pages: [][]byte{first, nil}, status: 500, code: "pagination_incomplete", calls: 2},
		{name: "page2 transport failure", pages: [][]byte{first, nil}, pageErr: errors.New("transport failed"), code: "pagination_incomplete", calls: 2},
		{name: "page2 malformed", pages: [][]byte{first, []byte(`{}`)}, code: "pagination_incomplete", calls: 2},
		{name: "repeated token", pages: [][]byte{first, first}, code: "pagination_limit", calls: 2},
		{name: "eleven pages", code: "pagination_limit", calls: 10},
		{name: "over 500 ids", pages: [][]byte{modelPageFixture(t, large, "")}, code: "pagination_limit", calls: 1},
		{name: "exactly 500 ids", pages: [][]byte{modelPageFixture(t, large[:500], "")}, want: large[:500], calls: 1},
		{name: "empty revocation", pages: [][]byte{[]byte(`{"models":[]}`)}, want: []string{}, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			// When
			ids, err := CollectAvailableModels(context.Background(), func(ctx context.Context, token string) ([]byte, int, http.Header, error) {
				calls++
				if calls == 1 {
					require.Empty(t, token)
				} else if len(tt.pages) > 0 {
					require.Equal(t, "page2", token)
				}
				if len(tt.pages) == 0 {
					return modelPageFixture(t, []string{"auto"}, fmt.Sprint(calls)), 200, nil, nil
				}
				require.LessOrEqual(t, calls, len(tt.pages))
				if calls == 2 && (tt.status != 0 || tt.pageErr != nil) {
					return nil, tt.status, nil, tt.pageErr
				}
				return tt.pages[calls-1], 200, nil, nil
			})
			// Then
			require.Equal(t, tt.calls, calls)
			if tt.code != "" {
				var fetchErr *CatalogFetchError
				require.ErrorAs(t, err, &fetchErr)
				require.Equal(t, tt.code, fetchErr.Code)
				require.Nil(t, ids)
				if tt.status != 0 {
					var httpErr *CatalogHTTPError
					require.ErrorAs(t, err, &httpErr)
					require.Equal(t, tt.status, httpErr.StatusCode)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, ids)
		})
	}
}

func TestCollectAvailableModels_NeverInjectsDefaultModelOrAuto(t *testing.T) {
	// Given / When
	ids, err := CollectAvailableModels(context.Background(), func(context.Context, string) ([]byte, int, http.Header, error) {
		return []byte(`{"defaultModel":{"modelId":"auto"},"models":[{"modelId":" Claude-Haiku-4.5 "},{"modelId":"claude-haiku-4.5"}]}`), 200, nil, nil
	})
	// Then
	require.NoError(t, err)
	require.Equal(t, []string{"claude-haiku-4.5"}, ids)
}

func TestParseRetryAfter_RFC9110Forms(t *testing.T) {
	// Given
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	future := now.Add(90 * time.Second)
	past := now.Add(-time.Hour)
	tests := []struct {
		name string
		raw  string
		want time.Duration
		ok   bool
	}{
		{"delay seconds", "120", 120 * time.Second, true},
		{"zero is legal", "0", 0, true},
		{"negative rejected", "-5", 0, false},
		{"explicit plus rejected", "+5", 0, false},
		{"empty", "", 0, false},
		{"garbage", "abc", 0, false},
		{"imf-fixdate", future.Format(http.TimeFormat), 90 * time.Second, true},
		{"rfc850", future.Format(time.RFC850), 90 * time.Second, true},
		{"ansi c asctime", future.Format(time.ANSIC), 90 * time.Second, true},
		{"past date lifts restriction", past.Format(http.TimeFormat), 0, true},
		{"surrounding whitespace", "  120  ", 120 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// When
			got, ok := ParseRetryAfter(tt.raw, now)
			// Then
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				require.InDelta(t, tt.want, got, float64(2*time.Second))
			}
		})
	}
}

func TestCollectAvailableModels_429_HTTPDateRetryAfter(t *testing.T) {
	// Given
	pinned := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	original := catalogNow
	catalogNow = func() time.Time { return pinned }
	t.Cleanup(func() { catalogNow = original })
	retryAt := pinned.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	// When
	ids, err := CollectAvailableModels(context.Background(), func(context.Context, string) ([]byte, int, http.Header, error) {
		return nil, 429, http.Header{"Retry-After": []string{retryAt}}, nil
	})
	// Then
	var httpErr *CatalogHTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, 429, httpErr.StatusCode)
	require.InDelta(t, 90*time.Second, httpErr.RetryAfter, float64(2*time.Second))
	require.Nil(t, ids)
}

func TestParseListAvailableModelsPage_MalformedMember_IsFailure(t *testing.T) {
	for _, body := range []string{
		`{"models":[{}]}`,
		`{"models":[null]}`,
		`{"models":[{"modelId":""}]}`,
		`{"models":[{"modelId":"   "}]}`,
		`{"models":[{"modelId":"a"},{}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			// Given / When
			_, err := ParseListAvailableModelsPage([]byte(body))
			// Then
			var fetchErr *CatalogFetchError
			require.ErrorAs(t, err, &fetchErr)
			require.Equal(t, "malformed_model", fetchErr.Code)
		})
	}
	// Given / When: a literal empty page stays a legitimate success
	page, err := ParseListAvailableModelsPage([]byte(`{"models":[]}`))
	// Then
	require.NoError(t, err)
	require.NotNil(t, page.Models)
	require.Empty(t, *page.Models)
}

func TestCollectAvailableModels_MalformedMember_ReturnsError(t *testing.T) {
	// Given / When
	ids, err := CollectAvailableModels(context.Background(), func(context.Context, string) ([]byte, int, http.Header, error) {
		return []byte(`{"models":[{"modelId":"claude-haiku-4.5"},{}]}`), 200, nil, nil
	})
	// Then
	var fetchErr *CatalogFetchError
	require.ErrorAs(t, err, &fetchErr)
	require.Equal(t, "malformed_model", fetchErr.Code)
	require.Nil(t, ids)
}

func TestCatalogHTTPError_RetryAfter(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   time.Duration
	}{{"600", 10 * time.Minute}, {"-1", 0}, {"invalid", 0}, {"9223372036854775807", 0}} {
		t.Run(tt.header, func(t *testing.T) {
			// Given / When
			ids, err := CollectAvailableModels(context.Background(), func(context.Context, string) ([]byte, int, http.Header, error) {
				return nil, 429, http.Header{"Retry-After": []string{tt.header}}, nil
			})
			// Then
			var httpErr *CatalogHTTPError
			require.ErrorAs(t, err, &httpErr)
			require.Equal(t, 429, httpErr.StatusCode)
			require.Equal(t, tt.want, httpErr.RetryAfter)
			require.Nil(t, ids)
		})
	}
}
