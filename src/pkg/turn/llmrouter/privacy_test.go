package llmrouter

import (
	"context"
	"errors"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPrivateRepoSkipsTrainingProviders: with WithPrivateRepo(true) a
// provider marked TrainsOnInputs never receives a request, even when it is
// first in the list, and the skip is reported.
func TestPrivateRepoSkipsTrainingProviders(t *testing.T) {
	trainer := &fakeProvider{handler: okText("from trainer")}
	clean := &fakeProvider{handler: okText("from clean")}
	a := prov("a", trainer.server(t).URL)
	a.TrainsOnInputs = true
	b := prov("b", clean.server(t).URL)
	var seen []Attempt
	r, err := New([]Provider{a, b}, WithEnv(allKeys()), WithPrivateRepo(true), WithAttemptHook(func(x Attempt) { seen = append(seen, x) }))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Generate(context.Background(), msgs(), nil)
	if err != nil || resp.Content != "from clean" {
		t.Fatalf("Generate = %+v %v", resp, err)
	}
	if trainer.calls != 0 || clean.calls != 1 {
		t.Fatalf("calls: trainer %d, clean %d", trainer.calls, clean.calls)
	}
	if len(seen) != 2 || seen[0].Provider != "a" || seen[0].Outcome != OutcomeSkipped {
		t.Fatalf("attempts = %+v", seen)
	}
}

// TestTrainingProvidersServeWhenAllowedOrPublic: a public repository, or a
// private one with the explicit opt-in, may use a training provider.
func TestTrainingProvidersServeWhenAllowedOrPublic(t *testing.T) {
	for name, opts := range map[string][]Option{
		"public":          {WithPrivateRepo(false)},
		"default":         nil,
		"private opt-in":  {WithPrivateRepo(true), WithAllowTrainingProvidersForPrivate(true)},
		"opt-in (public)": {WithAllowTrainingProvidersForPrivate(true)},
	} {
		trainer := &fakeProvider{handler: okText("from trainer")}
		a := prov("a", trainer.server(t).URL)
		a.TrainsOnInputs = true
		r, err := New([]Provider{a}, append(opts, WithEnv(allKeys()))...)
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}
		if resp, err := r.Generate(context.Background(), msgs(), nil); err != nil || resp.Content != "from trainer" || trainer.calls != 1 {
			t.Fatalf("%s: Generate = %+v %v (calls %d)", name, resp, err, trainer.calls)
		}
	}
}

// TestPrivateRepoWithOnlyTrainingProvidersFailsAtNew: rather than a
// capacity error that a caller would retry forever.
func TestPrivateRepoWithOnlyTrainingProvidersFailsAtNew(t *testing.T) {
	a := prov("a", "https://a.example")
	a.TrainsOnInputs = true
	b := prov("b", "https://b.example")
	b.TrainsOnInputs = true
	if _, err := New([]Provider{a, b}, WithPrivateRepo(true)); !errors.Is(err, ErrOnlyTrainingProviders) {
		t.Fatalf("New = %v, want ErrOnlyTrainingProviders", err)
	}
	if _, err := New([]Provider{a, b}, WithPrivateRepo(true), WithAllowTrainingProvidersForPrivate(true)); err != nil {
		t.Fatalf("opt-in New = %v", err)
	}
}

func TestTrainsOnInputsYAML(t *testing.T) {
	var p Provider
	if err := yaml.Unmarshal([]byte("trains_on_inputs: true\n"), &p); err != nil || !p.TrainsOnInputs {
		t.Fatalf("yaml = %+v %v", p, err)
	}
}
