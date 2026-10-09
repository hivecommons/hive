package evidence

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func sample() *Bundle {
	return &Bundle{
		SchemaVersion:    SchemaVersion,
		ID:               BundleID("hivecommons/hive", 42, "headsha"),
		Repo:             "hivecommons/hive",
		Number:           42,
		Author:           Author{Login: "octocat", Kind: AuthorAgent},
		BaseSHA:          "basesha",
		HeadSHA:          "headsha",
		PreviousBundleID: BundleID("hivecommons/hive", 42, "oldsha"),
		Policy: Policy{
			ACMMLevel:          4,
			Perspectives:       []string{"correctness", "security"},
			RequireApproval:    true,
			MinPriority:        "P2",
			HumanMergePaths:    []string{".github/workflows/**"},
			SentinelConfigHash: "abc123",
		},
		Verdicts: []Verdict{{
			Perspective: "security",
			Model:       "m1",
			Backend:     "copilot",
			Verdict:     "approve",
			Confidence:  0.9,
			Findings:    []Finding{{Path: "a.go", Line: 7, Severity: "low", Summary: "<nit> & more"}},
			RecordedAt:  t0,
		}},
		PostedReviews: []PostedReview{{URL: "https://example.com/r/1", Head: "headsha", At: t0}},
		CI:            CI{Checks: []Check{{Name: "build", Conclusion: "success", URL: "https://example.com/c"}}, CapturedAt: t0},
		Sentinel:      []SentinelFinding{{Rule: "r1", Summary: "s", Paths: []string{"x"}}},
		HumanActions:  []Action{{Actor: "alice", Kind: "approve", Detail: "ok", At: t0}},
		Merge:         &MergeEvent{Actor: "hive", Method: "squash", At: t0, SHA: "mergesha"},
		CreatedAt:     t0,
		UpdatedAt:     t0,
	}
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Bundle)
		wantErr string
	}{
		{"valid", func(*Bundle) {}, ""},
		{"valid minimal", func(b *Bundle) {
			b.Verdicts, b.PostedReviews, b.Sentinel, b.HumanActions, b.Merge = nil, nil, nil, nil, nil
			b.CI.Checks = nil
		}, ""},
		{"schema version", func(b *Bundle) { b.SchemaVersion = "v0" }, "schema_version"},
		{"repo no slash", func(b *Bundle) { b.Repo = "hive" }, "repo"},
		{"repo empty owner", func(b *Bundle) { b.Repo = "/hive" }, "repo"},
		{"repo empty name", func(b *Bundle) { b.Repo = "o/" }, "repo"},
		{"repo extra slash", func(b *Bundle) { b.Repo = "o/n/x" }, "repo"},
		{"number", func(b *Bundle) { b.Number = 0 }, "number"},
		{"head", func(b *Bundle) { b.HeadSHA = "" }, "head_sha"},
		{"base", func(b *Bundle) { b.BaseSHA = "" }, "base_sha"},
		{"id mismatch", func(b *Bundle) { b.ID = "other" }, "id"},
		{"author login", func(b *Bundle) { b.Author.Login = "" }, "author.login"},
		{"author kind", func(b *Bundle) { b.Author.Kind = "robot" }, "author.kind"},
		{"created", func(b *Bundle) { b.CreatedAt = time.Time{} }, "created_at"},
		{"updated", func(b *Bundle) { b.UpdatedAt = time.Time{} }, "updated_at"},
		{"verdict fields", func(b *Bundle) { b.Verdicts[0].Model = "" }, "verdicts[0]: perspective"},
		{"verdict confidence high", func(b *Bundle) { b.Verdicts[0].Confidence = 1.5 }, "confidence"},
		{"verdict confidence low", func(b *Bundle) { b.Verdicts[0].Confidence = -0.1 }, "confidence"},
		{"verdict recorded", func(b *Bundle) { b.Verdicts[0].RecordedAt = time.Time{} }, "recorded_at"},
		{"finding", func(b *Bundle) { b.Verdicts[0].Findings[0].Path = "" }, "findings[0]"},
		{"posted review", func(b *Bundle) { b.PostedReviews[0].URL = "" }, "posted_reviews[0]"},
		{"check", func(b *Bundle) { b.CI.Checks[0].Name = "" }, "ci.checks[0]"},
		{"sentinel", func(b *Bundle) { b.Sentinel[0].Rule = "" }, "sentinel[0]"},
		{"action", func(b *Bundle) { b.HumanActions[0].Actor = "" }, "human_actions[0]"},
		{"merge", func(b *Bundle) { b.Merge.SHA = "" }, "merge:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := sample()
			tc.mutate(b)
			err := b.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateNil(t *testing.T) {
	var b *Bundle
	if err := b.Validate(); err == nil {
		t.Fatal("Validate on nil bundle = nil, want error")
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	b := sample()
	b.BaseSHA, b.Number = "", 0
	err := b.Validate()
	if err == nil || !strings.Contains(err.Error(), "base_sha") || !strings.Contains(err.Error(), "number") {
		t.Fatalf("Validate() = %v, want both base_sha and number errors", err)
	}
}

func TestBundleID(t *testing.T) {
	if got, want := BundleID("o/r", 5, "abc"), "o/r#5@abc"; got != want {
		t.Fatalf("BundleID = %q, want %q", got, want)
	}
}

func TestCanonical(t *testing.T) {
	b := sample()
	got, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(got), "\n\t") || strings.Contains(string(got), `": `) || strings.Contains(string(got), ", ") {
		t.Fatalf("canonical output has whitespace: %s", got)
	}
	if !strings.Contains(string(got), `"<nit> & more"`) {
		t.Fatalf("canonical output HTML-escaped: %s", got)
	}
	if !strings.HasPrefix(string(got), `{"author":{"kind":"agent","login":"octocat"},"base_sha":"basesha","ci":`) {
		t.Fatalf("keys not sorted: %s", got)
	}
	for i := 0; i < 20; i++ {
		again, err := Canonical(sample())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(got) {
			t.Fatalf("canonical output not deterministic:\n%s\n%s", got, again)
		}
	}
}

func TestCanonicalSurvivesRoundTrip(t *testing.T) {
	b := sample()
	want, err := Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var back Bundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got, err := Canonical(&back)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("round trip changed canonical form:\n%s\n%s", want, got)
	}
}

func TestCanonicalNil(t *testing.T) {
	if _, err := Canonical(nil); err == nil {
		t.Fatal("Canonical(nil) = nil error")
	}
}

func TestHash(t *testing.T) {
	b := sample()
	h1, err := Hash(b)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h1) {
		t.Fatalf("Hash = %q, want 64 hex chars", h1)
	}
	b.Hash, b.Signature = "deadbeef", "sig"
	h2, err := Hash(b)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatal("Hash must exclude the Hash and Signature fields")
	}
	b.HeadSHA = "other"
	h3, _ := Hash(b)
	if h3 == h1 {
		t.Fatal("Hash must change when content changes")
	}
	if _, err := Hash(nil); err == nil {
		t.Fatal("Hash(nil) = nil error")
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	b := sample()
	if err := Sign(b, priv); err != nil {
		t.Fatal(err)
	}
	if !b.Signed || b.Hash == "" || b.Signature == "" {
		t.Fatalf("Sign left bundle incomplete: signed=%v hash=%q sig=%q", b.Signed, b.Hash, b.Signature)
	}
	if err := Verify(b, pub); err != nil {
		t.Fatalf("Verify = %v", err)
	}

	raw, _ := json.Marshal(b)
	var back Bundle
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := Verify(&back, pub); err != nil {
		t.Fatalf("Verify after JSON round trip = %v", err)
	}
}

