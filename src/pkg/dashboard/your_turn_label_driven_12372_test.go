package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestLabelDrivenRefusesUnparkCommands12372(t *testing.T) {
	off := []string{}
	cfg := &config.Config{Project: config.ProjectConfig{Org: "o", RepoPolicies: []config.RepoPolicy{
		{Repo: "o/refuses", LabelDriven: true, LabelDrivenApprovalLabels: off},
		{Repo: "o/approves", LabelDriven: true},
	}}}
	if !labelDrivenRefusesUnparkCommands(cfg, "o/refuses") {
		t.Fatal("a label-driven repo without label approval refuses /hive approve")
	}
	if labelDrivenRefusesUnparkCommands(cfg, "o/approves") {
		t.Fatal("label approval (default triage/accepted) makes /hive approve act")
	}
	if labelDrivenRefusesUnparkCommands(cfg, "o/plain") || labelDrivenRefusesUnparkCommands(nil, "o/refuses") {
		t.Fatal("a repo that did not opt in keeps the commands")
	}

	decisions := map[string]Ownership{
		ownershipKey(ownershipKindIssue, 1): yourTurn(OwnershipReasonParkedNeedsDecision, "Parked until a maintainer decides"),
		ownershipKey(ownershipKindIssue, 2): notYourTurn(),
		ownershipKey(ownershipKindPR, 3):    yourTurn(OwnershipReasonParkedNeedsDecision, "pr"),
	}
	env := ownershipEnv{unparkCommandsRefused: func(repo string) bool { return labelDrivenRefusesUnparkCommands(cfg, repo) }}
	markLabelDrivenReplies(decisions, "o/refuses", env)
	if !decisions[ownershipKey(ownershipKindIssue, 1)].LabelDriven {
		t.Fatal("the your-turn issue on a refusing repo must be flagged")
	}
	if decisions[ownershipKey(ownershipKindIssue, 2)].LabelDriven || decisions[ownershipKey(ownershipKindPR, 3)].LabelDriven {
		t.Fatal("only your-turn issues are flagged")
	}
	if ownershipIdentityOf(decisions[ownershipKey(ownershipKindIssue, 1)]) != ownershipIdentityOf(yourTurn(OwnershipReasonParkedNeedsDecision, "x")) {
		t.Fatal("the flag is not part of the ownership identity")
	}

	plain := map[string]Ownership{ownershipKey(ownershipKindIssue, 1): yourTurn(OwnershipReasonParkedNeedsDecision, "x")}
	markLabelDrivenReplies(plain, "o/approves", env)
	markLabelDrivenReplies(plain, "o/approves", ownershipEnv{})
	if plain[ownershipKey(ownershipKindIssue, 1)].LabelDriven {
		t.Fatal("a repo whose sweep acts on the commands keeps them")
	}
}

func TestYourTurnMenuLabelDrivenRepo12372(t *testing.T) {
	script := yourTurnPageScript(t, nil, `
const base = { kind: 'issue', url: 'https://github.com/o/r/issues/1', isOwner: true, planningAllowed: true, hasPlan: false, canToggleHold: true, held: false, sentinelLabel: '', admissionLabels: [] };
const kinds = (own, extra) => yourTurnMenuModel(Object.assign({}, base, { own }, extra || {})).entries.map(e => e.type + ':' + (e.label || e.text || e.id));
const own = (verb, code) => ({ your_turn: true, verb, reason_code: code, reason: 'because' });
for (const [verb, code] of [['DECIDE', 'parked_needs_decision'], ['STEER', 'parked_needs_direction'], ['REVIEW', 'parked_needs_human']]) {
  const entries = kinds(own(verb, code), { labelDriven: true });
  assert.ok(!entries.some(e => e.indexOf('/hive approve') === 0 || e.indexOf('copy:') === 0), verb + ' on a label-driven repo copies no /hive command: ' + entries);
  assert.equal(entries.length, 2);
  assert.match(entries[1], /^note:This repository is label-driven/);
  assert.deepEqual(kinds(own(verb, code)), ['link:Open on GitHub', 'copy:/hive approve', 'copy:/hive decision <text>'], 'other repos keep the commands');
}
`, "yourTurnMenuModel")
	runYourTurnNode(t, script)
}
