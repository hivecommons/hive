package adminmcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// auto_merge.update and auto_merge.promotions: branch-promotion rules set one
// repository at a time. On v6 tip the operation has no promotions field, so
// every preview below is refused as an unsupported merge setting.

// autoMergePromotionsFixture is autoMergeFixture on a hive that reports
// promotions, with a main to stable rule already set for org/a.
func autoMergePromotionsFixture() map[string]any {
	f := autoMergeFixture()
	f["promotions"] = map[string]any{
		"org/a": []any{map[string]any{"source": "main", "target": "stable", "authors": []string{"github-actions[bot]"}, "head_ref_prefix": "auto/promote-"}},
	}
	return f
}

func promotionArg(source, target, prefix string, authors ...any) map[string]any {
	return map[string]any{"source": source, "target": target, "head_ref_prefix": prefix, "authors": authors}
}

// Setting one repository's rules sends the whole map back with every other
// repository's rules unchanged, and discloses the new trust path as widening.
func TestAutoMergeUpdatePromotionsSetKeepsOtherRepos(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, map[string]any{
		"promotions": map[string]any{"b": []any{promotionArg(" main ", "release", "auto/release-", "github-actions[bot]", " github-actions[bot] ")}},
	})
	want := `{"promotions":{` +
		`"org/a":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/promote-","source":"main","target":"stable"}],` +
		`"org/b":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/release-","source":"main","target":"release"}]}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 1 || changes[0]["field"] != "promotions[org/b]" || changes[0]["before"] != nil {
		t.Fatalf("changes = %+v, want one row for org/b from nothing", changes)
	}
	for _, s := range []string{"Widening:", "promotions[org/b] adds a trust path", "main to release", "github-actions[bot]", "auto/release-*", "with no person, no linked issue and no author tier", "Trust carries over", "main takes changes only through a pull request", "inherits exactly main's review guarantee"} {
		if !strings.Contains(preview.WideningDisclosure, s) {
			t.Fatalf("disclosure missing %q: %s", s, preview.WideningDisclosure)
		}
	}
	if strings.Contains(preview.WideningDisclosure, "org/a") {
		t.Fatalf("the untouched org/a rule is disclosed as a change: %s", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
}

// Adding a rule, an author or a looser prefix lets more pull requests merge
// with no person. Removing any of those, or narrowing the prefix, does not.
func TestAutoMergeUpdatePromotionsWidening(t *testing.T) {
	a := func(rules ...any) map[string]any {
		return map[string]any{"promotions": map[string]any{"org/a": rules}}
	}
	for name, tc := range map[string]struct {
		args  map[string]any
		widen bool
		names string
	}{
		"new repository":  {map[string]any{"promotions": map[string]any{"org/c": []any{promotionArg("main", "stable", "auto/promote-", "github-actions[bot]")}}}, true, "promotions[org/c] adds a trust path"},
		"adds an author":  {a(promotionArg("main", "stable", "auto/promote-", "github-actions[bot]", "release-bot")), true, "release-bot"},
		"looser prefix":   {a(promotionArg("main", "stable", "auto/", "github-actions[bot]")), true, "auto/*"},
		"another rule":    {a(promotionArg("main", "stable", "auto/promote-", "github-actions[bot]"), promotionArg("stable", "release", "auto/promote-", "github-actions[bot]")), true, "stable to release"},
		"narrower prefix": {a(promotionArg("main", "stable", "auto/promote-main-", "github-actions[bot]")), false, ""},
		// Git refs are case-sensitive: Main is another branch, a new trust
		// path. Logins are not: the same author in other case adds nothing.
		"source differs only in case": {a(promotionArg("Main", "stable", "auto/promote-", "github-actions[bot]")), true, "Main to stable"},
		"author differs only in case": {a(promotionArg("main", "stable", "auto/promote-", "GitHub-Actions[bot]")), false, ""},
		"unset with null":             {map[string]any{"promotions": map[string]any{"org/a": nil}}, false, ""},
		"unset with empty":            {map[string]any{"promotions": map[string]any{"org/a": []any{}}}, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: autoMergePromotionsFixture()}), tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %s", got, tc.widen, preview.WideningDisclosure)
			}
			if tc.widen && !strings.Contains(preview.WideningDisclosure, tc.names) {
				t.Fatalf("disclosure does not name %q: %s", tc.names, preview.WideningDisclosure)
			}
		})
	}
}

// What the endpoint or hive.yaml would refuse is refused at preview, with no
// confirmation minted.
func TestAutoMergeUpdatePromotionsRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		settings map[string]any
		args     map[string]any
		want     string
	}{
		"source equals target":             {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{promotionArg("main", "main", "auto/", "github-actions[bot]")}}}, "must be different"},
		"no author":                        {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{promotionArg("main", "stable", "auto/")}}}, "at least one author"},
		"empty prefix":                     {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{promotionArg("main", "stable", " ", "github-actions[bot]")}}}, "head_ref_prefix must be a non-empty string"},
		"unknown rule field":               {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{map[string]any{"source": "main", "target": "stable", "head_ref_prefix": "auto/", "authors": []any{"x"}, "labels": []any{"release/ready"}}}}}, `not "labels"`},
		"not a list":                       {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": promotionArg("main", "stable", "auto/", "x")}}, "must be a list of rules"},
		"target twice":                     {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{promotionArg("main", "stable", "auto/", "x"), promotionArg("dev", "stable", "auto/", "x")}}}, "more than one rule"},
		"remove what is not there":         {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/b": nil}}, "no entry for org/b to remove"},
		"repository outside project.repos": {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/z": []any{promotionArg("main", "stable", "auto/", "x")}}}, "not one of this hive's repositories"},
		"same rules, other casing":         {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"ORG/A": []any{promotionArg("main", "stable", "auto/promote-", "github-actions[bot]")}}}, "nothing would change"},
		// A hive whose endpoint has no promotions field would drop it
		// silently, so it is refused rather than sent.
		"hive that does not report it": {autoMergeFixture(), map[string]any{"promotions": map[string]any{"org/b": []any{promotionArg("main", "stable", "auto/", "x")}}}, "may run an older build"},
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
}

// The change is merged into the rules the preview read. If those change
// before write_confirm, the confirm is refused and nothing is written.
func TestAutoMergeUpdatePromotionsRefusesStalePreview(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	h := autoMergeHandler(p)
	_, id := autoMergePreview(t, h, map[string]any{
		"promotions": map[string]any{"org/b": []any{promotionArg("main", "stable", "auto/promote-", "github-actions[bot]")}},
	})
	p.settings["promotions"] = map[string]any{} // someone removed org/a's rule meanwhile
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}
}
