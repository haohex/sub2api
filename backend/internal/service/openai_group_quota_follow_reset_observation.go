package service

import (
	"context"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

func isDefaultCodexRateLimitEvent(payload []byte) bool {
	limitName := strings.TrimSpace(gjson.GetBytes(payload, "metered_limit_name").String())
	if limitName == "" {
		limitName = strings.TrimSpace(gjson.GetBytes(payload, "limit_name").String())
	}
	if limitName == "" {
		return true
	}
	normalized := strings.ReplaceAll(strings.ToLower(limitName), "_", "-")
	return normalized == "codex"
}

// observeOpenAIWeeklyResetEvent extracts the weekly window from a normal
// codex.rate_limits WebSocket event. It deliberately ignores non-default
// meters and short windows; no upstream quota endpoint is queried.
func observeOpenAIWeeklyResetEvent(ctx context.Context, account *Account, payload []byte) {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.ParentAccountID != nil {
		return
	}
	if !gjson.ValidBytes(payload) || strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "codex.rate_limits" || !isDefaultCodexRateLimitEvent(payload) {
		return
	}
	for _, name := range []string{"primary", "secondary"} {
		window := gjson.GetBytes(payload, "rate_limits."+name)
		minutes := window.Get("window_minutes")
		if minutes.Type != gjson.Number || minutes.Float() != float64(minutes.Int()) || minutes.Int() <= 360 {
			continue
		}
		resetAt := window.Get("reset_at")
		if resetAt.Type == gjson.Number && resetAt.Float() == float64(resetAt.Int()) && validOpenAIQuotaResetUnix(resetAt.Int()) {
			ObserveOpenAIWeeklyResetAt(ctx, account.ID, time.Unix(resetAt.Int(), 0).UTC())
			return
		}
	}
}
