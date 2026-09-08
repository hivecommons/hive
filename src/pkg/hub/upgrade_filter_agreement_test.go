package hub

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The "Upgrading" filter pill and the row's Upgrading badge are two readers of
// one question: is this hive upgrading right now? They disagreed in production
// — spokes visibly badged "Upgrading" were not matched when the operator
// clicked the pill.
//
// These tests execute the REAL shipped predicate out of dashboardHTML under
// node, rather than re-implementing it in Go. A Go re-implementation would be a
// second copy of the logic and could agree with itself perfectly while the
// shipped dashboard stayed broken — precisely the failure mode being fixed.
//
// Everything below drives hiveIsUpgradingNow / hiveUpgradeState /
// normalizeUpgradeState as extracted from the source of truth.

// jsFunc slices one top-level `function NAME(` ... matching-brace block out of
// dashboardHTML. Brace matching is string- and comment-aware enough for this
// file's style; if it ever mis-slices, node --check fails loudly in the test
// rather than silently testing the wrong text.
func jsFunc(t *testing.T, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^ {4}function ` + regexp.QuoteMeta(name) + `\s*\(`)
	loc := re.FindStringIndex(dashboardHTML)
	if loc == nil {
		t.Fatalf("function %s not found in dashboardHTML — the predicate under test "+
			"was renamed or removed; update this test deliberately, do not delete it", name)
	}
	src := dashboardHTML[loc[0]:]
	// Advance to the body's opening brace, then match to its close.
	open := strings.Index(src, "{")
	if open < 0 {
		t.Fatalf("function %s has no body", name)
	}
	depth, inStr, inLine, inBlock := 0, byte(0), false, false
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlock, i = false, i+1
			}
		case inStr != 0:
			switch c {
			case '\\':
				i++
			case inStr:
				inStr = 0
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLine, i = true, i+1
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlock, i = true, i+1
		case c == '\'' || c == '"':
			inStr = c
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return src[:i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces slicing function %s", name)
	return ""
}

// upgradeHarness is the shipped predicate plus the exact globals it closes
// over, with no behavioural stand-ins: every function below is the real source.
func upgradeHarness(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		"var SWITCH_SENTINEL_PREFIX = 'switch:';",
		"var BRANCH_TARGET_SUFFIX = '-latest';",
		"var UPGRADE_FILTER_UPGRADING = 'upgrading';",
		"var UPGRADE_FILTER_QUEUED = 'queued';",
		"var _latestSHA = '';",
		"var _latestSHAs = {};",
		"var _upgradingHives = {};",
		"var _switchStartedAt = {};",
		// hiveSwitchState reads the release-channel list to recognize a
		// channel-name target ("stable") as a switch that survives a page
		// refresh (added in #3771 for durable channel switches). In the
		// browser _releaseChannels is declared at page scope and seeded from
		// the dashboard poll; here the extracted function closes over it too,
		// so the harness must declare it or node throws ReferenceError before
		// any predicate runs. None of these fixtures targets a channel, so the
		// empty list is the correct default — the switch branch is not taken.
		"var _releaseChannels = [];",
		jsFunc(t, "sameShaJS"),
		jsFunc(t, "imageTagJS"),
		jsFunc(t, "hiveSwitchState"),
		jsFunc(t, "hiveIsUpgradingNow"),
		jsFunc(t, "normalizeUpgradeState"),
		jsFunc(t, "normalizeUpgradeStates"),
		jsFunc(t, "hiveUpgradeState"),
	}, "\n")
}

// runUpgradeJS evaluates body (which must assign to `result`) against the
// harness and unmarshals the JSON it prints.
func runUpgradeJS(t *testing.T, body string, out interface{}) {
	t.Helper()
	src := upgradeHarness(t) + "\nvar result;\n" + body +
		"\nprocess.stdout.write(JSON.stringify(result));\n"
	cmd := exec.Command("node", "-e", src)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		t.Fatalf("bad JSON from node: %v\ngot: %s", err, stdout)
	}
}

// upgFixture is one hive as the dashboard JSON delivers it, plus the client
// state that exists only in the browser.
type upgFixture struct {
	name string
	// hive is the hive object literal (JS source).
	hive string
	// latestSHAs seeds _latestSHAs.
	latestSHAs string
	// sentinel, when non-empty, seeds _upgradingHives[id].
	sentinel string
	// releaseChannels, when non-empty, seeds _releaseChannels (JS array
	// literal) — a channel switch's completion is judged against it.
	releaseChannels string
	// wantSentinelGone asserts the sentinel was expired by
	// normalizeUpgradeState; only meaningful with a sentinel set.
	wantSentinelGone bool
	// wantUpgrading is the expected answer from the SHARED predicate, which is
	// simultaneously the pill's classification and the row's badge.
	wantUpgrading bool
	// wantState is the expected hiveUpgradeState value.
	wantState string
	why       string
}

func upgradeFixtures() []upgFixture {
	const latest = `{"v4":"aaaaaaa"}`
	return []upgFixture{
		{
			name:          "armed latch in flight",
			hive:          `{id:'h1',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:true,upgradeTarget:'aaaaaaa'}`,
			latestSHAs:    latest,
			wantUpgrading: true,
			wantState:     "upgrading",
			why:           "the hub says a rollout is in flight; both surfaces must show it",
		},
		{
			// The operator's case. Floating v4-latest tag, hub tracks git SHA.
			name:          "behind branch tip with an armed target, no latch yet",
			hive:          `{id:'h2',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:false,upgradeTarget:'aaaaaaa'}`,
			latestSHAs:    latest,
			wantUpgrading: true,
			wantState:     "upgrading",
			why: "the hub has instructed this SHA and is waiting for the spoke to " +
				"report it — in flight before the latch sets",
		},
		{
			// POSITIVE CONTROL. Without this an always-true predicate passes
			// every other case in this table.
			name:          "at latest, nothing armed",
			hive:          `{id:'h3',gitBranch:'v4',gitHash:'aaaaaaa',upgrading:false}`,
			latestSHAs:    latest,
			wantUpgrading: false,
			wantState:     "",
			why:           "settled hive: neither surface may claim an upgrade",
		},
		{
			name:          "behind with auto-upgrade on and a target: queued, not upgrading",
			hive:          `{id:'h4',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:false,autoUpgrade:true,upgradeTarget:'aaaaaaa'}`,
			latestSHAs:    latest,
			wantUpgrading: false,
			wantState:     "queued",
			why:           "waiting on the auto-upgrade window is a distinct state",
		},
		{
			name:          "branch switch in flight",
			hive:          `{id:'h5',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:false,upgradeTarget:'v2-latest'}`,
			latestSHAs:    latest,
			wantUpgrading: true,
			wantState:     "upgrading",
			why:           "the spoke reports the OLD branch until the new pod beats",
		},
		{
			name:          "just-clicked sentinel, hive still on the pre-click SHA",
			hive:          `{id:'h6',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:false}`,
			latestSHAs:    latest,
			sentinel:      "bbbbbbb",
			wantUpgrading: true,
			wantState:     "upgrading",
			why:           "the row spins on the click before the hub latches",
		},
		{
			// #6301. Six hives switched to :candidate; pods Ready on the
			// candidate image; hub cleared its side ("spoke branch switch
			// complete") and reports no target. The row still said
			// "Switching to candidate — taking longer than expected" 20
			// minutes later, because a channel image heartbeats branch v4
			// and the switch test is targetBranch !== branchName.
			name:             "channel switch sentinel expires once the reported image tag matches",
			hive:             `{id:'h9',gitBranch:'v4',gitHash:'ccccccc',upgrading:false,imageRef:'ghcr.io/hivecommons/hive:candidate',trackedChannel:'candidate'}`,
			latestSHAs:       latest,
			sentinel:         "switch:candidate",
			releaseChannels:  `['stable','candidate']`,
			wantUpgrading:    false,
			wantState:        "",
			wantSentinelGone: true,
			why:              "the hub judged completion by the reported image tag; the row must agree",
		},
		{
			// POSITIVE CONTROL for the above: the old pod still reports the
			// old tag, so the switch is genuinely in flight.
			name:            "channel switch sentinel holds while the old image tag is still reported",
			hive:            `{id:'h10',gitBranch:'v4',gitHash:'ccccccc',upgrading:false,imageRef:'ghcr.io/hivecommons/hive:stable',trackedChannel:'candidate'}`,
			latestSHAs:      latest,
			sentinel:        "switch:candidate",
			releaseChannels: `['stable','candidate']`,
			wantUpgrading:   true,
			wantState:       "upgrading",
			why:             "the new pod is not up yet; the row must keep spinning",
		},
		{
			// Same family: "Upgrade now" on a hive already at everything its
			// tag delivers. The hub accepted, found nothing to do, cleared its
			// latch a minute later; the SHA never changed, so the sentinel
			// never expired and the row spun until reload.
			name:             "no-op upgrade sentinel expires when the hub reports the hive at its target",
			hive:             `{id:'h11',gitBranch:'v4',gitHash:'ccccccc',upgrading:false,behindTargetSHA:'ccccccc'}`,
			latestSHAs:       latest,
			sentinel:         "ccccccc",
			wantUpgrading:    false,
			wantState:        "",
			wantSentinelGone: true,
			why:              "the hub cleared the upgrade as a no-op; nothing will ever change the SHA",
		},
		{
			// POSITIVE CONTROL: same click, but the hub has latched — the
			// upgrade is real and in flight.
			name:          "upgrade sentinel holds while the hub reports upgrading",
			hive:          `{id:'h12',gitBranch:'v4',gitHash:'ccccccc',upgrading:true,upgradeTarget:'aaaaaaa',behindTargetSHA:'aaaaaaa'}`,
			latestSHAs:    latest,
			sentinel:      "ccccccc",
			wantUpgrading: true,
			wantState:     "upgrading",
			why:           "a genuine rollout must keep its spinner",
		},
		{
			// The 50-spoke case. A :stable spoke sits at exactly the commit
			// :stable carries (the hub resolves that as behindTargetSHA) while
			// the branch tip has moved on. Judged against its OWN target it is
			// current: not upgrading, not queued — even with auto-upgrade on.
			name:          "stable spoke current on its channel while the branch tip moved on",
			hive:          `{id:'h8',gitBranch:'v4',gitHash:'ccccccc',upgrading:false,autoUpgrade:true,trackedChannel:'stable',behindTargetSHA:'ccccccc'}`,
			latestSHAs:    latest,
			wantUpgrading: false,
			wantState:     "",
			why:           "the hub already refuses to move it; the row and pill must not say otherwise",
		},
		{
			name:          "latest unresolved",
			hive:          `{id:'h7',gitBranch:'v4',gitHash:'bbbbbbb',upgrading:true,upgradeTarget:'aaaaaaa'}`,
			latestSHAs:    `{}`,
			wantUpgrading: false,
			wantState:     "",
			why:           "the row suppresses the spinner while resolving; the pill must too",
		},
	}
}

// jsSetup renders the per-fixture globals.
func (f upgFixture) jsSetup() string {
	s := "_latestSHAs = " + f.latestSHAs + ";\nvar h = " + f.hive + ";\n"
	if f.releaseChannels != "" {
		s += "_releaseChannels = " + f.releaseChannels + ";\n"
	}
	if f.sentinel != "" {
		s += "_upgradingHives[h.id] = " + jsStr(f.sentinel) + ";\n"
	}
	return s
}

func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestUpgradingPredicateCases pins the shared predicate's answer per state,
// including the positive control that keeps "always true" from passing.
func TestUpgradingPredicateCases(t *testing.T) {
	for _, f := range upgradeFixtures() {
		t.Run(f.name, func(t *testing.T) {
			var got struct {
				Upgrading    bool   `json:"u"`
				State        string `json:"s"`
				SentinelGone bool   `json:"g"`
			}
			runUpgradeJS(t, f.jsSetup()+
				"normalizeUpgradeState(h);\n"+
				// The row resolves latest through the hub's reachable target
				// first (behindTargetSHA), then the branch tip — mirror that.
				"result = {u: hiveIsUpgradingNow(h, h.gitBranch, h.behindTargetSHA || _latestSHAs[h.gitBranch] || ''), s: hiveUpgradeState(h), g: _upgradingHives[h.id] === undefined};",
				&got)
			if got.Upgrading != f.wantUpgrading {
				t.Errorf("hiveIsUpgradingNow = %v, want %v — %s", got.Upgrading, f.wantUpgrading, f.why)
			}
			if got.State != f.wantState {
				t.Errorf("hiveUpgradeState = %q, want %q — %s", got.State, f.wantState, f.why)
			}
			if f.sentinel != "" && f.wantSentinelGone && !got.SentinelGone {
				t.Errorf("sentinel %q still set after normalizeUpgradeState — %s", f.sentinel, f.why)
			}
			if f.sentinel != "" && !f.wantSentinelGone && f.wantUpgrading && got.SentinelGone {
				t.Errorf("sentinel %q expired while the upgrade is still in flight — %s", f.sentinel, f.why)
			}
		})
	}
}

// TestPillAndBadgeAgree is the invariant that was violated in production: for
// the SAME fixture, what the filter pill classifies and what the row badge
// draws must be the same answer, in every state.
//
// The pill path is hiveUpgradeState(h) with no resolved branch args — it
// iterates raw hives. The row path passes the branch values it already
// resolved. Those two call shapes are exactly where the old code diverged, so
// the test drives both and compares.
func TestPillAndBadgeAgree(t *testing.T) {
	for _, f := range upgradeFixtures() {
		t.Run(f.name, func(t *testing.T) {
			var got struct {
				Pill bool `json:"pill"`
				Row  bool `json:"row"`
			}
			runUpgradeJS(t, f.jsSetup()+
				"normalizeUpgradeState(h);\n"+
				// Pill: the facet counter's call shape (no resolved args).
				"var pill = hiveUpgradeState(h) === UPGRADE_FILTER_UPGRADING;\n"+
				// Row: the render loop's call shape (resolved args).
				"var bn = h.gitBranch || 'v2';\n"+
				"var bl = _latestSHAs[bn] || _latestSHA;\n"+
				"var row = hiveIsUpgradingNow(h, bn, bl);\n"+
				"result = {pill: pill, row: row};",
				&got)
			if got.Pill != got.Row {
				t.Errorf("pill=%v but row badge=%v for the same hive — the filter and "+
					"the list disagree, which is the reported bug", got.Pill, got.Row)
			}
			if got.Row != f.wantUpgrading {
				t.Errorf("agreed on %v but both are wrong, want %v — %s",
					got.Row, f.wantUpgrading, f.why)
			}
		})
	}
}

// TestSentinelExpiryIsOrderIndependent is the ordering fix.
//
// applyDashFilters and the facet counter sweep the WHOLE list before the row
// loop draws anything. While sentinel expiry lived in the row loop, the pill
// read the map pre-expiry and the row read it post-expiry, so a hive whose
// sentinel had just gone stale was counted by the pill and not badged by the
// row (and vice versa on the following paint). Normalizing first makes the
// answer identical no matter which surface asks, and asking twice must not
// change it either.
func TestSentinelExpiryIsOrderIndependent(t *testing.T) {
	var got struct {
		PillBefore bool `json:"pb"`
		RowAfter   bool `json:"ra"`
		PillAgain  bool `json:"pa"`
	}
	// The hive has MOVED OFF the SHA the sentinel was armed with: the click has
	// landed, so the sentinel is stale and nothing is upgrading.
	runUpgradeJS(t,
		"_latestSHAs = {v4:'aaaaaaa'};\n"+
			"var h = {id:'h9',gitBranch:'v4',gitHash:'aaaaaaa',upgrading:false};\n"+
			"_upgradingHives[h.id] = 'bbbbbbb';\n"+
			// renderHives normalizes the whole list up front...
			"normalizeUpgradeStates([h]);\n"+
			// ...then the pill counts...
			"var pb = hiveUpgradeState(h) === UPGRADE_FILTER_UPGRADING;\n"+
			// ...then the row draws...
			"var ra = hiveIsUpgradingNow(h, 'v4', _latestSHAs.v4);\n"+
			// ...and a second paint must not flip anything.
			"normalizeUpgradeStates([h]);\n"+
			"var pa = hiveUpgradeState(h) === UPGRADE_FILTER_UPGRADING;\n"+
			"result = {pb: pb, ra: ra, pa: pa};",
		&got)
	if got.PillBefore || got.RowAfter {
		t.Errorf("stale sentinel still reads as upgrading: pill=%v row=%v — "+
			"expiry did not run ahead of the readers", got.PillBefore, got.RowAfter)
	}
	if got.PillBefore != got.RowAfter {
		t.Errorf("pill=%v row=%v — order-dependent divergence is back",
			got.PillBefore, got.RowAfter)
	}
	if got.PillAgain != got.PillBefore {
		t.Errorf("second paint changed the answer (%v -> %v) — normalization is not idempotent",
			got.PillBefore, got.PillAgain)
	}
}

// The row loop must not mutate the sentinel maps: that is what reintroduces the
// order dependency. Guard the source directly, because a re-introduced
// `delete _upgradingHives[...]` inside the render loop would pass every
// behavioural test above (which normalizes explicitly) and still ship the bug.
func TestRowLoopDoesNotMutateUpgradeSentinels(t *testing.T) {
	start := strings.Index(dashboardHTML, "var isUpgrading = hiveIsUpgradingNow(")
	if start < 0 {
		t.Fatal("row-loop call to hiveIsUpgradingNow not found — update this guard deliberately")
	}
	// Look at the render block around the badge decision.
	lo := start - 2000
	if lo < 0 {
		lo = 0
	}
	hi := start + 2000
	if hi > len(dashboardHTML) {
		hi = len(dashboardHTML)
	}
	window := dashboardHTML[lo:hi]
	for _, bad := range []string{
		"delete _upgradingHives[h.id]",
		"delete _switchStartedAt[h.id]",
		"h.upgrading = false",
	} {
		if strings.Contains(window, bad) {
			t.Errorf("row render mutates shared upgrade state (%q). The filter pill and "+
				"the facet counter run over the whole list BEFORE this loop, so mutating "+
				"here makes them read different inputs than the rows and the pill "+
				"under-reports again. Expire sentinels in normalizeUpgradeState instead.", bad)
		}
	}
}
