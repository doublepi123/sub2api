//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newBare429CaptureParams(body string) antigravityRetryLoopParams {
	return antigravityRetryLoopParams{
		account:        &Account{ID: 8, Name: "antigravity", Platform: PlatformAntigravity},
		action:         "streamGenerateContent",
		body:           []byte(body),
		requestedModel: "gemini-4-carbon-high",
		groupID:        7,
		sessionHash:    "t6e660amdq1o",
	}
}

func readCaptures(t *testing.T, dir string) []antigravity429Sample {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	out := make([]antigravity429Sample, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		var s antigravity429Sample
		require.NoError(t, json.Unmarshal(data, &s))
		out = append(out, s)
	}
	return out
}

func resetBare429Sampler() { antigravityBare429Sampler = antigravity429Sampler{} }

func newCaptureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "captures")
	require.NoError(t, os.Mkdir(dir, 0o700))
	return dir
}

func TestCaptureAntigravityBare429_MissingDirectoryIsSkipped(t *testing.T) {
	resetBare429Sampler()
	dir := filepath.Join(t.TempDir(), "missing")
	t.Setenv(antigravity429CaptureDirEnv, dir)

	captureAntigravityBare429(newBare429CaptureParams(`{}`), http.StatusTooManyRequests, []byte(antigravityBare429Body))
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "capture never creates the directory")
}

func TestCaptureAntigravityBare429_SavesUpstreamRequest(t *testing.T) {
	resetBare429Sampler()
	dir := newCaptureDir(t)
	t.Setenv(antigravity429CaptureDirEnv, dir)

	req := `{"model":"gemini-4-carbon-high","request":{"systemInstruction":{"parts":[{"text":"sys"}]}}}`
	captureAntigravityBare429(newBare429CaptureParams(req), http.StatusTooManyRequests, []byte(antigravityBare429Body))

	got := readCaptures(t, dir)
	require.Len(t, got, 1)
	require.Equal(t, int64(8), got[0].AccountID)
	require.Equal(t, "gemini-4-carbon-high", got[0].RequestedModel)
	require.Equal(t, "t6e660amdq1o", got[0].SessionHash)
	require.Equal(t, len(req), got[0].RequestBytes)
	require.JSONEq(t, req, string(got[0].Request))
	require.Contains(t, got[0].UpstreamBody, "Resource has been exhausted")
}

func TestCaptureAntigravityBare429_SkipsWhenDisabledOrNotBare(t *testing.T) {
	resetBare429Sampler()
	dir := newCaptureDir(t)

	t.Setenv(antigravity429CaptureDirEnv, "")
	captureAntigravityBare429(newBare429CaptureParams(`{}`), http.StatusTooManyRequests, []byte(antigravityBare429Body))
	require.Empty(t, readCaptures(t, dir))

	t.Setenv(antigravity429CaptureDirEnv, dir)
	captureAntigravityBare429(newBare429CaptureParams(`{}`), http.StatusTooManyRequests, buildGeminiRateLimitBody("30s"))
	captureAntigravityBare429(newBare429CaptureParams(`{}`), http.StatusServiceUnavailable, []byte(antigravityBare429Body))
	require.Empty(t, readCaptures(t, dir))
}

func TestCaptureAntigravityBare429_HourlyCapAndRetention(t *testing.T) {
	resetBare429Sampler()
	dir := newCaptureDir(t)
	t.Setenv(antigravity429CaptureDirEnv, dir)
	t.Setenv(antigravity429CaptureMaxPerHourEnv, "3")
	t.Setenv(antigravity429CaptureKeepEnv, "2")

	for i := 0; i < 5; i++ {
		captureAntigravityBare429(newBare429CaptureParams(`{"n":1}`), http.StatusTooManyRequests, []byte(antigravityBare429Body))
	}
	require.Len(t, readCaptures(t, dir), 2, "hourly cap admits 3, retention keeps the newest 2")

	antigravityBare429Sampler.windowStart = time.Now().Add(-2 * time.Hour)
	captureAntigravityBare429(newBare429CaptureParams(`{"n":2}`), http.StatusTooManyRequests, []byte(antigravityBare429Body))
	got := readCaptures(t, dir)
	require.Len(t, got, 2)
	require.JSONEq(t, `{"n":2}`, string(got[1].Request), "a new hour admits captures again and the newest file sorts last")
}

func TestAntigravityRetryLoop_CapturesRequestOnExhaustedBare429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetBare429Sampler()
	dir := newCaptureDir(t)
	t.Setenv(antigravity429CaptureDirEnv, dir)

	responses := make([]*http.Response, 0, 16)
	for i := 0; i < 16; i++ {
		responses = append(responses, &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(antigravityBare429Body))),
		})
	}
	upstream := &queuedHTTPUpstreamStub{responses: responses}
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	body := []byte(`{"model":"gemini-3-flash","messages":[{"role":"user","content":"hi"}]}`)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", body)

	_, _ = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)

	got := readCaptures(t, dir)
	require.Len(t, got, 1, "one capture per exhausted bare 429, not one per retry")
	require.NotEmpty(t, upstream.requestBodies)
	require.JSONEq(t, string(upstream.requestBodies[len(upstream.requestBodies)-1]), string(got[0].Request))
	require.Equal(t, account.ID, got[0].AccountID)
}
