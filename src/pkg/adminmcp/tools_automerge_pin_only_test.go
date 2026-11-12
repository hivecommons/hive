package adminmcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// auto_merge.update and auto_merge.human_merge_pin_only: the pin-only
// exception to human_merge_paths (#16377), set one repository at a time.

// autoMergePinOnlyFixture is autoMergeFixture on a hive that reports
// human_merge_pin_only, with an exception already set for org/a.
func autoMergePinOnlyFixture() map[string]any {
	f := autoMergeFixture()
	f["human_merge_pin_only"] = map[string]any{
		"org/a": map[string]any{"enabled": true, "paths": []string{".github/workflows/*.yml", "Containerfile"}, "kinds": []string{"action-sha", "image-digest"}, "trusted_authors": []string{"renovate[bot]"}},
	}
	return f
}

func autoMergePinOnlyArg(paths, kinds, authors []any) map[string]any {
	return map[string]any{"enabled": true, "paths": paths, "kinds": kinds, "trusted_authors": authors}
}

// Setting one repository's exception sends the whole map back with every
// other repository's entry unchanged, and says what will now merge on its own.
func TestAutoMergeUpdatePinOnlySetKeepsOtherRepos(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePinOnlyFixture()}
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, map[string]any{
		"human_merge_pin_only": map[string]any{"b": autoMergePinOnlyArg([]any{"CODEOWNERS", " CODEOWNERS "}, []any{"version-pin"}, []any{"dependabot[bot]"})},
	})
	want := `{"human_merge_pin_only":{` +
		`"org/a":{"enabled":true,"kinds":["action-sha","image-digest"],"paths":[".github/workflows/*.yml","Containerfile"],"trusted_authors":["renovate[bot]"]},` +
		`"org/b":{"enabled":true,"kinds":["version-pin"],"paths":["CODEOWNERS"],"trusted_authors":["dependabot[bot]"]}}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 1 || changes[0]["field"] != "human_merge_pin_only[org/b]" || changes[0]["before"] != nil {
		t.Fatalf("changes = %+v, want one row for org/b from nothing", changes)
	}
	for _, s := range []string{"Widening:", "pin bumps", "CODEOWNERS", "in org/b", "dependabot[bot]", "will merge without a hand merge"} {
		if !strings.Contains(preview.WideningDisclosure, s) {
			t.Fatalf("disclosure missing %q: %s", s, preview.WideningDisclosure)
		}
	}
	if strings.Contains(preview.WideningDisclosure, "org/a") {
		t.Fatalf("the untouched org/a entry is disclosed as a change: %s", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
}

// Turning an exception on or giving it more paths, kinds or authors lets more
// pull requests past a human-merge hold. Taking any of those away does not.
func TestAutoMergeUpdatePinOnlyWidening(t *testing.T) {
	a := func(paths, kinds, authors []any) map[string]any {
		return map[string]any{"human_merge_pin_only": map[string]any{"org/a": autoMergePinOnlyArg(paths, kinds, authors)}}
	}
	paths, kinds, ra := []any{".github/workflows/*.yml", "Containerfile"}, []any{"action-sha", "image-digest"}, []any{"renovate[bot]"}
	for name, tc := range map[string]struct {
		args  map[string]any
		widen bool
		names string
	}{
		"new repository":  {map[string]any{"human_merge_pin_only": map[string]any{"org/b": autoMergePinOnlyArg([]any{"CODEOWNERS"}, []any{"action-sha"}, ra)}}, true, "human_merge_pin_only[org/b] turned on"},
		"adds a path":     {a(append([]any{".claude/hooks/*.sh"}, paths...), kinds, ra), true, "paths .claude/hooks/*.sh"},
		"adds a kind":     {a(paths, []any{"action-sha", "image-digest", "version-pin"}, ra), true, "kinds version-pin"},
		"adds an author":  {a(paths, kinds, []any{"renovate[bot]", "someone"}), true, "trusted_authors someone"},
		"swaps a path":    {a([]any{"Dockerfile", "Containerfile"}, kinds, ra), true, "paths Dockerfile"},
		"removes a path":  {a([]any{"Containerfile"}, kinds, ra), false, ""},
		"removes a kind":  {a(paths, []any{"image-digest"}, ra), false, ""},
		"disabled":        {map[string]any{"human_merge_pin_only": map[string]any{"org/a": map[string]any{"enabled": false, "paths": paths, "kinds": kinds, "trusted_authors": ra}}}, false, ""},
		"unset with null": {map[string]any{"human_merge_pin_only": map[string]any{"org/a": nil}}, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: autoMergePinOnlyFixture()}), tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %s", got, tc.widen, preview.WideningDisclosure)
			}
			if tc.widen && !strings.Contains(preview.WideningDisclosure, tc.names) {
				t.Fatalf("disclosure does not name %q: %s", tc.names, preview.WideningDisclosure)
			}
			if tc.widen && !strings.Contains(preview.WideningDisclosure, "will merge without a hand merge") {
				t.Fatalf("disclosure does not say what now merges on its own: %s", preview.WideningDisclosure)
			}
		})
	}
}

// The change is merged into the exceptions the preview read. If those change
// before write_confirm, sending the merged map would undo that change, so the
// confirm is refused and nothing is written.
func TestAutoMergeUpdatePinOnlyRefusesStalePreview(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePinOnlyFixture()}
	h := autoMergeHandler(p)
	_, id := autoMergePreview(t, h, map[string]any{
		"human_merge_pin_only": map[string]any{"org/b": autoMergePinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, []any{"dependabot[bot]"})},
	})
	p.settings["human_merge_pin_only"] = map[string]any{} // someone removed org/a's exception meanwhile
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}
}

// What the endpoint would refuse is refused at preview, with no confirmation.
func TestAutoMergeUpdatePinOnlyRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		args     map[string]any
		want     string
	}{
		"repository without human-merge paths": {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/c": autoMergePinOnlyArg([]any{"Containerfile"}, []any{"image-digest"}, []any{"renovate[bot]"})}}, "org/c has no human_merge_paths"},
		"enabled without trusted authors":      {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": autoMergePinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, nil)}}, "enabled policy requires nonempty trusted_authors"},
		"unknown kind":                         {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": autoMergePinOnlyArg([]any{"CODEOWNERS"}, []any{"any-line"}, []any{"renovate[bot]"})}}, `unknown kind "any-line"`},
		"unknown entry field":                  {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": map[string]any{"enabled": true, "authors": []any{"x"}}}}, `not "authors"`},
		"enabled not given":                    {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": map[string]any{"paths": []any{"CODEOWNERS"}}}}, "enabled must be true or false"},
		"remove what is not there":             {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": nil}}, "no entry for org/b to remove"},
		"same entry, other casing":             {autoMergePinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"ORG/A": autoMergePinOnlyArg([]any{".github/workflows/*.yml", "Containerfile"}, []any{"action-sha", "image-digest"}, []any{"renovate[bot]"})}}, "nothing would change"},
		// A hive whose endpoint has no human_merge_pin_only field would drop
		// it silently, so it is refused rather than sent.
		"hive that does not report it": {autoMergeFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": autoMergePinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, []any{"dependabot[bot]"})}}, "may run an older build"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &autoMergeProvider{settings: tc.settings}
			h := autoMergeHandler(p)
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAutoMergeUpdate, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
				t.Fatalf("a refused preview minted a confirmation: %+v", pending)
			}
		})
	}

	// Setting the human-merge paths in the same change makes it meaningful.
	p := &autoMergeProvider{settings: autoMergePinOnlyFixture()}
	preview, _ := autoMergePreview(t, autoMergeHandler(p), map[string]any{
		"human_merge_paths":    map[string]any{"org/c": []any{"Containerfile"}},
		"human_merge_pin_only": map[string]any{"org/c": autoMergePinOnlyArg([]any{"Containerfile"}, []any{"image-digest"}, []any{"renovate[bot]"})},
	})
	if !strings.Contains(preview.WideningDisclosure, "human_merge_pin_only[org/c] turned on") {
		t.Fatalf("disclosure = %s", preview.WideningDisclosure)
	}
}

// The operation's schema offers every kind the hive accepts, so a caller can
// name action-sha-ref, and says what that kind admits.
func TestAutoMergeUpdatePinOnlySchemaListsKinds(t *testing.T) {
	op, ok := DefaultWriteRegistry().Get(WriteOpAutoMergeUpdate)
	if !ok {
		t.Fatalf("%s is not registered", WriteOpAutoMergeUpdate)
	}
	props, _ := op.InputSchema()["properties"].(map[string]any)
	pin, _ := props["human_merge_pin_only"].(map[string]any)
	perRepo, _ := pin["additionalProperties"].(map[string]any)
	anyOf, _ := perRepo["anyOf"].([]any)
	if len(anyOf) == 0 {
		t.Fatalf("human_merge_pin_only schema = %#v", pin)
	}
	entry, _ := anyOf[0].(map[string]any)
	fields, _ := entry["properties"].(map[string]any)
	kinds, _ := fields["kinds"].(map[string]any)
	items, _ := kinds["items"].(map[string]any)
	enum, _ := items["enum"].([]string)
	if got, want := strings.Join(enum, ","), strings.Join(config.PinOnlyKinds, ","); got != want || got != "action-sha,image-digest,version-pin,action-sha-ref" {
		t.Fatalf("kinds enum = %q, config accepts %q", got, want)
	}
	help, _ := kinds["description"].(string)
	for _, want := range append([]string{"same old and new commit as a uses: line", "needs action-sha"}, config.PinOnlyKinds...) {
		if !strings.Contains(help, want) {
			t.Fatalf("kinds description does not mention %q: %s", want, help)
		}
	}
}

// Turning action-sha-ref on is widening like any other kind, and the preview
// says what it admits; on an exception without action-sha it says the kind
// has no effect. An exception that does not hold the kind is described as
// before.
func TestAutoMergeUpdatePinOnlyDescribesActionSHARef(t *testing.T) {
	paths, ra := []any{".github/workflows/*.yml", "Containerfile"}, []any{"renovate[bot]"}
	preview := func(kinds []any) string {
		t.Helper()
		p, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: autoMergePinOnlyFixture()}), map[string]any{
			"human_merge_pin_only": map[string]any{"org/a": autoMergePinOnlyArg(paths, kinds, ra)},
		})
		return p.WideningDisclosure
	}
	const admits, inert = "same old and new commit as a uses: line bumped in the same pull request", "action-sha-ref has no effect"

	got := preview([]any{"action-sha", "image-digest", "action-sha-ref"})
	for _, want := range []string{"Widening:", "kinds action-sha-ref", "pin bumps (action-sha, image-digest, action-sha-ref)", "will merge without a hand merge", admits} {
		if !strings.Contains(got, want) {
			t.Fatalf("disclosure missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, inert) {
		t.Fatalf("disclosure calls the kind inert next to action-sha: %s", got)
	}

	got = preview([]any{"image-digest", "action-sha-ref"})
	for _, want := range []string{"Widening:", "kinds action-sha-ref", admits, inert} {
		if !strings.Contains(got, want) {
			t.Fatalf("disclosure missing %q: %s", want, got)
		}
	}

	if got = preview([]any{"action-sha", "image-digest", "version-pin"}); strings.Contains(got, "action-sha-ref") {
		t.Fatalf("an exception without the kind is described with it: %s", got)
	}
}
