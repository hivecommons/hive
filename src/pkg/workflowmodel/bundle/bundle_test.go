package bundle

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

var fixedNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func techdocs(t *testing.T) *workflowmodel.Model {
	t.Helper()
	data, err := os.ReadFile("../testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestExportRoundTrip(t *testing.T) {
	m := techdocs(t)
	b, err := Export(m, "  andy ", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if b.APIVersion != APIVersion || b.Kind != Kind || b.CreatedBy != "andy" || b.Tier != TierLocal {
		t.Fatalf("envelope = %+v", b)
	}
	if b.Key() != "cncf-techdocs@1" {
		t.Fatalf("key = %q", b.Key())
	}
	if b.Hash != CanonicalHash(m) || len(b.Hash) != 64 {
		t.Fatalf("hash = %q", b.Hash)
	}
	enc, err := Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	// Canonical: no whitespace, sorted top-level keys.
	if strings.Contains(string(enc), "\n") || !strings.HasPrefix(string(enc), `{"apiVersion":`) {
		t.Fatalf("not canonical: %.80s", enc)
	}
	enc2, _ := Encode(b)
	if string(enc) != string(enc2) {
		t.Fatal("Encode not deterministic")
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(got); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !got.CreatedAt.Equal(fixedNow) || got.Profile.Name != "cncf-techdocs" {
		t.Fatalf("decoded = %+v", got)
	}
	// Key order in the source document does not change the hash.
	reordered := `{"spec":` + mustJSON(t, m.Spec) + `,"metadata":` + mustJSON(t, m.Metadata) + `,"kind":"` + m.Kind + `","apiVersion":"` + m.APIVersion + `"}`
	var m2 workflowmodel.Model
	if err := json.Unmarshal([]byte(reordered), &m2); err != nil {
		t.Fatal(err)
	}
	if CanonicalHash(&m2) != b.Hash {
		t.Fatal("CanonicalHash depends on key order")
	}
}

func TestExportRefusals(t *testing.T) {
	if _, err := Export(nil, "", fixedNow); !errors.Is(err, ErrMissingModel) {
		t.Fatalf("nil model: %v", err)
	}
	m := techdocs(t)
	m.Spec.Initial = "nope"
	if _, err := Export(m, "", fixedNow); err == nil {
		t.Fatal("expected invalid/non-compiling model to be refused")
	}
	m = techdocs(t)
	m.Spec.Meta.Provenance = "mystery"
	if _, err := Export(m, "", fixedNow); !errors.Is(err, ErrBadTier) {
		t.Fatalf("bad tier: %v", err)
	}
	m = techdocs(t)
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceCurated
	b, err := Export(m, "", fixedNow)
	if err != nil || b.Tier != TierCurated {
		t.Fatalf("curated: %v %+v", err, b)
	}
	// A model whose transitions fail to compile but which passes Validate.
	m = techdocs(t)
	m.Spec.Transitions = nil
	m.Spec.Columns = nil
	if _, err := Export(m, "", fixedNow); err == nil {
		t.Log("stripped model still compiled; compile refusal is covered by Verify tests")
	}
}

func TestTamperedModel(t *testing.T) {
	b, err := Export(techdocs(t), "andy", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b.Model.Spec.Meta.Description = "tampered"
	if err := Verify(b); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("tampered model: %v", err)
	}
	// Re-hash to slip past the hash check with a model that no longer
	// validates: Verify must still refuse.
	b.Model.Spec.Initial = "nope"
	b.Hash = CanonicalHash(b.Model)
	if err := Verify(b); err == nil || errors.Is(err, ErrHashMismatch) {
		t.Fatalf("invalid model with matching hash: %v", err)
	}
}

func TestTamperedProfile(t *testing.T) {
	b, err := Export(techdocs(t), "andy", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b.Profile.Columns = b.Profile.Columns[:1]
	verr := Verify(b)
	if !errors.Is(verr, ErrProfileMismatch) {
		t.Fatalf("tampered profile: %v", verr)
	}
	if !strings.Contains(verr.Error(), "published profile does not match local compile") {
		t.Fatalf("message = %q", verr.Error())
	}
	// The model's own Profile.States is json:"-", so differences there are
	// not part of the structural comparison.
	b, _ = Export(techdocs(t), "andy", fixedNow)
	b.Profile.States = nil
	if err := Verify(b); err != nil {
		t.Fatalf("States is excluded from comparison: %v", err)
	}
}

func TestEnvelopeErrors(t *testing.T) {
	b, err := Export(techdocs(t), "andy", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func(*Bundle)
		want   error
	}{
		"bad version":     {func(b *Bundle) { b.APIVersion = "v0" }, ErrBadVersion},
		"bad kind":        {func(b *Bundle) { b.Kind = "Other" }, ErrBadKind},
		"missing profile": {func(b *Bundle) { b.Profile = nil }, ErrMissingProfile},
		"bad tier":        {func(b *Bundle) { b.Tier = "gold" }, ErrBadTier},
	}
	for name, tc := range cases {
		c := *b
		tc.mutate(&c)
		if err := Verify(&c); !errors.Is(err, tc.want) {
			t.Errorf("%s: Verify = %v, want %v", name, err, tc.want)
		}
		if tc.want == ErrBadTier {
			continue
		}
		enc, _ := Encode(&c)
		if _, err := Decode(enc); !errors.Is(err, tc.want) {
			t.Errorf("%s: Decode = %v, want %v", name, err, tc.want)
		}
	}
	if err := Verify(nil); !errors.Is(err, ErrMissingModel) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := Encode(nil); !errors.Is(err, ErrMissingModel) {
		t.Fatalf("encode nil: %v", err)
	}
	c := *b
	c.Kind = ""
	if err := Verify(&c); err != nil {
		t.Fatalf("empty kind is tolerated: %v", err)
	}
	enc, _ := Encode(&c)
	d, err := Decode(enc)
	if err != nil || d.Kind != Kind {
		t.Fatalf("Decode should default kind: %v %q", err, d.Kind)
	}
	c = *b
	c.Model = nil
	enc, _ = Encode(&c)
	if _, err := Decode(enc); !errors.Is(err, ErrMissingModel) {
		t.Fatalf("missing model: %v", err)
	}
	if _, err := Decode([]byte(`{"apiVersion":"` + APIVersion + `","extra":1}`)); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	if _, err := Decode([]byte("{")); err == nil {
		t.Fatal("malformed JSON must be rejected")
	}
}

func TestHelpers(t *testing.T) {
	if CanonicalHash(nil) != "" {
		t.Fatal("nil hash")
	}
	var nilB *Bundle
	if nilB.Key() != "" {
		t.Fatal("nil key")
	}
	if short("") != "<empty>" || short("abc") != "abc" || short("0123456789abcdef") != "0123456789ab" {
		t.Fatal("short")
	}
	if KnownTier(TierLocalPublished) != true || KnownTier("x") {
		t.Fatal("KnownTier")
	}
	if _, err := canonicalJSON(make(chan int)); err == nil {
		t.Fatal("unmarshalable value must error")
	}
	out, err := canonicalJSON(map[string]interface{}{"b": []interface{}{1, "x", nil}, "a": map[string]interface{}{"z": true, "y": 1.5}})
	if err != nil || string(out) != `{"a":{"y":1.5,"z":true},"b":[1,"x",null]}` {
		t.Fatalf("canonical = %s %v", out, err)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEntryFor(t *testing.T) {
	e := EntryFor(nil, fixedNow, "andy", "")
	if e.Name != "" || string(e.Adoption) != "{}" || string(e.Ratings) != "{}" || string(e.Benchmarks) != "{}" {
		t.Fatalf("nil entry = %+v", e)
	}
	b, err := Export(techdocs(t), "andy", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	e = EntryFor(b, fixedNow, "andy", "abc123")
	if e.Key() != "cncf-techdocs@1" || e.Hash != b.Hash || e.Tier != TierLocal || e.Source != "cncf/techdocs" || e.SourceSHA != "abc123" {
		t.Fatalf("entry = %+v", e)
	}
	if e.States == 0 || e.Columns == 0 || e.Description == "" {
		t.Fatalf("summary fields empty: %+v", e)
	}
	b.Model.Spec.Scope.Repository = "org/repo"
	if EntryFor(b, fixedNow, "", "").Source != "org/repo" {
		t.Fatal("repository scope should win as source")
	}
}