func TestVerifyFailsOnMutation(t *testing.T) {
	pub, priv := newKey(t)
	mutations := map[string]func(*Bundle){
		"schema_version":     func(b *Bundle) { b.SchemaVersion = "v2" },
		"id":                 func(b *Bundle) { b.ID += "x" },
		"repo":               func(b *Bundle) { b.Repo = "evil/repo" },
		"number":             func(b *Bundle) { b.Number++ },
		"author login":       func(b *Bundle) { b.Author.Login = "mallory" },
		"author kind":        func(b *Bundle) { b.Author.Kind = AuthorHuman },
		"base_sha":           func(b *Bundle) { b.BaseSHA = "x" },
		"head_sha":           func(b *Bundle) { b.HeadSHA = "x" },
		"previous bundle":    func(b *Bundle) { b.PreviousBundleID = "" },
		"policy level":       func(b *Bundle) { b.Policy.ACMMLevel = 6 },
		"policy approval":    func(b *Bundle) { b.Policy.RequireApproval = false },
		"policy perspective": func(b *Bundle) { b.Policy.Perspectives = nil },
		"verdict":            func(b *Bundle) { b.Verdicts[0].Verdict = "reject" },
		"confidence":         func(b *Bundle) { b.Verdicts[0].Confidence = 0.1 },
		"finding line":       func(b *Bundle) { b.Verdicts[0].Findings[0].Line++ },
		"verdict dropped":    func(b *Bundle) { b.Verdicts = nil },
		"posted review":      func(b *Bundle) { b.PostedReviews[0].URL = "https://evil" },
		"ci conclusion":      func(b *Bundle) { b.CI.Checks[0].Conclusion = "failure" },
		"ci captured":        func(b *Bundle) { b.CI.CapturedAt = t0.Add(time.Second) },
		"sentinel":           func(b *Bundle) { b.Sentinel = nil },
		"human action":       func(b *Bundle) { b.HumanActions[0].Actor = "bob" },
		"merge actor":        func(b *Bundle) { b.Merge.Actor = "bob" },
		"merge removed":      func(b *Bundle) { b.Merge = nil },
		"created":            func(b *Bundle) { b.CreatedAt = t0.Add(time.Hour) },
		"updated":            func(b *Bundle) { b.UpdatedAt = t0.Add(time.Hour) },
		"signed flag":        func(b *Bundle) { b.Signed = false },
		"hash":               func(b *Bundle) { b.Hash = strings.Repeat("0", 64) },
		"signature":          func(b *Bundle) { b.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b := sample()
			if err := Sign(b, priv); err != nil {
				t.Fatal(err)
			}
			mutate(b)
			if err := Verify(b, pub); err == nil {
				t.Fatal("Verify succeeded after mutation")
			}
		})
	}
}

