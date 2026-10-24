package types

import "testing"

func TestFieldsDropsOddKeyNonStringKeyAndEmptyString(t *testing.T) {
	got := Fields("backend", "copilot", 7, "x", "model", "", "trigger", "governor", "dangling")
	want := map[string]any{"backend": "copilot", "trigger": "governor"}
	if len(got) != len(want) {
		t.Fatalf("Fields = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Fields[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestFieldsKeepsNonStringValues(t *testing.T) {
	got := Fields("count", 3, "ok", true)
	if got["count"] != 3 || got["ok"] != true {
		t.Fatalf("Fields = %v, want count=3 ok=true", got)
	}
}

func TestPRResultIsDuplicate(t *testing.T) {
	cases := []struct {
		name string
		r    PRResult
		want bool
	}{
		{"fresh", PRResult{Number: 1}, false},
		{"same head branch", PRResult{AlreadyExisted: true}, false},
		{"same tree", PRResult{AlreadyExisted: true, DuplicateTree: true}, true},
		{"guard redirected", PRResult{AlreadyExisted: true, DuplicateReason: "issue"}, true},
	}
	for _, tc := range cases {
		if got := tc.r.IsDuplicate(); got != tc.want {
			t.Errorf("%s: IsDuplicate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgentTokenCachePath(t *testing.T) {
	if got, want := AgentTokenCachePath(DefaultAgentTokenCacheDir, "guide"), "/var/run/hive-metrics/agent-tokens/gh-token-guide.cache"; got != want {
		t.Fatalf("AgentTokenCachePath = %q, want %q", got, want)
	}
	if got, want := AgentTokenCachePath("/tmp/x", "a"), "/tmp/x/gh-token-a.cache"; got != want {
		t.Fatalf("AgentTokenCachePath = %q, want %q", got, want)
	}
}
