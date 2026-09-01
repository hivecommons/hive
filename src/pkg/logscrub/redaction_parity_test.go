package logscrub

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Cross-language parity guard for the two redaction implementations
// (kubestellar/hive#5478).
//
// Hive redacts credential-shaped text in two places:
//
//   - this package (Go), used by the hub logger, pkg/ioscan, pkg/turn and the
//     sandbox executor; and
//   - redactTokens() in bin/contributor-relay.sh (JavaScript), which scrubs
//     agent tmux output before it is forwarded to the hub.
//
// They were hand-maintained lists in different languages with no shared source
// of truth and nothing failing when they drifted. The relay had fallen four
// categories behind: JWTs, PEM private-key blocks, `Bearer` values and AWS
// access keys were redacted by Go and forwarded unredacted by the relay.
//
// This is the same shape as TestShellAndGoCLIBackendListsAgree
// (src/pkg/config/backend_list_parity_test.go): a closed exception set that
// itself fails when an entry goes stale. It differs in one way that matters —
// the backend guard compares NAMES only, because a backend name is the whole
// contract. A redaction category name proves nothing on its own: a relay entry
// could declare `category: 'jwt'` next to a regex that never fires and the
// name check would pass while JWTs still leaked. So this file checks names AND
// behaviour, by running a synthetic sample of every category through both
// implementations.

const relayPath = "../../../bin/contributor-relay.sh"

// relayCategoryRe pulls the `category: 'name'` fields out of the
// HIVE_REDACTION_CATEGORIES array in the relay. Parsing the source rather than
// executing it keeps this test free of a Node dependency for the NAME half;
// the BEHAVIOUR half below shells out to node and skips if it is absent.
var relayCategoryRe = regexp.MustCompile(`category:\s*'([a-z0-9_]+)'`)

// redactionParityException documents one category that legitimately lives in
// only one implementation, and why. This is NOT a place to silence real drift.
// An entry whose stated asymmetry no longer holds fails the test below rather
// than rotting quietly.
type redactionParityException struct {
	name   string
	reason string
}

// redactionExceptions is the complete, closed set of permitted asymmetries.
// Anything not listed here must appear on both sides or the test fails.
//
// It is deliberately EMPTY: as of #5478 both implementations cover exactly the
// same eight categories. The type and the staleness check are kept so that a
// future genuinely one-sided category has a documented home, rather than being
// bolted on under time pressure by deleting the guard.
var redactionExceptions = []redactionParityException{}

func exceptedCategories() map[string]bool {
	out := make(map[string]bool, len(redactionExceptions))
	for _, e := range redactionExceptions {
		out[e.name] = true
	}
	return out
}

