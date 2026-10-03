package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestResetFirstSelectorPick_PrefersEarliestReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{nowFunc: func() time.Time { return now }}
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }

	auths := []*Auth{
		{ID: "a-unknown", Provider: "claude"},
		{ID: "b-late", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unix(96 * time.Hour),
			"Anthropic-Ratelimit-Unified-5h-Reset": unix(time.Hour),
		}}},
		{ID: "c-soon", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unix(24 * time.Hour),
		}}},
		{ID: "d-expired", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unix(-time.Hour),
		}}},
	}

	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "c-soon" {
		t.Fatalf("Pick() auth = %v, want c-soon", got)
	}
}

func TestResetFirstSelectorPick_CodexRelativeReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	selector := &ResetFirstSelector{nowFunc: func() time.Time { return now }}

	auths := []*Auth{
		{ID: "a", Provider: "codex", Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Secondary-Reset-At": strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10),
		}}},
		{ID: "b", Provider: "codex", Quota: QuotaState{ObservedAt: now.Add(-time.Hour), Signals: map[string]string{
			"X-Codex-Secondary-Reset-After-Seconds": strconv.Itoa(int((48 * time.Hour).Seconds())),
		}}},
	}

	got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "b" {
		t.Fatalf("Pick() auth = %v, want b", got)
	}
}

func TestResetFirstSelectorPick_FallsBackToFillFirst(t *testing.T) {
	t.Parallel()

	selector := &ResetFirstSelector{}
	auths := []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}}

	got, err := selector.Pick(context.Background(), "gemini", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil || got.ID != "a" {
		t.Fatalf("Pick() auth = %v, want a", got)
	}
}
