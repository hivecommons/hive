#!/usr/bin/env bash
# test-patch-coverage.sh — hermetic regression test for patch-coverage.sh.
#
# Exercises git diff parsing, coverprofile aggregation, exclusions
# (_test.go, testdata, generated code, comments, blank lines),
# threshold calculation, advisory mode, and PR comment posting / in-place updates.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/.github/scripts/patch-coverage.sh"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/patch-cov.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
assert_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then pass "$msg"; else bad "$msg: expected '$needle' in '$haystack'"; fi
}
assert_not_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then bad "$msg: did not expect '$needle' in '$haystack'"; else pass "$msg"; fi
}
assert_exit0() {
  local rc="$1" out="$2" msg="$3"
  if [[ $rc -eq 0 ]]; then pass "$msg"; else bad "$msg: exited $rc: $out"; fi
}
assert_exit1() {
  local rc="$1" out="$2" msg="$3"
  if [[ $rc -ne 0 ]]; then pass "$msg"; else bad "$msg: expected non-zero, got 0: $out"; fi
}

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

REPO_DIR="$TMP/repo"
git init -q -b v5 "$REPO_DIR"

commit_file() { # relpath content msg
  local dir
  dir="$(dirname "$REPO_DIR/$1")"
  mkdir -p "$dir"
  printf '%s\n' "$2" >"$REPO_DIR/$1"
  git -C "$REPO_DIR" add "$1"
  git -C "$REPO_DIR" commit -q -m "$3"
}

# Base commit: initial Go file
commit_file "src/pkg/sample/sample.go" 'package sample

func Initial() int {
	return 42
}
' 'initial commit'
base_commit="$(git -C "$REPO_DIR" rev-parse HEAD)"

# Fake gh CLI in PATH
MOCK_BIN="$TMP/mock_bin"
mkdir -p "$MOCK_BIN"
GH_LOG="$TMP/gh_calls.log"
cat > "$MOCK_BIN/gh" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$GH_LOG"
if [[ "\$*" == *"--input"* ]]; then
  cat >> "$GH_LOG"
fi

if [[ "\$*" == *"repos/example/hive/issues/42/comments?per_page=100"* ]]; then
  if [[ -f "$TMP/existing_comment_id" ]]; then
    cid=\$(cat "$TMP/existing_comment_id")
    echo "\$cid"
  fi
  exit 0
fi

if [[ "\$*" == *"repos/example/hive/issues/42/comments"* ]]; then
  # POST new comment
  exit 0
fi

if [[ "\$*" == *"repos/example/hive/issues/comments/"* ]]; then
  # PATCH existing comment
  exit 0
fi
exit 0
EOF
chmod +x "$MOCK_BIN/gh"
export PATH="$MOCK_BIN:$PATH"

# ==============================================================================
# Test 1: Full patch coverage (100%)
# ==============================================================================
printf '\n--- Test 1: 100%% patch coverage ---\n'
git -C "$REPO_DIR" checkout -q -b pr-full "$base_commit"
commit_file "src/pkg/sample/sample.go" 'package sample

func Initial() int {
	return 42
}

func Added(x int) int {
	y := x * 2
	return y + 1
}
' 'add Added func'

cat > "$TMP/coverage-full.out" <<'EOF'
mode: atomic
github.com/hivecommons/hive/pkg/sample/sample.go:3.20,5.2 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:7.23,10.2 2 1
EOF

SUM="$TMP/summary1.md"
rm -f "$SUM" "$GH_LOG"
out=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-full.out" --pr 42 --repo example/hive --summary-file "$SUM" 2>&1)
rc=$?
assert_exit0 $rc "$out" "full coverage exits 0"
assert_contains "$out" "Patch coverage: 100.0%" "reports 100% patch coverage"
assert_contains "$out" "Result: PASS" "reports PASS"
assert_contains "$(cat "$SUM")" "100.0%" "step summary records 100%"
# No previous comment existed and uncovered=0, so no comment posted
assert_not_contains "$(cat "$GH_LOG" 2>/dev/null || true)" "POST" "does not post comment on clean PR with no prior comment"

