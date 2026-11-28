package adminmcp

import (
	"context"
	"encoding/json"
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

// inherit_human_merge_paths on a rule (off unless sent). On v6 tip the rule
// shape refuses the key as an unknown field.

func promotionInheritArg(source, target, prefix string, authors ...any) map[string]any {
	rule := promotionArg(source, target, prefix, authors...)
	rule["inherit_human_merge_paths"] = true
	return rule
}

// storePromotions makes p store a write's promotions as its settings, the
// way the endpoint replaces the map, so a read after the write reports
// them. dropKeys are rule keys this hive does not know and drops silently,
// as a build older than the key does.
func storePromotions(t *testing.T, p *autoMergeProvider, dropKeys ...string) {
	t.Helper()
	p.onWrite = func(req WriteRequest) {
		b, err := json.Marshal(req.Body)
		if err != nil {
			t.Errorf("encode the request body: %v", err)
			return
		}
		var body struct {
			Promotions map[string][]map[string]any `json:"promotions"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("decode the request body: %v", err)
			return
		}
		stored := map[string]any{}
		for repo, rules := range body.Promotions {
			list := []any{}
			for _, rule := range rules {
				for _, k := range dropKeys {
					delete(rule, k)
				}
				list = append(list, rule)
			}
			stored[repo] = list
		}
		p.settings["promotions"] = stored
	}
}

// Turning the option on for a rule that is already there sends the rule with
// the key and discloses, in plain words, that files on the human-merge list
// reach the target with no person; nothing else is disclosed as a new trust
// path.
func TestAutoMergeUpdatePromotionsInheritTurnedOn(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	storePromotions(t, p)
	h := autoMergeHandler(p)
	preview, id := autoMergePreview(t, h, map[string]any{
		"promotions": map[string]any{"org/a": []any{promotionInheritArg("main", "stable", "auto/promote-", "github-actions[bot]")}},
	})
	want := `{"promotions":{"org/a":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/promote-","inherit_human_merge_paths":true,"source":"main","target":"stable"}]}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	changes, _ := preview.Details["changes"].([]map[string]any)
	if len(changes) != 1 || changes[0]["field"] != "promotions[org/a]" {
		t.Fatalf("changes = %+v, want one row for org/a", changes)
	}
	for _, s := range []string{
		"Widening:",
		"promotions[org/a] turns on inherit_human_merge_paths for main to stable, opened by github-actions[bot] from auto/promote-*",
		"files on the repository's human-merge list (auto_merge.human_merge_paths) reach stable with no person.",
		// What is proved, and what is not.
		"The hive proves only that the promotion's tree is main's current tree; it does not check how or when a listed file's content reached main.",
		// The cases that follow from the gap.
		"the path was added to human_merge_paths after the content was already on main",
		"the list binds only the hive's own merges, not another bot's or GitHub's own auto-merge, nor a person with merge rights",
		"a human_merge_pin_only exception or a bypass actor landed it on main",
		"main's own protection is weaker than stable's",
		"a person had changed the listed file only on stable (a hot-fix), which main's version then replaces",
		"main is itself the target of a rule with the option on",
		"Workflows triggered by a push to stable run as soon as the hive merges, so the merge into main is the last look a person gets.",
		"A hold or needs-owner-merge label a person adds",
	} {
		if !strings.Contains(preview.WideningDisclosure, s) {
			t.Fatalf("disclosure missing %q: %s", s, preview.WideningDisclosure)
		}
	}
	// The disclosure does not state what the hive never checks: that the
	// listed files were merged into the source by a person, under the list.
	for _, s := range []string{"same change", "same list applied", "inherits exactly what", "allowed, no more"} {
		if strings.Contains(preview.WideningDisclosure, s) {
			t.Fatalf("disclosure claims %q, which the hive does not check: %s", s, preview.WideningDisclosure)
		}
	}
	if strings.Contains(preview.WideningDisclosure, "adds a trust path") {
		t.Fatalf("an existing rule is disclosed as a new trust path: %s", preview.WideningDisclosure)
	}
	if strings.ContainsAny(preview.WideningDisclosure, "@\u2013\u2014") {
		t.Fatalf("disclosure carries a mention or a dash: %s", preview.WideningDisclosure)
	}
	res, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || compactJSON(p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
	// The rule is read back: the hive holds it with the option, so there is
	// no warning.
	out, _ := res.(map[string]any)
	after, _ := out["after"].(map[string]any)
	if out["after_error"] != nil || after["inherit_human_merge_paths_stored"] != true || after["warning"] != nil {
		t.Fatalf("confirm result = %+v, want after.inherit_human_merge_paths_stored true and no warning", out)
	}
	if got := compactJSON(after["promotions"]); got != `{"org/a":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/promote-","inherit_human_merge_paths":true,"source":"main","target":"stable"}]}` {
		t.Fatalf("after.promotions = %s, want org/a's rule as the hive holds it", got)
	}
}

// A hive on a build older than the option accepts the rule, drops the key
// it does not know and answers success. The write happened, so the
// confirmation does not fail: it reads the rule back and says, next to the
// result, that the hive stored it without the option. A read-back that
// fails is reported the same way, and a rule change that does not turn the
// option on is not read back at all.
func TestAutoMergeUpdatePromotionsInheritDroppedByAnOlderHive(t *testing.T) {
	inheritArgs := map[string]any{
		"promotions": map[string]any{"org/a": []any{promotionInheritArg("main", "stable", "auto/promote-", "github-actions[bot]")}},
	}
	p := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	storePromotions(t, p, "inherit_human_merge_paths")
	h := autoMergeHandler(p)
	_, id := autoMergePreview(t, h, inheritArgs)
	res, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm failed after the write was sent: %v", err)
	}
	out, _ := res.(map[string]any)
	if out["type"] != "write_result" || out["after_error"] != nil || len(p.requests) != 1 {
		t.Fatalf("confirm result = %+v (writes %d), want a write_result for the one write", out, len(p.requests))
	}
	after, _ := out["after"].(map[string]any)
	if after["inherit_human_merge_paths_stored"] != false {
		t.Fatalf("after = %+v, want inherit_human_merge_paths_stored false", after)
	}
	warning, _ := after["warning"].(string)
	for _, s := range []string{
		"the hive stored the rule without inherit_human_merge_paths; it may run an older build; the option is off",
		"promotions[org/a] main to stable",
		"human_merge_paths still leaves that rule's promotions for a person",
	} {
		if !strings.Contains(warning, s) {
			t.Fatalf("warning = %q, want it to say %q", warning, s)
		}
	}
	if strings.ContainsAny(warning, "@\u2013\u2014") {
		t.Fatalf("warning carries a mention or a dash: %s", warning)
	}
	// What the hive holds for the named repository: the rule, option off.
	if got := compactJSON(after["promotions"]); got != `{"org/a":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/promote-","source":"main","target":"stable"}]}` {
		t.Fatalf("after.promotions = %s, want org/a's rule without the option", got)
	}

	// The read-back itself failing: the write is still reported, with an
	// after_error saying the option could not be verified.
	p = &autoMergeProvider{settings: autoMergePromotionsFixture()}
	p.onWrite = func(WriteRequest) { p.readErr = errors.New("settings unavailable") }
	h = autoMergeHandler(p)
	_, id = autoMergePreview(t, h, inheritArgs)
	res, err = h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm failed after the write was sent: %v", err)
	}
	out, _ = res.(map[string]any)
	msg, _ := out["after_error"].(string)
	if out["type"] != "write_result" || out["after"] != nil || len(p.requests) != 1 ||
		!strings.Contains(msg, "the write succeeded") || !strings.Contains(msg, "the change was sent, but inherit_human_merge_paths could not be verified as stored") {
		t.Fatalf("confirm result = %+v (writes %d), want a write_result with an after_error saying the option could not be verified", out, len(p.requests))
	}

	// No rule in the request has the option on: nothing is read back, and
	// the result carries no "after".
	old := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	storePromotions(t, old, "inherit_human_merge_paths")
	store, readsAtWrite := old.onWrite, -1
	old.onWrite = func(req WriteRequest) {
		store(req)
		readsAtWrite = old.reads
	}
	h = autoMergeHandler(old)
	_, id = autoMergePreview(t, h, map[string]any{
		"promotions": map[string]any{"org/b": []any{promotionArg("main", "release", "auto/release-", "github-actions[bot]")}},
	})
	res, err = h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm of a rule without the option: %v", err)
	}
	if readsAtWrite < 0 || old.reads != readsAtWrite {
		t.Fatalf("settings read %d times, %d of them before the write; want none after it", old.reads, readsAtWrite)
	}
	out, _ = res.(map[string]any)
	if _, has := out["after"]; has || out["after_error"] != nil {
		t.Fatalf("confirm result = %+v, want no after and no after_error for a request that reads nothing back", out)
	}
}

// What widens and what does not: a rule added with the option on is a new
// trust path that also says what it inherits; the option already on is kept
// silently; turning it off, or sending the rule without the key, narrows.
func TestAutoMergeUpdatePromotionsInheritWidening(t *testing.T) {
	on := func() map[string]any {
		f := autoMergePromotionsFixture()
		f["promotions"] = map[string]any{
			"org/a": []any{map[string]any{"source": "main", "target": "stable", "authors": []string{"github-actions[bot]"}, "head_ref_prefix": "auto/promote-", "inherit_human_merge_paths": true}},
		}
		return f
	}
	a := func(rules ...any) map[string]any {
		return map[string]any{"promotions": map[string]any{"org/a": rules}}
	}
	for name, tc := range map[string]struct {
		settings map[string]any
		args     map[string]any
		widen    bool
		names    []string
		lacks    []string
	}{
		"new rule with the option on": {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/c": []any{promotionInheritArg("main", "stable", "auto/promote-", "github-actions[bot]")}}}, true,
			[]string{"promotions[org/c] adds a trust path", "still apply, human_merge_paths does not", "The rule has inherit_human_merge_paths on: files on the repository's human-merge list (auto_merge.human_merge_paths) reach stable with no person"}, nil},
		"new rule with the option off": {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/c": []any{promotionArg("main", "stable", "auto/promote-", "github-actions[bot]")}}}, true,
			[]string{"promotions[org/c] adds a trust path", "needs-owner-merge, human_merge_paths, the review verdict hold"}, []string{"inherit_human_merge_paths"}},
		"explicit false on a new rule": {autoMergePromotionsFixture(), map[string]any{"promotions": map[string]any{"org/c": []any{map[string]any{"source": "main", "target": "stable", "head_ref_prefix": "auto/promote-", "authors": []any{"github-actions[bot]"}, "inherit_human_merge_paths": false}}}}, true,
			[]string{"promotions[org/c] adds a trust path"}, []string{"inherit_human_merge_paths"}},
		"option on and a narrower prefix": {autoMergePromotionsFixture(), a(promotionInheritArg("main", "stable", "auto/promote-main-", "github-actions[bot]")), true,
			[]string{"promotions[org/a] turns on inherit_human_merge_paths for main to stable"}, []string{"adds a trust path"}},
		"option already on, narrower prefix": {on(), map[string]any{"promotions": map[string]any{"org/a": []any{promotionInheritArg("main", "stable", "auto/promote-main-", "github-actions[bot]")}}}, false, nil, nil},
		"turned off":                         {on(), a(promotionArg("main", "stable", "auto/promote-", "github-actions[bot]")), false, nil, nil},
		"option on for a looser prefix than it covered": {on(), a(promotionInheritArg("main", "stable", "auto/", "github-actions[bot]")), true,
			[]string{"promotions[org/a] adds a trust path", "auto/*", "The rule has inherit_human_merge_paths on"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: tc.settings}), tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %s", got, tc.widen, preview.WideningDisclosure)
			}
			for _, s := range tc.names {
				if !strings.Contains(preview.WideningDisclosure, s) {
					t.Fatalf("disclosure does not say %q: %s", s, preview.WideningDisclosure)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(preview.WideningDisclosure, s) {
					t.Fatalf("disclosure says %q: %s", s, preview.WideningDisclosure)
				}
			}
		})
	}
}

// The option already on is kept for a repository the change does not name,
// a rule sent again unchanged is no change, and a value that is not true or
// false is refused with no confirmation minted.
func TestAutoMergeUpdatePromotionsInheritKeptAndRefused(t *testing.T) {
	settings := func() map[string]any {
		f := autoMergePromotionsFixture()
		f["promotions"] = map[string]any{
			"org/a": []any{map[string]any{"source": "main", "target": "stable", "authors": []string{"github-actions[bot]"}, "head_ref_prefix": "auto/promote-", "inherit_human_merge_paths": true}},
		}
		return f
	}
	preview, _ := autoMergePreview(t, autoMergeHandler(&autoMergeProvider{settings: settings()}), map[string]any{
		"promotions": map[string]any{"org/b": []any{promotionArg("main", "release", "auto/release-", "github-actions[bot]")}},
	})
	want := `{"promotions":{` +
		`"org/a":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/promote-","inherit_human_merge_paths":true,"source":"main","target":"stable"}],` +
		`"org/b":[{"authors":["github-actions[bot]"],"head_ref_prefix":"auto/release-","source":"main","target":"release"}]}}`
	if got := compactJSON(preview.Request.Body); got != want {
		t.Fatalf("body = %s\nwant   %s", got, want)
	}
	if strings.Contains(preview.WideningDisclosure, "org/a") {
		t.Fatalf("the untouched org/a rule is disclosed as a change: %s", preview.WideningDisclosure)
	}
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"same rule again": {map[string]any{"promotions": map[string]any{"org/a": []any{promotionInheritArg("main", "stable", "auto/promote-", "github-actions[bot]")}}}, "nothing would change"},
		"not a boolean":   {map[string]any{"promotions": map[string]any{"org/a": []any{map[string]any{"source": "main", "target": "stable", "head_ref_prefix": "auto/promote-", "authors": []any{"github-actions[bot]"}, "inherit_human_merge_paths": "yes"}}}}, "inherit_human_merge_paths must be true or false"},
	} {
		t.Run(name, func(t *testing.T) {
			h := autoMergeHandler(&autoMergeProvider{settings: settings()})
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

// A preview that turns the option on is refused at confirm when the rules
// changed meanwhile: the disclosure was computed against rules that are gone.
func TestAutoMergeUpdatePromotionsInheritRefusesStalePreview(t *testing.T) {
	p := &autoMergeProvider{settings: autoMergePromotionsFixture()}
	h := autoMergeHandler(p)
	_, id := autoMergePreview(t, h, map[string]any{
		"promotions": map[string]any{"org/a": []any{promotionInheritArg("main", "stable", "auto/promote-", "github-actions[bot]")}},
	})
	// Someone gave the rule another author meanwhile.
	p.settings["promotions"] = map[string]any{
		"org/a": []any{map[string]any{"source": "main", "target": "stable", "authors": []string{"github-actions[bot]", "release-bot"}, "head_ref_prefix": "auto/promote-"}},
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote: %+v", p.requests)
	}
}
