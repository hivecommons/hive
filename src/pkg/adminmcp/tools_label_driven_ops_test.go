package adminmcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// labelDrivenGovernor is the governor settings read of a hive with the
// built-in defaults, acme/widget label-driven and acme/docs not.
func labelDrivenGovernor() map[string]any {
	return map[string]any{
		"repos": []string{"acme/widget", "acme/docs"},
		"labelDrivenApproval": map[string]any{
			"approvalLabels": []string{"triage/accepted"}, "approvalLabelsSet": false,
			"trustOrgMembers": true, "trustOrgMembersSet": false,
			"trustedBots": []string{"kubestellar-prow[bot]"}, "trustedBotsSet": false,
		},
		"repoLabelDriven": map[string]any{
			"acme/widget": map[string]any{"enabled": true, "effectiveLabels": []string{"triage/accepted"}},
			"acme/docs":   map[string]any{"enabled": false},
		},
	}
}

func labelDrivenPreview(t *testing.T, g map[string]any, args map[string]any) WritePreview {
	t.Helper()
	p, _ := capPreview(t, surfaceHandler(&surfaceProvider{governor: g}), WriteOpGovernorLabelDriven, args)
	return p
}

func TestLabelDrivenOpRejectsUnknownFields(t *testing.T) {
	h := surfaceHandler(&surfaceProvider{governor: labelDrivenGovernor()})
	for name, args := range map[string]map[string]any{
		"top-level typo":        {"trusted_bot": map[string]any{"add": []any{"x[bot]"}}},
		"supersede not offered": {"supersede_park_actors": map[string]any{"add": []any{"x[bot]"}}},
		"list change verb":      {"trusted_bots": map[string]any{"set": []any{"x[bot]"}}},
		"list not object":       {"trusted_bots": []any{"x[bot]"}},
		"empty change":          {"approval_labels": map[string]any{}},
		"trust not a bool":      {"trust_org_members": "yes"},
		"repo field typo":       {"repos": map[string]any{"acme/widget": map[string]any{"enable": true}}},
		"repo entry not object": {"repos": map[string]any{"acme/widget": true}},
		"repo names no change":  {"repos": map[string]any{"acme/widget": map[string]any{}}},
		"repos names no repo":   {"repos": map[string]any{}},
		"repo not watched":      {"repos": map[string]any{"other/repo": map[string]any{"enabled": true}}},
		"no change at all":      {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorLabelDriven, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

// The projectbluefin case: Prow adds labels as its App, so the App joins the
// trusted bots. The preview shows the list before → after, discloses the
// widening by name, and the confirm sends the merged list to the endpoint
// Settings → Repos saves through.
func TestLabelDrivenOpAddsTrustedBot(t *testing.T) {
	provider := &surfaceProvider{governor: labelDrivenGovernor()}
	h := surfaceHandler(provider)
	p, id := capPreview(t, h, WriteOpGovernorLabelDriven, map[string]any{"trusted_bots": map[string]any{"add": []any{"mergeraptor[bot]"}}})
	if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/governor/repos" {
		t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
	}
	if got, want := bodyJSON(t, p.Request.Body), `{"labelDrivenApproval":{"trustedBots":["kubestellar-prow[bot]","mergeraptor[bot]"]}}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "trusted_bots: [kubestellar-prow[bot]] → [kubestellar-prow[bot], mergeraptor[bot]]") {
		t.Fatalf("effects = %q", p.Effects)
	}
	if !strings.HasPrefix(p.WideningDisclosure, "Widening: more people or bots can release parked work.") || !strings.Contains(p.WideningDisclosure, "Label additions by mergeraptor[bot] now count") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	if strings.Contains(p.WideningDisclosure, "github-actions[bot] is the shared identity") {
		t.Fatalf("a bot other than github-actions[bot] carries its warning: %q", p.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(provider.requests) != 1 || provider.requests[0].Path != "/api/config/governor/repos" {
		t.Fatalf("executed %+v", provider.requests)
	}
}

// clear on a defaulted list restores the built-in default and says so,
// rather than claiming the list is empty.
func TestLabelDrivenOpClearRestoresDefault(t *testing.T) {
	g := labelDrivenGovernor()
	g["labelDrivenApproval"].(map[string]any)["trustedBots"] = []string{"kubestellar-prow[bot]", "mergeraptor[bot]"}
	g["labelDrivenApproval"].(map[string]any)["trustedBotsSet"] = true
	p := labelDrivenPreview(t, g, map[string]any{"trusted_bots": map[string]any{"clear": true}})
	if got, want := bodyJSON(t, p.Request.Body), `{"labelDrivenApproval":{"trustedBots":[]}}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "trusted_bots: [kubestellar-prow[bot], mergeraptor[bot]] → [kubestellar-prow[bot]] (restores the built-in default (kubestellar-prow[bot]))") {
		t.Fatalf("effects = %q", p.Effects)
	}
	if isWidening(p) || !strings.Contains(p.WideningDisclosure, "Narrowing: label additions by mergeraptor[bot] no longer count") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	// Clearing a list that already reads as the default changes nothing.
	if _, err := surfaceHandler(&surfaceProvider{governor: labelDrivenGovernor()}).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorLabelDriven, "args": map[string]any{"trusted_bots": map[string]any{"clear": true}}}); err == nil || !strings.Contains(err.Error(), "changes nothing") {
		t.Fatalf("clearing the default: err = %v", err)
	}
	// approval_labels clears the same way.
	g = labelDrivenGovernor()
	g["labelDrivenApproval"].(map[string]any)["approvalLabels"] = []string{"lgtm"}
	p = labelDrivenPreview(t, g, map[string]any{"approval_labels": map[string]any{"clear": true}})
	if !strings.Contains(strings.Join(p.Effects, "\n"), "approval_labels: [lgtm] → [triage/accepted] (restores the built-in default (triage/accepted))") {
		t.Fatalf("effects = %q", p.Effects)
	}
}

