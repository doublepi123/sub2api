package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsKiroInvalidModelIDError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "400 full real body",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"message":"Invalid model ID or insufficient subscription level to use it.","reason":"INVALID_MODEL_ID"}`),
			want:       true,
		},
		{
			name:       "400 reason only",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"reason":"INVALID_MODEL_ID"}`),
			want:       true,
		},
		{
			name:       "400 message only no reason field",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"message":"Invalid model ID or insufficient subscription level to use it."}`),
			want:       true,
		},
		{
			name:       "400 bad json body",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"message":"bad json body"}`),
			want:       false,
		},
		{
			name:       "400 throttled reason",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"reason":"THROTTLED"}`),
			want:       false,
		},
		{
			name:       "400 empty body",
			statusCode: http.StatusBadRequest,
			body:       []byte{},
			want:       false,
		},
		{
			name:       "400 non-JSON garbage bytes",
			statusCode: http.StatusBadRequest,
			body:       []byte(`not a json at all {[]`),
			want:       false,
		},
		{
			name:       "403 full real body status gate",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"message":"Invalid model ID or insufficient subscription level to use it.","reason":"INVALID_MODEL_ID"}`),
			want:       false,
		},
		{
			name:       "404 full real body status gate",
			statusCode: http.StatusNotFound,
			body:       []byte(`{"message":"Invalid model ID or insufficient subscription level to use it.","reason":"INVALID_MODEL_ID"}`),
			want:       false,
		},
		{
			name:       "200 full real body status gate",
			statusCode: http.StatusOK,
			body:       []byte(`{"message":"Invalid model ID or insufficient subscription level to use it.","reason":"INVALID_MODEL_ID"}`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isKiroInvalidModelIDError(tt.statusCode, tt.body)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsKiroInvalidModelIDError_NormalizationGotcha(t *testing.T) {
	normalized := normalizeModelNotFoundBody([]byte("INVALID_MODEL_ID"))
	require.Equal(t, "invalid model id", normalized)
}
