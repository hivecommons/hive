package proxy

import (
	"reflect"
	"testing"
	"time"
)

func TestInferenceAuthErrorNilReceivers(t *testing.T) {
	var p *GitHubProxy
	if msg, since := p.InferenceAuthError(); msg != "" || !since.IsZero() {
		t.Fatalf("nil proxy InferenceAuthError = (%q, %v), want empty", msg, since)
	}

	p = &GitHubProxy{}
	if msg, since := p.InferenceAuthError(); msg != "" || !since.IsZero() {
		t.Fatalf("nil tracker InferenceAuthError = (%q, %v), want empty", msg, since)
	}
}

func TestParseTeam403ModelsMalformedTeamDenial(t *testing.T) {
	body := "team not allowed to access model, but response omitted the models list"
	if got := parseTeam403Models(body); got != nil {
		t.Fatalf("malformed team denial parsed models %v, want nil", got)
	}
}

func TestEntitledModelsFromKeyInfoInvalidAndAllTeamWildcard(t *testing.T) {
	if got := entitledModelsFromKeyInfo([]byte("{")); got != nil {
		t.Fatalf("invalid key-info body parsed models %v, want nil", got)
	}
	if got := entitledModelsFromKeyInfo([]byte(`{"models":["all-team-models","Azure/gpt-4o"]}`)); got != nil {
		t.Fatalf("all-team wildcard parsed models %v, want nil", got)
	}

	want := []string{"Azure/gpt-4o"}
	if got := sanitizeEntitledModels([]string{"", "  Azure/gpt-4o  "}); !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitizeEntitledModels = %v, want %v", got, want)
	}
}

func TestEntitlementStoreGetFreshness(t *testing.T) {
	store := newEntitlementStore()
	models := []string{"Azure/gpt-4o"}
	store.set("https://litellm.example", "sk-test", models, "unit")

	got, source, ok, stale := store.get("https://litellm.example")
	if !ok || stale || source != "unit" || !reflect.DeepEqual(got, models) {
		t.Fatalf("fresh get = (%v, %q, %v, %v), want (%v, unit, true, false)",
			got, source, ok, stale, models)
	}

	key := normalizeEndpoint("https://litellm.example")
	store.mu.Lock()
	entry := store.byEndpt[key]
	entry.at = time.Now().Add(-(entitlementTTL + time.Second))
	store.byEndpt[key] = entry
	store.mu.Unlock()

	_, _, ok, stale = store.get("https://litellm.example")
	if !ok || !stale {
		t.Fatalf("expired get ok=%v stale=%v, want true/true", ok, stale)
	}
}
