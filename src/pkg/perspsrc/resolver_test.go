package perspsrc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

type stubFetcher struct {
	docs  []string
	errs  []error
	calls int
}

func (f *stubFetcher) GetFileContentRef(_ context.Context, _, _, _, _ string) (string, error) {
	i := f.calls
	f.calls++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if err != nil {
		return "", err
	}
	if i < len(f.docs) {
		return f.docs[i], nil
	}
	return "", errors.New("no document")
}

type recLogger struct{ warns int }

func (l *recLogger) Warn(string, ...any) { l.warns++ }
func (l *recLogger) Info(string, ...any) {}

func allowAll(string) bool { return true }

var testSource = config.DefinitionSourceConfig{Owner: "torch-spyre", Repo: "spyre-inference", Path: ".hive/review-perspectives.yaml"}

const goodDoc = `kind: ReviewPerspectives
perspectives:
  - name: security
  - name: spyre-numerics
    focus: "Silent CPU fallbacks."
`

// A custom perspective without focus text fails NewPerspectiveSet validation.
const invalidDoc = `kind: ReviewPerspectives
perspectives:
  - name: correctness
  - name: undescribed-custom
`

func TestResolverDeniedNotAllowlisted(t *testing.T) {
	f := &stubFetcher{docs: []string{goodDoc}}
	log := &recLogger{}
	r := NewResolver(f, func(string) bool { return false }, log)
	res := r.Resolve(context.Background(), testSource)
	if res.Ok || res.Source != "denied" {
		t.Fatalf("Resolve = %+v, want Ok=false Source=denied", res)
	}
	if f.calls != 0 {
		t.Errorf("fetcher called %d times for a denied repo, want 0", f.calls)
	}
	if log.warns == 0 {
		t.Error("expected a Warn log for a denied repo")
	}

	// A nil allow fails closed.
	if res := NewResolver(f, nil, nil).Resolve(context.Background(), testSource); res.Ok || res.Source != "denied" {
		t.Errorf("nil allow Resolve = %+v, want denied", res)
	}
}

func TestResolverNoClientUsesCache(t *testing.T) {
	r := NewResolver(nil, allowAll, nil)
	if res := r.Resolve(context.Background(), testSource); res.Ok || res.Source != "no-client" {
		t.Fatalf("empty-cache Resolve = %+v, want Ok=false Source=no-client", res)
	}

	r.store(testSource, goodDoc)
	res := r.Resolve(context.Background(), testSource)
	if !res.Ok || res.Source != "github-cache:torch-spyre/spyre-inference" {
		t.Fatalf("cached Resolve = %+v, want Ok=true from github-cache", res)
	}
	if want := []string{"security", "spyre-numerics"}; !reflect.DeepEqual(res.Names, want) {
		t.Errorf("Names = %v, want %v", res.Names, want)
	}
}

func TestResolverFetchErrorKeepsLastKnownGood(t *testing.T) {
	f := &stubFetcher{docs: []string{goodDoc}, errs: []error{nil, errors.New("boom")}}
	r := NewResolver(f, allowAll, nil)
	first := r.Resolve(context.Background(), testSource)
	if !first.Ok || first.Source != "github:torch-spyre/spyre-inference@default" {
		t.Fatalf("first Resolve = %+v, want Ok from github", first)
	}
	second := r.Resolve(context.Background(), testSource)
	if !second.Ok || second.Source != "github-cache:torch-spyre/spyre-inference" {
		t.Fatalf("second Resolve = %+v, want Ok from github-cache", second)
	}
	if !reflect.DeepEqual(second.Names, first.Names) || !reflect.DeepEqual(second.Focus, first.Focus) {
		t.Errorf("second = %v %v, want last-known-good %v %v", second.Names, second.Focus, first.Names, first.Focus)
	}

	noCache := NewResolver(&stubFetcher{errs: []error{errors.New("boom")}}, allowAll, nil)
	if res := noCache.Resolve(context.Background(), testSource); res.Ok || res.Source != "error" {
		t.Errorf("no-cache fetch error Resolve = %+v, want Ok=false Source=error", res)
	}
}

func TestResolverMalformedKeepsLastKnownGoodAndWarns(t *testing.T) {
	f := &stubFetcher{docs: []string{goodDoc, invalidDoc}}
	log := &recLogger{}
	r := NewResolver(f, allowAll, log)
	first := r.Resolve(context.Background(), testSource)
	if !first.Ok || first.Warning != "" {
		t.Fatalf("first Resolve = %+v, want Ok with no warning", first)
	}
	second := r.Resolve(context.Background(), testSource)
	if !second.Ok {
		t.Fatalf("second Resolve = %+v, want last-known-good kept", second)
	}
	if !reflect.DeepEqual(second.Names, first.Names) {
		t.Errorf("Names = %v, want last-known-good %v", second.Names, first.Names)
	}
	if second.Warning == "" {
		t.Error("expected a Warning for the malformed document")
	}
	if log.warns == 0 {
		t.Error("expected a Warn log for the malformed document")
	}
}

func TestResolverMalformedNoCacheFallsBackToInline(t *testing.T) {
	r := NewResolver(&stubFetcher{docs: []string{invalidDoc}}, allowAll, nil)
	res := r.Resolve(context.Background(), testSource)
	if res.Ok {
		t.Fatalf("Resolve = %+v, want Ok=false", res)
	}
	if res.Warning == "" {
		t.Error("expected a Warning for the malformed document")
	}

	cfg := &config.Config{}
	cfg.Review.Perspectives = []string{"correctness"}
	src := testSource
	cfg.Review.PerspectivesSource = &src
	ApplyToConfig(context.Background(), cfg, NewResolver(&stubFetcher{docs: []string{invalidDoc}}, allowAll, nil), nil)
	if cfg.Review.LinkedPerspectives != nil {
		t.Fatalf("LinkedPerspectives = %+v, want nil", cfg.Review.LinkedPerspectives)
	}
	if names, _ := cfg.Review.EffectivePerspectives(); !reflect.DeepEqual(names, []string{"correctness"}) {
		t.Errorf("EffectivePerspectives names = %v, want inline [correctness]", names)
	}
}

func TestApplyToConfigRecordsProvenance(t *testing.T) {
	cfg := &config.Config{}
	src := testSource
	cfg.Review.PerspectivesSource = &src
	ApplyToConfig(context.Background(), cfg, NewResolver(&stubFetcher{docs: []string{goodDoc}}, allowAll, nil), nil)
	lp := cfg.Review.LinkedPerspectives
	if lp == nil {
		t.Fatal("LinkedPerspectives = nil, want resolved set")
	}
	if lp.Source != "github:torch-spyre/spyre-inference@default" || lp.Warning != "" {
		t.Errorf("LinkedPerspectives = %+v, want github provenance and no warning", lp)
	}
	if names, _ := cfg.Review.EffectivePerspectives(); !reflect.DeepEqual(names, []string{"security", "spyre-numerics"}) {
		t.Errorf("EffectivePerspectives names = %v, want linked set", names)
	}
}
