package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestManagerSessionAffinityRebindsAfterHigherPriorityRecovery(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		providerSuffix string
		pick           func(*Manager, context.Context, string, string, cliproxyexecutor.Options) (*Auth, error)
	}{
		{
			name:           "single provider",
			providerSuffix: "single",
			pick: func(manager *Manager, ctx context.Context, provider, model string, opts cliproxyexecutor.Options) (*Auth, error) {
				auth, _, errPick := manager.pickNext(ctx, provider, model, opts, nil)
				return auth, errPick
			},
		},
		{
			name:           "mixed provider",
			providerSuffix: "mixed",
			pick: func(manager *Manager, ctx context.Context, provider, model string, opts cliproxyexecutor.Options) (*Auth, error) {
				auth, _, _, errPick := manager.pickNextMixed(ctx, []string{provider}, model, opts, nil)
				return auth, errPick
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			provider := "affinity-priority-" + testCase.providerSuffix
			model := "affinity-priority-model"
			highID := provider + "-high"
			lowID := provider + "-low"

			manager := NewManager(nil, nil, nil)
			affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
				Fallback: &RoundRobinSelector{},
				TTL:      time.Hour,
			})
			defer affinity.Stop()
			manager.SetSelector(affinity)
			manager.RegisterExecutor(schedulerTestExecutor{provider: provider})

			for _, auth := range []*Auth{
				{ID: highID, Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "1"}},
				{ID: lowID, Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "0"}},
			} {
				if _, errRegister := manager.Register(WithSkipPersist(ctx), auth); errRegister != nil {
					t.Fatalf("Register(%s): %v", auth.ID, errRegister)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}

			opts := cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.DerivedSessionIDMetadataKey: "stable-session",
			}}
			pick := func(pickOpts cliproxyexecutor.Options) *Auth {
				t.Helper()
				auth, errPick := testCase.pick(manager, ctx, provider, model, pickOpts)
				if errPick != nil {
					t.Fatalf("pick: %v", errPick)
				}
				if auth == nil {
					t.Fatal("pick returned nil auth")
				}
				return auth
			}

			if got := pick(opts); got.ID != highID {
				t.Fatalf("cold binding = %q, want high priority %q", got.ID, highID)
			}

			manager.MarkResult(ctx, Result{
				AuthID:   highID,
				Provider: provider,
				Model:    model,
				Success:  false,
				Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			})
			if got := pick(opts); got.ID != lowID {
				t.Fatalf("failover binding = %q, want %q", got.ID, lowID)
			}

			expireSessionAffinityPriorityModelCooldown(t, manager, highID, model)
			if got := pick(opts); got.ID != highID {
				t.Fatalf("binding after higher-priority recovery = %q, want %q", got.ID, highID)
			}

			newSessionOpts := cliproxyexecutor.Options{Metadata: map[string]any{
				cliproxyexecutor.DerivedSessionIDMetadataKey: "new-session",
			}}
			if got := pick(newSessionOpts); got.ID != highID {
				t.Fatalf("cold binding for new session = %q, want high priority %q", got.ID, highID)
			}

			manager.MarkResult(ctx, Result{
				AuthID:   lowID,
				Provider: provider,
				Model:    model,
				Success:  false,
				Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
			})
			if got := pick(opts); got.ID != highID {
				t.Fatalf("binding after bound auth became unavailable = %q, want %q", got.ID, highID)
			}
		})
	}
}

func TestSessionAffinityFallbackOnlyReceivesHighestAvailablePriority(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: lastAuthSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	high := &Auth{ID: "a-high", Provider: "test", Status: StatusActive, Attributes: map[string]string{"priority": "1"}}
	low := &Auth{ID: "z-low", Provider: "test", Status: StatusActive, Attributes: map[string]string{"priority": "0"}}
	auths := []*Auth{high, low}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.DerivedSessionIDMetadataKey: "stable-session",
	}}

	assertPick := func(label string, pickOpts cliproxyexecutor.Options, wantID string) {
		t.Helper()
		got, errPick := selector.Pick(context.Background(), "test", "model", pickOpts, auths)
		if errPick != nil {
			t.Fatalf("%s: %v", label, errPick)
		}
		if got == nil {
			t.Fatalf("%s = nil, want %q", label, wantID)
		}
		if got.ID != wantID {
			t.Fatalf("%s = %q, want %q", label, got.ID, wantID)
		}
	}

	assertPick("cold binding", opts, high.ID)
	assertPick("no-session fallback", cliproxyexecutor.Options{}, high.ID)

	high.Unavailable = true
	assertPick("fallback after bound auth became unavailable", opts, low.ID)
}