# If a previous comment existed, it is updated to 100%
echo "11111" > "$TMP/existing_comment_id"
rm -f "$GH_LOG"
out_prev=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-full.out" --pr 42 --repo example/hive 2>&1)
assert_exit0 $? "$out_prev" "full coverage with prior comment exits 0"
assert_contains "$(cat "$GH_LOG")" "PATCH repos/example/hive/issues/comments/11111" "updates existing comment to 100%"
assert_contains "$(cat "$GH_LOG")" "Patch Coverage: 100.0%" "updated comment reflects 100%"
rm -f "$TMP/existing_comment_id"

# ==============================================================================
# Test 2: Uncovered lines below threshold (threshold failure in non-advisory)
# ==============================================================================
printf '\n--- Test 2: Uncovered lines below threshold ---\n'
git -C "$REPO_DIR" checkout -q -b pr-uncovered "$base_commit"
commit_file "src/pkg/sample/sample.go" 'package sample

func Initial() int {
	return 42
}

func Complex(x int) int {
	if x > 100 {
		a := 1
		b := 2
		c := 3
		d := 4
		e := 5
		return a + b + c + d + e
	}
	return x
}
' 'add Complex func with untested branch'

cat > "$TMP/coverage-uncovered.out" <<'EOF'
mode: atomic
github.com/hivecommons/hive/pkg/sample/sample.go:3.20,5.2 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:7.25,8.13 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:8.13,16.3 6 0
github.com/hivecommons/hive/pkg/sample/sample.go:17.2,17.10 1 1
EOF

SUM="$TMP/summary2.md"
rm -f "$SUM" "$GH_LOG"
out_advisory=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-uncovered.out" --pr 42 --repo example/hive --summary-file "$SUM" --advisory 2>&1)
rc_advisory=$?
assert_exit0 $rc_advisory "$out_advisory" "advisory mode exits 0 despite low coverage"
assert_contains "$out_advisory" "Result: ADVISORY" "reports ADVISORY in advisory mode"
assert_contains "$out_advisory" "src/pkg/sample/sample.go:" "lists uncovered file"

# Verify non-advisory exits 1
out_strict=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-uncovered.out" --pr 42 --repo example/hive --summary-file "$SUM" 2>&1)
rc_strict=$?
assert_exit1 $rc_strict "$out_strict" "strict mode exits 1 when threshold failed"
assert_contains "$out_strict" "Result: FAILED" "reports FAILED in strict mode"

# Verify mock gh received POST with comment containing marker and uncovered lines
assert_contains "$(cat "$GH_LOG")" "POST repos/example/hive/issues/42/comments" "posts comment to PR"
assert_contains "$(cat "$GH_LOG")" "<!-- hive-patch-coverage -->" "comment contains marker"
assert_contains "$(cat "$GH_LOG")" "src/pkg/sample/sample.go" "comment lists uncovered file"

# ==============================================================================
# Test 3: Edit-in-place existing comment
# ==============================================================================
printf '\n--- Test 3: Edit-in-place existing comment ---\n'
echo "98765" > "$TMP/existing_comment_id"
rm -f "$GH_LOG"
out_update=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-uncovered.out" --pr 42 --repo example/hive --advisory 2>&1)
assert_exit0 $? "$out_update" "advisory update exits 0"
assert_contains "$(cat "$GH_LOG")" "PATCH repos/example/hive/issues/comments/98765" "edits existing comment in-place"
assert_not_contains "$(cat "$GH_LOG")" "POST repos/example/hive/issues/42/comments" "does not create new comment when one exists"
rm -f "$TMP/existing_comment_id"

# ==============================================================================
# Test 4: Small uncovered statements (< min-uncovered) do not trip gate
# ==============================================================================
printf '\n--- Test 4: Small uncovered statements (< 5) ---\n'
git -C "$REPO_DIR" checkout -q -b pr-small "$base_commit"
commit_file "src/pkg/sample/sample.go" 'package sample

func Initial() int {
	return 42
}