// Every change that lets more people or bots release parked work says so by
// name; one that lets fewer is disclosed as narrowing only.
func TestLabelDrivenOpFlagsWidening(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(g map[string]any)
		args   map[string]any
		widen  bool
		phrase string
	}{
		{"approval label added", nil, map[string]any{"approval_labels": map[string]any{"add": []any{"lgtm"}}}, true, "adding lgtm now approves"},
		{"approval label removed", func(g map[string]any) {
			g["labelDrivenApproval"].(map[string]any)["approvalLabels"] = []string{"triage/accepted", "lgtm"}
		}, map[string]any{"approval_labels": map[string]any{"remove": []any{"lgtm"}}}, false, "lgtm no longer approves"},
		{"org members turned on", func(g map[string]any) { g["labelDrivenApproval"].(map[string]any)["trustOrgMembers"] = false }, map[string]any{"trust_org_members": true}, true, "Public members of each repository's organisation may now release"},
		{"org members turned off", nil, map[string]any{"trust_org_members": false}, false, "can no longer approve by label"},
		{"trusted bot removed", func(g map[string]any) {
			g["labelDrivenApproval"].(map[string]any)["trustedBots"] = []string{"kubestellar-prow[bot]", "mergeraptor[bot]"}
		}, map[string]any{"trusted_bots": map[string]any{"remove": []any{"mergeraptor[bot]"}}}, false, "mergeraptor[bot] no longer count"},
		{"repository opts in", nil, map[string]any{"repos": map[string]any{"docs": map[string]any{"enabled": true}}}, true, "Acme/docs opts in"},
		{"repository opts out", nil, map[string]any{"repos": map[string]any{"acme/widget": map[string]any{"enabled": false}}}, false, "acme/widget opts out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := labelDrivenGovernor()
			if tc.mutate != nil {
				tc.mutate(g)
			}
			p := labelDrivenPreview(t, g, tc.args)
			if isWidening(p) != tc.widen {
				t.Fatalf("widening = %v, want %v: %q", isWidening(p), tc.widen, p.WideningDisclosure)
			}
			if !strings.Contains(p.WideningDisclosure, tc.phrase) {
				t.Fatalf("disclosure %q does not say %q", p.WideningDisclosure, tc.phrase)
			}
		})
	}
}

// Trusting github-actions[bot] carries the shared-identity warning.
func TestLabelDrivenOpWarnsOnGitHubActionsBot(t *testing.T) {
	p := labelDrivenPreview(t, labelDrivenGovernor(), map[string]any{"trusted_bots": map[string]any{"add": []any{"github-actions[bot]"}}})
	if !isWidening(p) || !strings.Contains(p.WideningDisclosure, "github-actions[bot] is the shared identity of every workflow in the repository") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
}