type lastAuthSelector struct{}

func (lastAuthSelector) Pick(_ context.Context, _, _ string, _ cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if len(auths) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth candidates"}
	}
	return auths[len(auths)-1], nil
}

func expireSessionAffinityPriorityModelCooldown(t *testing.T, manager *Manager, authID, model string) {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	auth := manager.auths[authID]
	if auth == nil {
		t.Fatalf("auth %q not found", authID)
	}
	state := auth.ModelStates[model]
	if state == nil {
		t.Fatalf("model state %q not found for auth %q", model, authID)
	}
	expired := time.Now().Add(-time.Second)
	state.NextRetryAfter = expired
	state.Quota.NextRecoverAt = expired
}

func TestSessionAffinityPriorityChanges(t *testing.T) {
	for _, mode := range []string{"explicit", "derived", "lcp"} {
		for _, priority := range []string{"", "5"} {
			t.Run(mode+"/priority="+priority, func(t *testing.T) {
				selector := NewSessionAffinitySelector(&RoundRobinSelector{})
				defer selector.Stop()
				first := &Auth{ID: "a", Attributes: map[string]string{"priority": priority}}
				second := &Auth{ID: "b", Attributes: map[string]string{"priority": priority}}
				auths := []*Auth{first, second}
				pick := func(want string) {
					t.Helper()
					opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
					switch mode {
					case "explicit":
						opts.Headers = http.Header{"X-Session-Id": []string{"stable"}}
					case "derived":
						opts.Metadata[cliproxyexecutor.DerivedSessionIDMetadataKey] = "stable"
					case "lcp":
						opts.SourceFormat = sdktranslator.FormatOpenAI
						opts.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"stable prompt"}]}`)
						opts.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = "caller"
					}
					got, errPick := selector.Pick(context.Background(), "openai", "model", opts, auths)
					if errPick != nil || got == nil || got.ID != want {
						t.Fatalf("Pick() = %v, %v; want %s", got, errPick, want)
					}
				}

				pick(first.ID)
				pick(first.ID) // Equal or unset priorities preserve affinity despite round-robin.
				second.Attributes["priority"] = "10"
				pick(second.ID) // A priority edit overrides the existing binding immediately.
				pick(second.ID)
				first.Attributes["priority"] = "10"
				pick(second.ID) // Equalizing priorities preserves the new binding.
				second.Disabled = true
				pick(first.ID) // An unavailable preferred account still permits failover.
				second.Disabled = false
				pick(first.ID) // Equal-priority recovery retains the failover binding.
			})
		}
	}
}

func TestSessionAffinityPriorityOverridesParentBinding(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	defer selector.Stop()
	first := &Auth{ID: "a"}
	second := &Auth{ID: "b", Attributes: map[string]string{}}
	auths := []*Auth{first, second}
	parentOpts := cliproxyexecutor.Options{Headers: http.Header{"X-Task-ID": []string{"parent"}}}
	parent, errParent := selector.Pick(context.Background(), "openai", "model", parentOpts, auths)
	if errParent != nil || parent == nil || parent.ID != first.ID {
		t.Fatalf("parent Pick() = %v, %v; want %s", parent, errParent, first.ID)
	}
	second.Attributes["priority"] = "10"
	childOpts := cliproxyexecutor.Options{Headers: http.Header{
		"X-Task-ID":        []string{"child"},
		"X-Parent-Task-ID": []string{"parent"},
	}}
	child, errChild := selector.Pick(context.Background(), "openai", "model", childOpts, auths)
	if errChild != nil || child == nil || child.ID != second.ID {
		t.Fatalf("child Pick() = %v, %v; want higher-priority %s", child, errChild, second.ID)
	}
}