func Small(err error) error {
	if err != nil {
		return err
	}
	return nil
}
' 'add Small func with 1 uncovered return err'

cat > "$TMP/coverage-small.out" <<'EOF'
mode: atomic
github.com/hivecommons/hive/pkg/sample/sample.go:3.20,5.2 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:7.29,8.16 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:8.16,10.3 1 0
github.com/hivecommons/hive/pkg/sample/sample.go:11.2,11.12 1 1
EOF

rm -f "$GH_LOG"
out_small=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-small.out" --pr 42 --repo example/hive --min-percent 80 --min-uncovered 5 2>&1)
rc_small=$?
assert_exit0 $rc_small "$out_small" "uncovered < 5 does not trip failure even if < 80%"
assert_contains "$out_small" "Result: PASS" "small return err arm passes gate"

# ==============================================================================
# Test 5: Exclusions (_test.go, testdata, generated files, comments, blank lines)
# ==============================================================================
printf '\n--- Test 5: Exclusions ---\n'
git -C "$REPO_DIR" checkout -q -b pr-exclusions "$base_commit"
commit_file "src/pkg/sample/sample_test.go" 'package sample
import "testing"
func TestSample(t *testing.T) {}
' 'add test file'
commit_file "src/pkg/sample/testdata/fixture.go" 'package testdata
var Fixture = 123
' 'add testdata'
commit_file "src/pkg/sample/types.pb.go" 'package sample
type ProtobufType struct {}
' 'add pb.go'
commit_file "src/pkg/sample/gen.go" '// Code generated by tool. DO NOT EDIT.
package sample
func Generated() {}
' 'add generated comment file'
commit_file "src/pkg/sample/comments.go" 'package sample

// Only a comment line

' 'add comments and blanks only'

rm -f "$GH_LOG"
out_excl=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-full.out" --pr 42 --repo example/hive 2>&1)
rc_excl=$?
assert_exit0 $rc_excl "$out_excl" "diff with only excluded files exits 0"
assert_contains "$out_excl" "No changed Go lines to score" "all excluded files ignored"

# ==============================================================================
# Test 6: Multi-shard coverprofile merging
# ==============================================================================
printf '\n--- Test 6: Multi-shard coverprofile merging ---\n'
git -C "$REPO_DIR" checkout -q -b pr-multi "$base_commit"
commit_file "src/pkg/sample/sample.go" 'package sample

func Initial() int {
	return 42
}

func FuncA() int {
	return 1
}

func FuncB() int {
	return 2
}
' 'add FuncA and FuncB'

cat > "$TMP/shard-1.out" <<'EOF'
mode: atomic
github.com/hivecommons/hive/pkg/sample/sample.go:7.18,9.2 1 1
github.com/hivecommons/hive/pkg/sample/sample.go:11.18,13.2 1 0
EOF

cat > "$TMP/shard-2.out" <<'EOF'
mode: atomic
github.com/hivecommons/hive/pkg/sample/sample.go:7.18,9.2 1 0
github.com/hivecommons/hive/pkg/sample/sample.go:11.18,13.2 1 1
EOF

