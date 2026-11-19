package crux

import "testing"

// The spool's kinds (hivecommons/hive#18572, #18573): a kindless envelope is
// a review whose MAC is unchanged, so an older hive-review still verifies;
// every other kind binds its name into the MAC.
func TestSandboxSpoolMACBindsKind(t *testing.T) {
	req := []byte(`{"repo":"o/r"}`)
	review := SandboxReviewMAC("k", req)
	if SandboxSpoolMAC("k", "", req) != review || SandboxSpoolMAC("k", SandboxSpoolKindReview, req) != review {
		t.Fatal("a review's MAC must stay SandboxReviewMAC")
	}
	issue := SandboxSpoolMAC("k", SandboxSpoolKindIssue, req)
	topic := SandboxSpoolMAC("k", SandboxSpoolKindTopic, req)
	if issue == review || topic == review || issue == topic {
		t.Fatal("each kind must MAC differently")
	}
	if issue != SandboxReviewMAC("k", []byte("issue\n"+string(req))) {
		t.Error("the issue MAC is HMAC(key, \"issue\\n\" + request), as bin/hive-open-issue.sh computes")
	}
	if SandboxSpoolMAC("other", SandboxSpoolKindIssue, req) == issue {
		t.Error("the MAC must depend on the key")
	}
}

func TestNormalizeSandboxSpoolKind(t *testing.T) {
	for in, want := range map[string]string{"": SandboxSpoolKindReview, "review": SandboxSpoolKindReview, "issue": SandboxSpoolKindIssue, "topic": SandboxSpoolKindTopic} {
		if got, ok := NormalizeSandboxSpoolKind(in); !ok || got != want {
			t.Errorf("NormalizeSandboxSpoolKind(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"merge", "Issue", "pr"} {
		if _, ok := NormalizeSandboxSpoolKind(bad); ok {
			t.Errorf("NormalizeSandboxSpoolKind(%q) accepted", bad)
		}
	}
}
