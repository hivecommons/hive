#!/usr/bin/env bash
# Regression tests for bin/go-wrapper.sh's in-pod test/vet block.
# Run: bash bin/go-wrapper.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="${ROOT_DIR}/bin/go-wrapper.sh"
WORK_DIR="${ROOT_DIR}/.go-wrapper-test-work-$$"
MOCK_GO="${WORK_DIR}/mock-go"
LOG_FILE="${WORK_DIR}/go-argv.log"
PASSED=0
FAILED=0

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

mkdir -p "$WORK_DIR"
cat >"$MOCK_GO" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_GO_LOG}"
if [[ "${1:-}" == "version" ]]; then
  echo "go version mock"
fi
exit 0
MOCK
chmod +x "$MOCK_GO"
: >"$LOG_FILE"

run_case() {
  local name="$1"
  local expected_exit="$2"
  local expected_reached="$3"
  shift 3

  : >"$LOG_FILE"
  set +e
  HIVE_GO_REAL="$MOCK_GO" MOCK_GO_LOG="$LOG_FILE" "$@" >"${WORK_DIR}/stdout" 2>"${WORK_DIR}/stderr"
  local exit_code=$?
  set -e

  local reached="no"
  if [[ -s "$LOG_FILE" ]]; then
    reached="yes"
  fi

  if [[ "$exit_code" == "$expected_exit" && "$reached" == "$expected_reached" ]]; then
    echo "ok - $name"
    PASSED=$((PASSED + 1))
  else
    echo "not ok - $name" >&2
    echo "  expected exit=$expected_exit reached=$expected_reached" >&2
    echo "  got exit=$exit_code reached=$reached" >&2
    echo "  stderr: $(cat "${WORK_DIR}/stderr")" >&2
    FAILED=$((FAILED + 1))
  fi
}

run_case "HIVE_AGENT go test is blocked" 2 no env HIVE_AGENT=scanner "$WRAPPER" test ./pkg/dashboard/
run_case "HIVE_AGENT go vet is blocked" 2 no env HIVE_AGENT=scanner "$WRAPPER" vet ./...
run_case "HIVE_AGENT_ID go test is blocked" 2 no env HIVE_AGENT_ID=scanner "$WRAPPER" test ./pkg/dashboard/
run_case "HIVE_AGENT_ID go vet is blocked" 2 no env HIVE_AGENT_ID=scanner "$WRAPPER" vet ./...
run_case "HIVE_AGENT go build passes through" 0 yes env HIVE_AGENT=scanner "$WRAPPER" build ./cmd/hive
run_case "agent go build passes through" 0 yes env HIVE_AGENT_ID=scanner "$WRAPPER" build ./cmd/hive
run_case "agent go version passes through" 0 yes env HIVE_AGENT_ID=scanner "$WRAPPER" version
run_case "escape hatch allows go test" 0 yes env HIVE_AGENT_ID=scanner HIVE_ALLOW_LOCAL_GO_TEST=1 "$WRAPPER" test ./pkg/dashboard/
run_case "non-agent go test passes through" 0 yes env -u HIVE_AGENT -u HIVE_AGENT_ID "$WRAPPER" test ./pkg/dashboard/
run_case "go tool vet is blocked" 2 no env HIVE_AGENT_ID=scanner "$WRAPPER" tool vet ./...
run_case "go tool test2json passes through" 0 yes env HIVE_AGENT_ID=scanner "$WRAPPER" tool test2json -h

set +e
HIVE_GO_REAL="$MOCK_GO" MOCK_GO_LOG="$LOG_FILE" HIVE_AGENT=scanner "$WRAPPER" test ./pkg/dashboard/ >"${WORK_DIR}/agent-name-stdout" 2>"${WORK_DIR}/agent-name-stderr"
agent_name_exit=$?
set -e
if [[ "$agent_name_exit" == "2" ]] && grep -q "⛔ BLOCKED: scanner:" "${WORK_DIR}/agent-name-stderr"; then
  echo "ok - blocked message includes agent name"
  PASSED=$((PASSED + 1))
else
  echo "not ok - blocked message includes agent name" >&2
  echo "  got exit=$agent_name_exit stderr=$(cat "${WORK_DIR}/agent-name-stderr")" >&2
  FAILED=$((FAILED + 1))
fi

set +e
HIVE_GO_REAL="${WORK_DIR}/missing-go" "$WRAPPER" version >"${WORK_DIR}/missing-stdout" 2>"${WORK_DIR}/missing-stderr"
missing_exit=$?
set -e
if [[ "$missing_exit" == "127" && "$(cat "${WORK_DIR}/missing-stderr")" == "go toolchain is not available" ]]; then
  echo "ok - missing real go exits 127"
  PASSED=$((PASSED + 1))
else
  echo "not ok - missing real go exits 127" >&2
  echo "  got exit=$missing_exit stderr=$(cat "${WORK_DIR}/missing-stderr")" >&2
  FAILED=$((FAILED + 1))
fi

if [[ "$FAILED" -ne 0 ]]; then
  echo "go-wrapper tests failed: $FAILED failed, $PASSED passed" >&2
  exit 1
fi

echo "go-wrapper tests passed: $PASSED"
