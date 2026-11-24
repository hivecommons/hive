package hub

import (
	"strings"
	"testing"
	"time"
)

func TestDetectTransitionsAuditChainRegressed(t *testing.T) {
	h := func(c string) string { return strings.Repeat(c, 64) }
	cases := []struct {
		name     string
		old, new RegistryEntry
		want     bool
	}{
		{"chain advanced", RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, RegistryEntry{AuditChainSeq: 12, AuditChainHead: h("b")}, false},
		{"unchanged", RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, false},
		{"first report", RegistryEntry{}, RegistryEntry{AuditChainSeq: 3, AuditChainHead: h("a")}, false},
		{"not reported", RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, RegistryEntry{}, false},
		{"seq went backwards", RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, RegistryEntry{AuditChainSeq: 2, AuditChainHead: h("b")}, true},
		{"same seq, new hash", RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("a")}, RegistryEntry{AuditChainSeq: 10, AuditChainHead: h("b")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.old.Online, c.new.Online = true, true
			got := false
			for _, ev := range detectTransitions(&c.old, &c.new, time.Now()) {
				if ev.Kind == TimelineAuditChainRegressed {
					got = true
				}
			}
			if got != c.want {
				t.Fatalf("audit_chain_regressed recorded = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSanitizeAuditChainHead(t *testing.T) {
	good := strings.Repeat("0f", 32)
	if got := sanitizeAuditChainHead(good); got != good {
		t.Fatalf("valid head dropped: %q", got)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("G", 64), strings.Repeat("A", 64), "<script>" + strings.Repeat("a", 56)} {
		if got := sanitizeAuditChainHead(bad); got != "" {
			t.Fatalf("sanitizeAuditChainHead(%q) = %q, want empty", bad, got)
		}
	}
}
