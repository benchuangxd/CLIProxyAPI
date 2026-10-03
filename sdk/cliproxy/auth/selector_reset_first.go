package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ResetFirstSelector prefers the credential whose quota window resets soonest,
// so quota that is about to expire is consumed before quota that will last longer.
//
// Reset times come from the passive quota snapshot observed on upstream responses
// (Claude anthropic-ratelimit-unified-* and Codex x-codex-* headers). The longest
// window (weekly) is preferred over the short window because unused weekly quota
// is the expensive kind to waste. Credentials without a known upcoming reset are
// ordered after those with one, falling back to deterministic fill-first order.
type ResetFirstSelector struct {
	// nowFunc overrides the clock in tests.
	nowFunc func() time.Time
}

// resetSignalKeys lists, per provider, the quota headers carrying a reset time in
// preference order. Keys ending in "-after-seconds" are relative to ObservedAt;
// all others are absolute Unix seconds.
var resetSignalKeys = map[string][]string{
	"claude": {
		"Anthropic-Ratelimit-Unified-7d-Reset",
		"Anthropic-Ratelimit-Unified-Reset",
		"Anthropic-Ratelimit-Unified-5h-Reset",
	},
	"codex": {
		"X-Codex-Secondary-Reset-At",
		"X-Codex-Secondary-Reset-After-Seconds",
		"X-Codex-Primary-Reset-At",
		"X-Codex-Primary-Reset-After-Seconds",
	},
}

// Pick selects the available auth with the earliest upcoming quota reset.
func (s *ResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	var picked *Auth
	var pickedReset time.Time
	for _, candidate := range available {
		resetAt, ok := quotaResetAt(candidate, now)
		if !ok {
			if picked == nil {
				picked = candidate
			}
			continue
		}
		if pickedReset.IsZero() || resetAt.Before(pickedReset) {
			picked = candidate
			pickedReset = resetAt
		}
	}
	return picked, nil
}

// quotaResetAt returns the next observed quota reset for the auth, if it is in the future.
func quotaResetAt(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}
	keys := resetSignalKeys[strings.ToLower(strings.TrimSpace(auth.Provider))]
	for _, key := range keys {
		raw := strings.TrimSpace(auth.Quota.Signals[key])
		if raw == "" {
			continue
		}
		value, errParse := strconv.ParseFloat(raw, 64)
		if errParse != nil || value <= 0 {
			continue
		}
		var resetAt time.Time
		if strings.HasSuffix(key, "-After-Seconds") {
			if auth.Quota.ObservedAt.IsZero() {
				continue
			}
			resetAt = auth.Quota.ObservedAt.Add(time.Duration(value * float64(time.Second)))
		} else {
			resetAt = time.Unix(int64(value), 0)
		}
		if resetAt.After(now) {
			return resetAt, true
		}
	}
	return time.Time{}, false
}
