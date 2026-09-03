package knowledge

import (
	"context"
	"errors"
	"testing"
)

func TestValidateGitSourceURL_RejectsPrivateHosts(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	blocked := []string{
		"http://127.0.0.1:8080/repo.git",
		"http://127.255.255.255/repo.git",
		"http://10.0.0.1/internal-repo.git",
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.0.1/latest/meta-data/",
		"http://192.168.1.1/sensitive-path",
		"http://172.16.0.1/repo.git",
		"http://172.31.255.255/repo.git",
		"https://localhost/repo.git",
		"http://[::1]/repo.git",
		"http://[::ffff:127.0.0.1]/repo.git",
		"http://[::ffff:169.254.169.254]/repo.git",
		"http://[fc00::1]/repo.git",
		"http://[fd12::1]/repo.git",
		"http://[fe80::1]/repo.git",
		"http://[fe80::1%25lo0]/repo.git",
		"http://[::]/repo.git",
		"http://0.0.0.0/repo.git",
		"http://0.1.2.3/repo.git",
		"http://2130706433/repo.git",
		"https://2130706433/repo.git",
		"http://0x7f000001/repo.git",
		"http://017700000001/repo.git",
		"https://0177.0.0.1/repo.git",
		"http://127.1/repo.git",
		"http://2852039166/repo.git",
		"http://0xA9FEA9FE/repo.git",
		"http://2886729729/repo.git",
		"http://3232235777/repo.git",
	}
	for _, u := range blocked {
		if err := ValidateGitSourceURL(u); err == nil {
			t.Errorf("expected %q to be rejected as private/internal, got nil", u)
		}
	}
}

func TestValidateGitSourceURL_AllowsPublicHosts(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	ok := []string{
		"https://github.com/hivecommons/hive.git",
		"https://gitlab.com/group/project.git",
		"https://8.8.8.8/repo.git", // public IP literal
		"https://[2001:4860:4860::8888]/repo.git",
	}
	for _, u := range ok {
		if err := ValidateGitSourceURL(u); err != nil {
			t.Errorf("expected %q to pass, got %v", u, err)
		}
	}
}

func TestValidateGitSourceURL_OptInAllowsPrivate(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	if err := ValidateGitSourceURL("http://10.0.0.5/internal.git"); err != nil {
		t.Errorf("opt-in must allow a private internal Git server, got %v", err)
	}
}

// Audit F8 (CWE-918). The SSRF check inspected the host only as an IP LITERAL,
// so every rejection test above passes while `http://internal.attacker.tld/`
// resolving to 169.254.169.254 validated cleanly — an attacker controls their
// own DNS, so a literal-only check is trivially sidestepped.
//
// The old comment justified skipping DNS as avoiding a "rebinding TOCTOU", but
// that has it backwards: resolving narrows the window to a genuine rebind race,
// while not resolving removes the check altogether. The dashboard's
// isPrivateURL already resolves and fails closed; this brings the git path in
// line.
func TestF8_RejectsHostnameResolvingToPrivateAddress(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, host string) ([]string, error) {
		if host == "internal.attacker.tld" {
			return []string{"169.254.169.254"}, nil
		}
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { gitSourceResolver = orig })

	if err := ValidateGitSourceURL("https://internal.attacker.tld/repo.git"); err == nil {
		t.Fatal("F8: a hostname resolving to the cloud metadata endpoint was accepted — the SSRF check only inspects IP literals")
	}
	// Positive control: a hostname resolving to a public address must still be
	// allowed, or the fix is just a blanket denial of every non-literal host.
	if err := ValidateGitSourceURL("https://github.com/hivecommons/hive.git"); err != nil {
		t.Fatalf("a public hostname was rejected, which would break every legitimate git source: %v", err)
	}
}

// A resolver failure must fail CLOSED, matching isPrivateURL — otherwise an
// attacker who can make lookups fail (a timeout, a hostile resolver) turns the
// error path itself into the bypass.
func TestF8_FailsClosedWhenResolverErrors(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "")
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("dns unavailable")
	}
	t.Cleanup(func() { gitSourceResolver = orig })

	if err := ValidateGitSourceURL("https://unresolvable.example/repo.git"); err == nil {
		t.Fatal("F8: a host whose DNS lookup failed was accepted — the error path must fail closed")
	}
}

// The operator opt-in must still work: a hive with a legitimately internal Git
// server sets HIVE_ALLOW_PRIVATE_GIT_SOURCE and the check steps aside.
func TestF8_OperatorOptInStillAllowsPrivate(t *testing.T) {
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")
	orig := gitSourceResolver
	gitSourceResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"10.0.0.5"}, nil
	}
	t.Cleanup(func() { gitSourceResolver = orig })

	if err := ValidateGitSourceURL("https://git.internal.corp/repo.git"); err != nil {
		t.Fatalf("operator opt-in no longer permits an internal Git server: %v", err)
	}
}