rm -f "$GH_LOG"
out_multi=$(cd "$REPO_DIR" && bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/shard-1.out" --profile "$TMP/shard-2.out" 2>&1)
rc_multi=$?
assert_exit0 $rc_multi "$out_multi" "multiple shard profiles merge correctly"
assert_contains "$out_multi" "Patch coverage: 100.0%" "both functions covered across shards"

# ==============================================================================
# Test 7: Comment upsert without the gh CLI (self-hosted runner images)
# ==============================================================================
# The v5 coverage job runs on the self-hosted pool, whose images do not ship
# gh; the script must still post/update the comment over plain HTTPS with
# GH_TOKEN against GITHUB_API_URL instead of dropping it with a ::notice.
printf '\n--- Test 7: comment upsert without gh on PATH ---\n'
API_LOG="$TMP/api_calls.log"
API_PORT_FILE="$TMP/api_port"
rm -f "$API_LOG" "$API_PORT_FILE"
python3 - "$API_LOG" "$API_PORT_FILE" <<'PYEOF' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

log_path, port_path = sys.argv[1], sys.argv[2]
existing = {"GET /repos/example/hive/issues/42/comments": []}

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def _record(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode("utf-8") if n else ""
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(f"{self.command} {self.path} auth={self.headers.get('Authorization','')}\n{body}\n")
        return body
    def _reply(self, obj):
        data = json.dumps(obj).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        self._record()
        if self.path.startswith("/repos/example/hive/issues/42/comments"):
            self._reply(existing["GET /repos/example/hive/issues/42/comments"])
        else:
            self._reply([])
    def do_POST(self):
        body = self._record()
        existing["GET /repos/example/hive/issues/42/comments"] = [{"id": 555, "body": json.loads(body)["body"]}]
        self._reply({"id": 555})
    def do_PATCH(self):
        self._record()
        self._reply({"id": 555})

srv = HTTPServer(("127.0.0.1", 0), H)
with open(port_path, "w") as f:
    f.write(str(srv.server_address[1]))
srv.serve_forever()
PYEOF
API_PID=$!
for _ in $(seq 1 50); do [[ -s "$API_PORT_FILE" ]] && break; sleep 0.1; done
if [[ ! -s "$API_PORT_FILE" ]]; then
  bad "stub GitHub API server did not start"
else
  API_URL="http://127.0.0.1:$(cat "$API_PORT_FILE")"
  NO_GH_BIN="$TMP/no_gh_bin"
  mkdir -p "$NO_GH_BIN"
  for tool in python3 git bash env awk sed grep cat dirname basename mktemp rm sort wc head tail; do
    p="$(command -v "$tool" 2>/dev/null || true)"
    [[ -n "$p" ]] && ln -sf "$p" "$NO_GH_BIN/$tool"
  done
  git -C "$REPO_DIR" checkout -q pr-uncovered
  run_without_gh() {
    (cd "$REPO_DIR" && env -i HOME="$HOME" PATH="$NO_GH_BIN" GH_TOKEN="test-token-123" GITHUB_API_URL="$API_URL" \
      bash "$SCRIPT" --base "$base_commit" --head HEAD --profile "$TMP/coverage-uncovered.out" --pr 42 --repo example/hive --advisory 2>&1)
  }
  out_nogh=$(run_without_gh)
  assert_exit0 $? "$out_nogh" "advisory run without gh exits 0"
  assert_not_contains "$out_nogh" "No such file or directory: 'gh'" "does not fail over to a missing gh binary"
  assert_contains "$out_nogh" "Posted patch coverage comment on PR #42" "reports the REST post"
  assert_contains "$(cat "$API_LOG")" "POST /repos/example/hive/issues/42/comments auth=Bearer test-token-123" "posts comment over REST with GH_TOKEN"
  assert_contains "$(cat "$API_LOG")" "<!-- hive-patch-coverage -->" "REST comment carries the marker"
  assert_contains "$(cat "$API_LOG")" "src/pkg/sample/sample.go" "REST comment lists uncovered file"

  rm -f "$API_LOG"
  out_nogh2=$(run_without_gh)
  assert_exit0 $? "$out_nogh2" "second advisory run without gh exits 0"
  assert_contains "$out_nogh2" "Updated patch coverage comment on PR #42 (comment ID 555)" "reports the REST in-place update"
  assert_contains "$(cat "$API_LOG")" "PATCH /repos/example/hive/issues/comments/555" "edits the existing comment over REST"
  assert_not_contains "$(cat "$API_LOG")" "POST /repos/example/hive/issues/42/comments" "does not post a duplicate over REST"
fi
kill "$API_PID" 2>/dev/null
wait "$API_PID" 2>/dev/null

# ==============================================================================
# Test Summary
# ==============================================================================
printf '\n=======================================================\n'
if [[ $failures -eq 0 ]]; then
  printf 'All patch-coverage self-tests PASSED!\n'
  exit 0
else
  printf 'patch-coverage self-tests FAILED: %d failure(s)\n' "$failures"
  exit 1
fi
