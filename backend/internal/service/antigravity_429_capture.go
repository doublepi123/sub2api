package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Antigravity rejects some request content with a bare 429 that looks like a quota
// error (no quotaResetDelay / RetryInfo). Setting SUB2API_ANTIGRAVITY_429_CAPTURE_DIR
// saves the exact upstream request body of such failures into that existing directory so
// the triggering text can be identified. Capture is off by default; files may contain
// user prompts and are 0600.
const (
	antigravity429CaptureDirEnv        = "SUB2API_ANTIGRAVITY_429_CAPTURE_DIR"
	antigravity429CaptureMaxPerHourEnv = "SUB2API_ANTIGRAVITY_429_CAPTURE_MAX_PER_HOUR"
	antigravity429CaptureKeepEnv       = "SUB2API_ANTIGRAVITY_429_CAPTURE_KEEP"

	antigravity429CaptureDefaultPerHour = 10
	antigravity429CaptureDefaultKeep    = 50
	antigravity429CaptureMaxBodyBytes   = 16 << 20
)

type antigravity429Sampler struct {
	mu          sync.Mutex
	windowStart time.Time
	count       int
	seq         uint64
}

var antigravityBare429Sampler antigravity429Sampler

func (s *antigravity429Sampler) allow(now time.Time, perHour int) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.windowStart) >= time.Hour {
		s.windowStart = now
		s.count = 0
	}
	if s.count >= perHour {
		return 0, false
	}
	s.count++
	s.seq++
	return s.seq, true
}

type antigravity429Sample struct {
	CapturedAt     string          `json:"captured_at"`
	AccountID      int64           `json:"account_id"`
	AccountName    string          `json:"account_name"`
	RequestedModel string          `json:"requested_model"`
	Action         string          `json:"action"`
	GroupID        int64           `json:"group_id"`
	SessionHash    string          `json:"session_hash"`
	UpstreamStatus int             `json:"upstream_status"`
	UpstreamBody   string          `json:"upstream_body"`
	RequestBytes   int             `json:"request_bytes"`
	Request        json.RawMessage `json:"request,omitempty"`
	RequestText    string          `json:"request_text,omitempty"`
}

func envPositiveInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return fallback
}

// captureAntigravityBare429 records the upstream request of a 429 without reset
// information. It never affects request handling: every failure is only logged.
func captureAntigravityBare429(p antigravityRetryLoopParams, statusCode int, respBody []byte) {
	dir := strings.TrimSpace(os.Getenv(antigravity429CaptureDirEnv))
	if dir == "" || statusCode != http.StatusTooManyRequests || ParseGeminiRateLimitResetTime(respBody) != nil {
		return
	}
	now := time.Now().UTC()
	seq, ok := antigravityBare429Sampler.allow(now, envPositiveInt(antigravity429CaptureMaxPerHourEnv, antigravity429CaptureDefaultPerHour))
	if !ok {
		return
	}

	sample := antigravity429Sample{
		CapturedAt:     now.Format(time.RFC3339Nano),
		RequestedModel: p.requestedModel,
		Action:         p.action,
		GroupID:        p.groupID,
		SessionHash:    p.sessionHash,
		UpstreamStatus: statusCode,
		UpstreamBody:   string(respBody),
		RequestBytes:   len(p.body),
	}
	if p.account != nil {
		sample.AccountID = p.account.ID
		sample.AccountName = p.account.Name
	}
	body := p.body
	if len(body) > antigravity429CaptureMaxBodyBytes {
		body = body[:antigravity429CaptureMaxBodyBytes]
	}
	if json.Valid(body) {
		sample.Request = json.RawMessage(body)
	} else {
		sample.RequestText = string(body)
	}

	data, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "antigravity_429_capture marshal_failed err=%v", err)
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "antigravity_429_capture open_dir_failed dir=%s err=%v (create the directory first)", dir, err)
		return
	}
	defer func() { _ = root.Close() }()
	name := fmt.Sprintf("%s_%06d_acct%d_%s.json", now.Format("20060102T150405.000000000Z"), seq, sample.AccountID, sanitizeCaptureName(p.requestedModel))
	if err := root.WriteFile(name, data, 0o600); err != nil {
		logger.LegacyPrintf("service.antigravity_gateway", "antigravity_429_capture write_failed dir=%s name=%s err=%v", dir, name, err)
		return
	}
	logger.LegacyPrintf("service.antigravity_gateway", "antigravity_429_capture saved dir=%s name=%s account=%d model=%s request_bytes=%d", dir, name, sample.AccountID, p.requestedModel, len(p.body))
	pruneAntigravity429Captures(root, envPositiveInt(antigravity429CaptureKeepEnv, antigravity429CaptureDefaultKeep))
}

func sanitizeCaptureName(model string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, model)
}

func pruneAntigravity429Captures(root *os.Root, keep int) {
	d, err := root.Open(".")
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-keep] {
		_ = root.Remove(name)
	}
}
