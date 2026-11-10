package adminmcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// auto_merge.update and auto_merge.human_merge_pin_only: the pin-only
// exception to human_merge_paths (#16377), set one repository at a time.

// pinOnlyFixture is autoMergeFixture on a hive that reports
// human_merge_pin_only, with an exception already set for org/a.
func pinOnlyFixture() map[string]any {
	f := autoMergeFixture()
	f["human_merge_pin_only"] = map[string]any{
		"org/a": map[string]any{"enabled": true, "paths": []string{".github/workflows/*.yml", "Containerfile"}, "kinds": []string{"action-sha", "image-digest"}, "trusted_authors": []string{"renovate[bot]"}},
	}
	return f
}

func pinOnlyArg(paths, kinds, authors []any) map[string]any {
	return map[string]any{"enabled": true, "paths": paths, "kinds": kinds, "trusted_authors": authors}
}

// Setting one repository's exception sends the whole map back with every
// other repository's entry unchanged, and says what will now merge on its own.
func TestAutoMergeUpdatePinOnlySetKeepsOtherRepos(t *testing.T) {
	p := &autoMergeProvider{settings: pinOnlyFixture()}
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, map[string]any{
		"human_merge_pin_only": map[string]any{"b": pinOnlyArg([]any{"CODEOWNERS", " CODEOWNERS "}, []any{"version-pin"}, []any{"dependabot[bot]"})},
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
		return map[string]any{"human_merge_pin_only": map[string]any{"org/a": pinOnlyArg(paths, kinds, authors)}}
	}
	paths, kinds, ra := []any{".github/workflows/*.yml", "Containerfile"}, []any{"action-sha", "image-digest"}, []any{"renovate[bot]"}
	for name, tc := range map[string]struct {
		args  map[string]any
		widen bool
		names string
	}{
		"new repository":  {map[string]any{"human_merge_pin_only": map[string]any{"org/b": pinOnlyArg([]any{"CODEOWNERS"}, []any{"action-sha"}, ra)}}, true, "human_merge_pin_only[org/b] turned on"},
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
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: pinOnlyFixture()}), tc.args)
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
	p := &autoMergeProvider{settings: pinOnlyFixture()}
	h := autoMergeHandler(p)
	_, id := autoMergePreview(t, h, map[string]any{
		"human_merge_pin_only": map[string]any{"org/b": pinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, []any{"dependabot[bot]"})},
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
		"repository without human-merge paths": {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/c": pinOnlyArg([]any{"Containerfile"}, []any{"image-digest"}, []any{"renovate[bot]"})}}, "org/c has no human_merge_paths"},
		"enabled without trusted authors":      {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": pinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, nil)}}, "enabled policy requires nonempty trusted_authors"},
		"unknown kind":                         {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": pinOnlyArg([]any{"CODEOWNERS"}, []any{"any-line"}, []any{"renovate[bot]"})}}, `unknown kind "any-line"`},
		"unknown entry field":                  {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": map[string]any{"enabled": true, "authors": []any{"x"}}}}, `not "authors"`},
		"enabled not given":                    {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": map[string]any{"paths": []any{"CODEOWNERS"}}}}, "enabled must be true or false"},
		"remove what is not there":             {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": nil}}, "no entry for org/b to remove"},
		"same entry, other casing":             {pinOnlyFixture(), map[string]any{"human_merge_pin_only": map[string]any{"ORG/A": pinOnlyArg([]any{".github/workflows/*.yml", "Containerfile"}, []any{"action-sha", "image-digest"}, []any{"renovate[bot]"})}}, "nothing would change"},
		// A hive whose endpoint has no human_merge_pin_only field would drop
		// it silently, so it is refused rather than sent.
		"hive that does not report it": {autoMergeFixture(), map[string]any{"human_merge_pin_only": map[string]any{"org/b": pinOnlyArg([]any{"CODEOWNERS"}, []any{"version-pin"}, []any{"dependabot[bot]"})}}, "may run an older build"},
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
	p := &autoMergeProvider{settings: pinOnlyFixture()}
	preview, _ := autoMergePreview(t, autoMergeHandler(p), map[string]any{
		"human_merge_paths":    map[string]any{"org/c": []any{"Containerfile"}},
		"human_merge_pin_only": map[string]any{"org/c": pinOnlyArg([]any{"Containerfile"}, []any{"image-digest"}, []any{"renovate[bot]"})},
	})
	if !strings.Contains(preview.WideningDisclosure, "human_merge_pin_only[org/c] turned on") {
		t.Fatalf("disclosure = %s", preview.WideningDisclosure)
	}
}