// relayCategories returns the category names declared in the relay source.
func relayCategories(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(relayPath)
	if err != nil {
		t.Skipf("contributor-relay.sh not reachable from the test working directory: %v", err)
	}
	// Scope the scan to the HIVE_REDACTION_CATEGORIES array so an unrelated
	// `category:` key elsewhere in the relay cannot inflate the set.
	start := strings.Index(string(src), "const HIVE_REDACTION_CATEGORIES = [")
	if start < 0 {
		t.Fatalf("HIVE_REDACTION_CATEGORIES not found in %s — the relay's redaction "+
			"category table was renamed or removed, which silently disables this parity guard", relayPath)
	}
	rest := string(src)[start:]
	end := strings.Index(rest, "\n];")
	if end < 0 {
		t.Fatalf("HIVE_REDACTION_CATEGORIES in %s is not terminated by a line starting `];` — "+
			"cannot delimit the array, so this guard would scan the rest of the file", relayPath)
	}
	var out []string
	for _, m := range relayCategoryRe.FindAllStringSubmatch(rest[:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// TestGoAndRelayRedactionCategoriesAgree is the guard #5478 asks for: every
// category redacted by Go is declared by the relay and vice versa.
//
// This DOES go red if a category is added to Go only. Adding an entry to
// secretPatterns with no HIVE_REDACTION_CATEGORIES counterpart fails the
// "Go has X, relay does not" direction on the first `go test` run touching
// this package.
func TestGoAndRelayRedactionCategoriesAgree(t *testing.T) {
	goSet := make(map[string]bool)
	for _, c := range Categories() {
		goSet[c] = true
	}
	relaySet := make(map[string]bool)
	for _, c := range relayCategories(t) {
		relaySet[c] = true
	}

	// Anti-vacuity (the #5409 lesson): a parse that yielded nothing on either
	// side would make every comparison below trivially true, so the test would
	// pass loudest exactly when it had stopped reading anything. Both sides
	// must be non-empty before any comparison is trusted.
	if len(goSet) == 0 {
		t.Fatalf("logscrub.Categories() returned no categories — the Go redaction list is " +
			"empty or unreadable, and every parity assertion below would pass vacuously")
	}
	if len(relaySet) == 0 {
		t.Fatalf("parsed no categories out of HIVE_REDACTION_CATEGORIES in %s — the array was "+
			"reshaped past what relayCategoryRe understands, and every parity assertion "+
			"below would pass vacuously", relayPath)
	}

	excepted := exceptedCategories()

	var problems []string
	for name := range goSet {
		if !relaySet[name] && !excepted[name] {
			problems = append(problems, "logscrub secretPatterns has "+name+
				" but HIVE_REDACTION_CATEGORIES in bin/contributor-relay.sh does not, "+
				"and it is not a declared exception — agent output carrying this "+
				"category reaches the hub unredacted")
		}
	}
	for name := range relaySet {
		if !goSet[name] && !excepted[name] {
			problems = append(problems, "HIVE_REDACTION_CATEGORIES in bin/contributor-relay.sh has "+
				name+" but logscrub secretPatterns does not, and it is not a declared exception")
		}
	}

	// A declared exception that no longer describes a real asymmetry (the
	// category is now in BOTH lists, or in NEITHER) is stale bookkeeping that
	// would mask a future real removal on one side.
	for name := range excepted {
		inGo, inRelay := goSet[name], relaySet[name]
		if inGo && inRelay {
			problems = append(problems, "declared exception "+name+
				" is now present in BOTH implementations; remove it from redactionExceptions")
		}
		if !inGo && !inRelay {
			problems = append(problems, "declared exception "+name+
				" is present in NEITHER implementation; remove it from redactionExceptions")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("Go and relay redaction categories have drifted:\n  %s", strings.Join(problems, "\n  "))
	}
}

// parityCase is a synthetic sample for one category. Values are obviously fake
// but shape-accurate; nothing here is copied from a real credential or log.
type parityCase struct {
	category string
	input    string
	// secret is the substring that must NOT survive redaction on either side.
	secret string
}

// A 36-character token body, the canonical length GitHub mints today.
const synthTokenBody = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// parityCases carries one sample per category. Every category declared by Go
// must appear here — TestRedactionParityCasesCoverEveryCategory enforces that,
// so a new category cannot be added with a behavioural sample quietly omitted.
var parityCases = []parityCase{
	{
		category: CategoryHiveCanary,
		input:    "canary HIVE-CANARY-" + strings.Repeat("ab", 24) + " tail",
		secret:   "HIVE-CANARY-" + strings.Repeat("ab", 24),
	},
	{
		category: CategoryGitHubToken,
		input:    "token=gho_" + synthTokenBody + " end",
		secret:   synthTokenBody,
	},
	{
		category: CategoryJWT,
		// Three dot-separated base64url runs of 20+ chars — JWT shape, not a
		// real token; the payload segment decodes to nothing meaningful.
		input:  "auth eyJ" + strings.Repeat("a", 25) + "." + strings.Repeat("b", 25) + "." + strings.Repeat("c", 25) + " end",
		secret: strings.Repeat("b", 25),
	},
	{
		category: CategoryAWSAccessKey,
		input:    "aws AKIAAAAAAAAAAAAAAAAA end",
		secret:   "AKIAAAAAAAAAAAAAAAAA",
	},
	{
		category: CategoryBearer,
		input:    "curl -H 'Authorization: Bearer " + strings.Repeat("a", 32) + "' https://example.test",
		secret:   strings.Repeat("a", 32),
	},
	{
		category: CategoryPEMPrivate,
		input:    "key -----BEGIN RSA PRIVATE KEY-----\nSYNTHETICKEYMATERIAL\n-----END RSA PRIVATE KEY----- done",
		secret:   "SYNTHETICKEYMATERIAL",
	},
	{
		category: CategoryPEMEncrypted,
		input:    "key -----BEGIN ENCRYPTED PRIVATE KEY-----\nSYNTHETICENCMATERIAL\n-----END ENCRYPTED PRIVATE KEY----- done",
		secret:   "SYNTHETICENCMATERIAL",
	},
	{
		category: CategoryPGPPrivate,
		input:    "key -----BEGIN PGP PRIVATE KEY BLOCK-----\nSYNTHETICPGPMATERIAL\n-----END PGP PRIVATE KEY BLOCK----- done",
		secret:   "SYNTHETICPGPMATERIAL",
	},
}

// TestRedactionParityCasesCoverEveryCategory keeps the behavioural half of
// this guard from going hollow. Without it, someone could add a category to
// both implementations, satisfy the name check, and never assert that either
// regex actually fires.
func TestRedactionParityCasesCoverEveryCategory(t *testing.T) {
	covered := make(map[string]bool, len(parityCases))
	for _, c := range parityCases {
		covered[c.category] = true
	}
	for _, name := range Categories() {
		if !covered[name] {
			t.Errorf("category %q is declared in secretPatterns but has no entry in parityCases, "+
				"so nothing asserts that either implementation actually redacts it", name)
		}
	}
	if len(parityCases) == 0 {
		t.Fatal("parityCases is empty — the behavioural half of this parity guard asserts nothing")
	}
}

// TestGoRedactsEveryParityCase pins the Go half: each synthetic sample is
// redacted and its secret substring does not survive.
func TestGoRedactsEveryParityCase(t *testing.T) {
	for _, c := range parityCases {
		out := ScrubString(c.input)
		// Assert only on post-redaction output; the input is never logged.
		if strings.Contains(out, c.secret) {
			t.Errorf("category %s: Go did not redact the sample (secret substring survived)", c.category)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("category %s: Go produced no %s marker", c.category, redacted)
		}
	}
}

// TestRelayRedactsEveryParityCase is the assertion the issue's verification
// list asks for: a JWT, a PEM block, a `Bearer` value and an AWS key are
// redacted in relay output. It runs the REAL redactTokens() from the relay,
// not a Go reimplementation of it — a reimplementation would pass while the
// shipped relay leaked.
func TestRelayRedactsEveryParityCase(t *testing.T) {
	for _, c := range parityCases {
		out := runRelayRedact(t, c.input)
		if strings.Contains(out, c.secret) {
			t.Errorf("category %s: the relay did not redact the sample (secret substring survived)", c.category)
		}
		if !strings.Contains(out, "***REDACTED***") {
			t.Errorf("category %s: the relay produced no ***REDACTED*** marker", c.category)
		}
	}
}

// TestUnderscoreGitHubTokenRedactedByBothPaths pins the specific divergence
// #5478 names: Go accepted [A-Za-z0-9_] after every GitHub prefix while the
// relay accepted only [A-Za-z0-9] for gh*_, so a gho_ token CONTAINING an
// underscore was redacted by Go and passed through by the relay.
func TestUnderscoreGitHubTokenRedactedByBothPaths(t *testing.T) {
	// Synthetic: a gho_ token whose body contains an underscore.
	body := "aaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbb"
	input := "token=gho_" + body + " end"

	if out := ScrubString(input); strings.Contains(out, body) {
		t.Error("Go did not redact a gho_ token containing an underscore")
	}
	if out := runRelayRedact(t, input); strings.Contains(out, body) {
		t.Error("the relay did not redact a gho_ token containing an underscore " +
			"(the [A-Za-z0-9] vs [A-Za-z0-9_] divergence from #5478 has regressed)")
	}
}

// TestShortGitHubTokenRedactedByBothPaths pins the other divergence: Go's
// floor was 10 trailing characters and the relay's was 36, so a truncated
// token-shaped run cleared the relay and was forwarded.
func TestShortGitHubTokenRedactedByBothPaths(t *testing.T) {
	// 20 characters: over Go's floor of 10, under the relay's OLD floor of 36.
	body := strings.Repeat("a", 20)
	input := "token=ghp_" + body + " end"

	if out := ScrubString(input); strings.Contains(out, body) {
		t.Error("Go did not redact a 20-character ghp_ token body")
	}
	if out := runRelayRedact(t, input); strings.Contains(out, body) {
		t.Error("the relay did not redact a 20-character ghp_ token body " +
			"(the 36-vs-10 length-floor divergence from #5478 has regressed)")
	}
}

// TestRelayKeepsOpenEndedTokenBound pins the #4267 invariant the fix had to
// preserve while moving the floor: the bound stays open-ended, so the WHOLE
// body of a longer-than-floor token is scrubbed rather than its first N
// characters with the tail leaking after the marker.
func TestRelayKeepsOpenEndedTokenBound(t *testing.T) {
	body := synthTokenBody + "TAILTAILTAIL"
	out := runRelayRedact(t, "log: gho_"+body+".")
	if strings.Contains(out, "TAILTAILTAIL") {
		t.Errorf("the tail of a long token leaked past the redaction marker — the {n,} "+
			"bound became exact, regressing #4267; got %q", out)
	}
}

// runRelayRedact loads bin/contributor-relay.sh in node and returns
// redactTokens(input). Only the redacted OUTPUT crosses back, and the input is
// passed on stdin rather than argv so it never lands in a process listing.
func runRelayRedact(t *testing.T, input string) string {
	t.Helper()
	if _, err := os.Stat(relayPath); err != nil {
		t.Skipf("contributor-relay.sh not reachable from the test working directory: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; skipping relay-side behavioural parity")
	}

	// HIVE_RELAY_TEST_MODE is what bin/contributor-relay.test.js sets to keep
	// the relay from touching tmux, bash or a WebSocket on load.
	// Loads the relay IN PLACE (so its own relative require of ./pi-backend.js
	// resolves) with 'ws' and child_process stubbed, mirroring the harness in
	// bin/contributor-relay.test.js. Nothing here reaches tmux, bash or a
	// socket.
	const driver = `
const Module = require('module');
const stubs = {
  ws: class { on() {} send() {} close() {} ping() {} },
  child_process: {
    execSync: () => '',
    execFile: () => {},
    execFileSync: () => '',
  },
};
const origLoad = Module._load;
Module._load = function (request) {
  if (Object.prototype.hasOwnProperty.call(stubs, request)) return stubs[request];
  return origLoad.apply(this, arguments);
};
// node refuses to require a .sh file with the default extension handlers.
Module._extensions['.sh'] = Module._extensions['.js'];
const relay = require(require('path').resolve(process.argv[1]));
Module._load = origLoad;

let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', d => { input += d; });
process.stdin.on('end', () => {
  // process.exit after an explicit flush: loading the relay arms heartbeat and
  // progress timers that would otherwise hold the event loop open forever and
  // hang the Go test rather than failing it.
  process.stdout.write(relay.redactTokens(input), () => process.exit(0));
});
`
	// Bounded: if the relay ever stops exiting (a new non-unref'd timer, say),
	// this must fail the test rather than hang the CI job indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", "-e", driver, relayPath)
	cmd.Stdin = strings.NewReader(input)
	// The relay refuses to load without a registration token and a backend, and
	// HIVE_RELAY_TEST_MODE keeps it from opening a hub connection. These mirror
	// what bin/contributor-relay.test.js sets. AGENT_BACKEND is deliberately
	// NOT 'pi': the pi path appends redactPiCredentials, whose behaviour is
	// covered by its own tests, and this guard is about the shared categories.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"HIVE_RELAY_TEST_MODE=1",
		"HIVE_REGISTRATION_TOKEN=parity-test-token",
		"AGENT_BACKEND=claude",
	)
	out, err := cmd.Output()
	if err != nil {
		// Surface the relay's own stderr: without it a load failure reports
		// only "exit status 1" and looks like a redaction bug.
		t.Fatalf("running the relay's redactTokens under node failed: %v\nstderr: %s", err, stderr.String())
	}
	return string(out)
}