func TestVerifyErrors(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signed := func() *Bundle {
		b := sample()
		if err := Sign(b, priv); err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name    string
		bundle  func() *Bundle
		pub     ed25519.PublicKey
		wantErr string
	}{
		{"nil bundle", func() *Bundle { return nil }, pub, "nil bundle"},
		{"bad key length", signed, pub[:5], "public key"},
		{"unsigned", sample, pub, "not signed"},
		{"wrong key", signed, otherPub, "does not verify"},
		{"bad base64", func() *Bundle {
			b := signed()
			b.Signature = "!!!"
			return b
		}, pub, "decode signature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.bundle(), tc.pub)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Verify = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSignErrors(t *testing.T) {
	if err := Sign(nil, make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("Sign(nil bundle) = nil error")
	}
	if err := Sign(sample(), ed25519.PrivateKey("short")); err == nil {
		t.Fatal("Sign with short key = nil error")
	}
}

func TestSeal(t *testing.T) {
	pub, priv := newKey(t)

	b := sample()
	b.Signed, b.Signature = true, "stale"
	if err := Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if b.Signed || b.Signature != "" || b.Hash == "" {
		t.Fatalf("unsigned seal wrong: signed=%v sig=%q hash=%q", b.Signed, b.Signature, b.Hash)
	}
	if want, _ := Hash(b); b.Hash != want {
		t.Fatal("unsigned seal hash does not match Hash()")
	}

	b = sample()
	if err := Seal(b, priv); err != nil {
		t.Fatal(err)
	}
	if err := Verify(b, pub); err != nil {
		t.Fatalf("Verify after signed Seal = %v", err)
	}

	if err := Seal(nil, nil); err == nil {
		t.Fatal("Seal(nil, nil) = nil error")
	}
}

func TestLoadSigningKey(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	want := ed25519.NewKeyFromSeed(seed)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name    string
		path    string
		wantKey bool
		wantErr string
	}{
		{"empty path", "", false, ""},
		{"absent file", filepath.Join(dir, "missing"), false, ""},
		{"hex seed", write("hex-seed", hex.EncodeToString(seed)+"\n"), true, ""},
		{"base64 seed", write("b64-seed", base64.StdEncoding.EncodeToString(seed)), true, ""},
		{"hex private key", write("hex-priv", hex.EncodeToString(want)), true, ""},
		{"not encoded", write("junk", "not a key!"), false, "neither hex nor base64"},
		{"wrong length", write("short", hex.EncodeToString(seed[:10])), false, "want 32 or 64"},
		{"unreadable", dir, false, "read signing key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LoadSigningKey(tc.path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadSigningKey = %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantKey {
				if got != nil {
					t.Fatalf("got key %x, want nil", got)
				}
				return
			}
			if !got.Equal(want) {
				t.Fatal("loaded key differs from expected key")
			}
		})
	}
}

func TestUnsignedBundleWhenKeyAbsent(t *testing.T) {
	priv, err := LoadSigningKey(filepath.Join(t.TempDir(), "absent.key"))
	if err != nil {
		t.Fatal(err)
	}
	b := sample()
	if err := Seal(b, priv); err != nil {
		t.Fatal(err)
	}
	if b.Signed || b.Signature != "" {
		t.Fatalf("bundle with absent key must be unsigned: signed=%v sig=%q", b.Signed, b.Signature)
	}
	raw, _ := json.Marshal(b)
	if !strings.Contains(string(raw), `"signed":false`) {
		t.Fatalf("JSON missing signed:false: %s", raw)
	}
}