// Per-repository changes send the entry Settings → Repos sends: enabled with
// the repository's own list, or null to clear the opt-in.
func TestLabelDrivenOpRepos(t *testing.T) {
	p := labelDrivenPreview(t, labelDrivenGovernor(), map[string]any{"repos": map[string]any{"docs": map[string]any{"enabled": true}}})
	if got, want := bodyJSON(t, p.Request.Body), `{"repoLabelDriven":{"acme/docs":{"approvalLabels":[],"enabled":true}}}`; got != want {
		t.Fatalf("opt in body = %s, want %s", got, want)
	}
	p = labelDrivenPreview(t, labelDrivenGovernor(), map[string]any{"repos": map[string]any{"acme/widget": nil}})
	if got, want := bodyJSON(t, p.Request.Body), `{"repoLabelDriven":{"acme/widget":null}}`; got != want {
		t.Fatalf("null body = %s, want %s", got, want)
	}
	if !strings.Contains(strings.Join(p.Effects, "\n"), "repos[acme/widget].enabled: on → off") {
		t.Fatalf("effects = %q", p.Effects)
	}
	p = labelDrivenPreview(t, labelDrivenGovernor(), map[string]any{"repos": map[string]any{"acme/widget": map[string]any{"approval_labels": map[string]any{"add": []any{"lgtm"}}}}})
	if got, want := bodyJSON(t, p.Request.Body), `{"repoLabelDriven":{"acme/widget":{"approvalLabels":["lgtm"],"enabled":true}}}`; got != want {
		t.Fatalf("own labels body = %s, want %s", got, want)
	}
	if !strings.Contains(p.WideningDisclosure, "On acme/widget, lgtm now approves too") || !strings.Contains(p.WideningDisclosure, "on acme/widget, triage/accepted no longer approves") {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
	h := surfaceHandler(&surfaceProvider{governor: labelDrivenGovernor()})
	for name, repos := range map[string]map[string]any{
		"null on a repository that is not label-driven":   {"acme/docs": nil},
		"own labels on a repository that is not opted in": {"acme/docs": map[string]any{"approval_labels": map[string]any{"add": []any{"lgtm"}}}},
		"own labels with enabled false":                   {"acme/widget": map[string]any{"enabled": false, "approval_labels": map[string]any{"add": []any{"lgtm"}}}},
	} {
		if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorLabelDriven, "args": map[string]any{"repos": repos}}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A confirm whose preview read settings that have changed since is refused,
// and nothing is written.
func TestLabelDrivenOpRefusesStalePreview(t *testing.T) {
	provider := &surfaceProvider{governor: labelDrivenGovernor()}
	h := surfaceHandler(provider)
	_, id := capPreview(t, h, WriteOpGovernorLabelDriven, map[string]any{"trusted_bots": map[string]any{"add": []any{"mergeraptor[bot]"}}})
	// Somebody saves Settings → Repos in between.
	provider.governor["labelDrivenApproval"].(map[string]any)["trustedBots"] = []string{"kubestellar-prow[bot]", "added-meanwhile[bot]"}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(provider.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", provider.requests)
	}
}

// A hive whose settings read has no label-driven block (an older build) is
// refused rather than guessed.
func TestLabelDrivenOpNeedsTheCurrentSettings(t *testing.T) {
	g := labelDrivenGovernor()
	delete(g, "labelDrivenApproval")
	if _, err := surfaceHandler(&surfaceProvider{governor: g}).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorLabelDriven, "args": map[string]any{"trust_org_members": false}}); err == nil || !strings.Contains(err.Error(), "labelDrivenApproval") {
		t.Fatalf("missing labelDrivenApproval: err = %v", err)
	}
	p, err := (labelDrivenOp{}).Preview(context.Background(), map[string]any{"trust_org_members": false})
	if err != nil || p.Request.Body != nil || p.StateFingerprint != "" || !strings.HasPrefix(p.WideningDisclosure, "Potential widening:") {
		t.Fatalf("provider-less preview = %+v, %v", p, err)
	}
}
