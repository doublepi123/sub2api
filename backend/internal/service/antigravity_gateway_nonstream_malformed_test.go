package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAntigravityMalformedFunctionCallNonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name   string
		frames string
		status int
	}{
		{"signature only", `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL","content":{"parts":[{"thoughtSignature":"test-signature"}]}}]}}` + "\n\n", http.StatusBadGateway},
		{"error after parts", `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}}` + "\n\n" + `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}` + "\n\n", http.StatusBadGateway},
		{"error before usage", `data: {"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}}` + "\n\n" + `data: {"response":{"usageMetadata":{"promptTokenCount":1}}}` + "\n\n", http.StatusBadGateway},
		{"normal tool", `data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"read","args":{}}}]}}]}}` + "\n\n", http.StatusOK},
		{"normal text", `data: {"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"hello"}]}}]}}` + "\n\n", http.StatusOK},
	} {
		for _, protocol := range []string{"responses", "chat"} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.frames))}
				handle := svc.handleResponsesNonStreamingFromAntigravity
				if protocol == "chat" {
					handle = svc.handleChatCompletionsNonStreamingFromAntigravity
				}
				result, err := handle(c, resp, time.Now(), "test-model")
				if tc.status == http.StatusBadGateway {
					// The fork retries malformed turns before committing any client
					// output. The outer handler writes the 502 only after retries end.
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result)
					require.Equal(t, tc.status, failover.StatusCode)
					require.Equal(t, antigravityMalformedFunctionCallReason, failover.Reason)
					require.True(t, failover.RetryableOnSameAccount)
					require.Contains(t, string(failover.ResponseBody), `"code":"malformed_function_call"`)
					require.False(t, c.Writer.Written())
					require.Empty(t, recorder.Body.String())
				} else {
					require.Equal(t, tc.status, recorder.Code)
					require.NoError(t, err)
					if protocol == "responses" {
						require.Contains(t, recorder.Body.String(), `"status":"completed"`)
					} else {
						require.Contains(t, recorder.Body.String(), `"choices"`)
					}
				}
			})
		}
	}
}
