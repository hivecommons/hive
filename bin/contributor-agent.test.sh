#!/usr/bin/env bash
# Regression coverage for kubestellar/hive#2833.
#
# Run: bash bin/contributor-agent.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${ROOT_DIR}/.contributor-agent-test-work-$$"
HOOK_DIR="${WORK_DIR}/entrypoint.d"
HOME_DIR="${WORK_DIR}/home"
CORE_PATH="/usr/bin:/bin:/usr/sbin:/sbin"
SERVER_PID=""

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

mkdir -p "$HOOK_DIR" "$HOME_DIR"

cat >"${HOOK_DIR}/10-backend-override.sh" <<'HOOK'
backend_binary() {
  case "$1" in
    goose) echo "goose-from-entrypoint-hook" ;;
    *) echo "$1" ;;
  esac
}

backend_perm_flag() {
  case "$1" in
    goose) echo "--from-entrypoint-hook" ;;
    *) echo "" ;;
  esac
}
HOOK

run_resolve() {
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="$HOOK_DIR" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=goose \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
}

output="$(run_resolve)"

case "$output" in
  *"backend_binary=goose-from-entrypoint-hook"* ) ;;
  *)
    echo "expected entrypoint.d backend_binary override to survive; got:" >&2
    echo "$output" >&2
    exit 1
    ;;
esac

case "$output" in
  *"backend_perm_flag=--from-entrypoint-hook"* ) ;;
  *)
    echo "expected entrypoint.d backend_perm_flag override to survive; got:" >&2
    echo "$output" >&2
    exit 1
    ;;
esac

hook_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK='backend_binary(){ echo "goose-from-pre-agent-hook"; }; backend_perm_flag(){ echo "--from-pre-agent-hook"; }' \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=goose \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"

case "$hook_output" in
  *"backend_binary=goose-from-pre-agent-hook"* ) ;;
  *)
    echo "expected HIVE_PRE_AGENT_HOOK backend_binary override to survive; got:" >&2
    echo "$hook_output" >&2
    exit 1
    ;;
esac

case "$hook_output" in
  *"backend_perm_flag=--from-pre-agent-hook"* ) ;;
  *)
    echo "expected HIVE_PRE_AGENT_HOOK backend_perm_flag override to survive; got:" >&2
    echo "$hook_output" >&2
    exit 1
    ;;
esac

cat >"${WORK_DIR}/knowledge_stub.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sys

VALID = b"# Agent Knowledge\n\nThis file is auto-generated from the hive knowledge base.\nIt refreshes periodically -- do not edit manually.\n\n"

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        mode = self.path.split("/")[1] if self.path.count("/") >= 1 else ""
        if mode == "ok":
            self.send_response(200)
            self.send_header("Content-Type", "text/markdown; charset=utf-8")
            self.end_headers()
            self.wfile.write(VALID)
        elif mode == "redirect-body":
            self.send_response(302)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"<html>redirecting</html>\n")
        elif mode == "missing":
            self.send_response(404)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"not found\n")
        elif mode == "truncated":
            self.send_response(200)
            self.send_header("Content-Type", "text/markdown; charset=utf-8")
            self.end_headers()
            self.wfile.write(b"# Agent Knowledge\n")
        else:
            self.send_response(500)
            self.end_headers()

    def log_message(self, *_args):
        pass

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
Path(sys.argv[1]).write_text(str(server.server_port))
server.serve_forever()
PY

python3 "${WORK_DIR}/knowledge_stub.py" "${WORK_DIR}/server.port" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  [[ -s "${WORK_DIR}/server.port" ]] && break
  sleep 0.1
done
PORT="$(cat "${WORK_DIR}/server.port")"

run_knowledge_fetch() {
  local mode="$1"
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_FETCH=1 \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_DEST="${HOME_DIR}/agent.md" \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_URL="http://127.0.0.1:${PORT}/${mode}/api/knowledge/export" \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
}

rm -f "${HOME_DIR}/agent.md"
knowledge_ok_output="$(run_knowledge_fetch ok)"
case "$knowledge_ok_output" in
  *"knowledge_fetch=installed"* ) ;;
  *)
    echo "expected valid knowledge export to install; got:" >&2
    echo "$knowledge_ok_output" >&2
    exit 1
    ;;
esac
grep -qx "This file is auto-generated from the hive knowledge base\\." "${HOME_DIR}/agent.md" || {
  echo "expected installed agent.md to contain export marker" >&2
  exit 1
}

for mode in redirect-body missing truncated; do
  rm -f "${HOME_DIR}/agent.md"
  if output="$(run_knowledge_fetch "$mode" 2>&1)"; then
    echo "expected ${mode} knowledge fetch to fail; got:" >&2
    echo "$output" >&2
    exit 1
  fi
  if [[ -e "${HOME_DIR}/agent.md" ]]; then
    echo "expected ${mode} knowledge fetch to leave agent.md absent" >&2
    exit 1
  fi
done

echo "contributor-agent hook override tests passed"
echo "contributor-agent knowledge fetch tests passed"

rm -f "${HOME_DIR}/CLAUDE.md"
rm -rf "${HOME_DIR}/.bob"
BOB_AGENT_MD="${HOME_DIR}/agent.md"
env -i \
  PATH="${PATH}" \
  HOME="$HOME_DIR" \
  HIVE_REGISTRATION_TOKEN="test-token" \
  HIVE_CONTRIBUTOR_AGENT_TEST_LINK_KNOWLEDGE=1 \
  HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_DEST="$BOB_AGENT_MD" \
  AGENT_BACKEND=bob \
  bash "${ROOT_DIR}/bin/contributor-agent.sh"

if [[ "$(readlink "${HOME_DIR}/.bob/AGENTS.md")" != "$BOB_AGENT_MD" ]]; then
  echo "expected bob global AGENTS.md to link to the hive knowledge export" >&2
  exit 1
fi
if [[ "$(readlink "${HOME_DIR}/CLAUDE.md")" != "$BOB_AGENT_MD" ]]; then
  echo "expected bob's compatibility CLAUDE.md link to be retained" >&2
  exit 1
fi
echo "contributor-agent bob knowledge link tests passed"

codex_flags_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$codex_flags_output" in
  *"backend_perm_flag=--ask-for-approval on-request --sandbox workspace-write -c approvals_reviewer=auto_review --add-dir ${WORK_DIR}/workspace"* ) ;;
  *)
    echo "expected codex default posture to auto-review and include the task workspace; got:" >&2
    echo "$codex_flags_output" >&2
    exit 1
    ;;
esac

codex_reviewer_override_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_CODEX_APPROVALS_REVIEWER=user \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$codex_reviewer_override_output" in
  *"-c approvals_reviewer=user --add-dir ${WORK_DIR}/workspace"* ) ;;
  *)
    echo "expected codex reviewer override to be preserved; got:" >&2
    echo "$codex_reviewer_override_output" >&2
    exit 1
    ;;
esac

# An EXPLICITLY EMPTY reviewer must drop the -c key while KEEPING the sandbox.
# This is the escape hatch for a Codex release that rejects the unknown
# approvals_reviewer config key at startup: without it the only way out is
# HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1, i.e. no sandbox at all.
# ${VAR:-default} would defeat this by treating empty as unset, so the guard is
# the parameter expansion as much as the branch.
codex_reviewer_disabled_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_CODEX_APPROVALS_REVIEWER= \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$codex_reviewer_disabled_output" in
  *"approvals_reviewer"* )
    echo "expected an empty HIVE_CODEX_APPROVALS_REVIEWER to drop the -c key; got:" >&2
    echo "$codex_reviewer_disabled_output" >&2
    exit 1
    ;;
esac
# ...but the sandbox posture and the workspace grant must survive.
case "$codex_reviewer_disabled_output" in
  *"backend_perm_flag=--ask-for-approval on-request --sandbox workspace-write --add-dir ${WORK_DIR}/workspace"* ) ;;
  *)
    echo "expected the sandbox posture to survive a disabled reviewer; got:" >&2
    echo "$codex_reviewer_disabled_output" >&2
    exit 1
    ;;
esac

# A workspace path containing whitespace CANNOT be expressed: the caller
# word-splits this string (agent-launch.sh: read -r -a PERM_ARGS <<< ...), so
# "--add-dir /work space" would arrive as three argv words and grant Codex the
# wrong directory. The flag must be omitted, not silently corrupted.
mkdir -p "${WORK_DIR}/work space"
codex_spaced_workspace_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/work space" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" 2>/dev/null
)"
case "$codex_spaced_workspace_output" in
  *"--add-dir"* )
    echo "expected --add-dir to be omitted for a whitespace workspace path; got:" >&2
    echo "$codex_spaced_workspace_output" >&2
    exit 1
    ;;
esac
# The sandbox posture still applies — only the ungrantable flag is dropped.
case "$codex_spaced_workspace_output" in
  *"backend_perm_flag=--ask-for-approval on-request --sandbox workspace-write -c approvals_reviewer=auto_review"* ) ;;
  *)
    echo "expected the sandbox posture to survive a whitespace workspace path; got:" >&2
    echo "$codex_spaced_workspace_output" >&2
    exit 1
    ;;
esac

codex_bypass_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1 \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$codex_bypass_output" in
  *"backend_perm_flag=--dangerously-bypass-approvals-and-sandbox"* ) ;;
  *)
    echo "expected codex dangerous bypass to be opt-in; got:" >&2
    echo "$codex_bypass_output" >&2
    exit 1
    ;;
esac
case "$codex_bypass_output" in
  *"approvals_reviewer="*|*"--add-dir"* )
    echo "dangerous codex bypass must not retain sandbox-only reviewer/workspace flags; got:" >&2
    echo "$codex_bypass_output" >&2
    exit 1
    ;;
esac

FAKE_BIN="${WORK_DIR}/bin"
mkdir -p "$FAKE_BIN"
cat >"${FAKE_BIN}/codex" <<'CODEX'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then
  if [[ "${FAKE_CODEX_VERSION_FAIL:-}" == "1" ]]; then
    exit 42
  fi
  echo "codex 0.146.0"
fi
CODEX
chmod +x "${FAKE_BIN}/codex"
CODEX_HOME_DIR="${WORK_DIR}/codex-home"
mkdir -p "$CODEX_HOME_DIR"

run_codex_detect() {
  env -i \
    PATH="${FAKE_BIN}:${CORE_PATH}" \
    HOME="$HOME_DIR" \
    CODEX_HOME="$CODEX_HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
}

rm -f "${CODEX_HOME_DIR}/auth.json"
if output="$(run_codex_detect)"; [[ "$output" != "NOT_AUTHED" ]]; then
  echo "expected codex without CODEX_HOME/auth.json to be NOT_AUTHED; got: $output" >&2
  exit 1
fi

cat >"${CODEX_HOME_DIR}/auth.json" <<'JSON'
{"tokens":{"access_token":"oauth-access-token"}}
JSON
if output="$(run_codex_detect)"; [[ "$output" != "OK" ]]; then
  echo "expected codex OAuth auth.json to be OK; got: $output" >&2
  exit 1
fi

cat >"${CODEX_HOME_DIR}/auth.json" <<'JSON'
{"OPENAI_API_KEY":"api-key-login"}
JSON
if output="$(run_codex_detect)"; [[ "$output" != "OK" ]]; then
  echo "expected codex API-key auth.json to be OK; got: $output" >&2
  exit 1
fi

rm -f "${CODEX_HOME_DIR}/auth.json"
if output="$(
  env -i \
    PATH="${FAKE_BIN}:${CORE_PATH}" \
    HOME="$HOME_DIR" \
    CODEX_HOME="$CODEX_HOME_DIR" \
    CODEX_API_KEY="api-key-env" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"; [[ "$output" != "OK" ]]; then
  echo "expected codex CODEX_API_KEY environment auth to be OK; got: $output" >&2
  exit 1
fi

if output="$(
  env -i \
    PATH="${FAKE_BIN}:${CORE_PATH}" \
    HOME="$HOME_DIR" \
    CODEX_HOME="$CODEX_HOME_DIR" \
    CODEX_API_KEY="api-key-env" \
    FAKE_CODEX_VERSION_FAIL=1 \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"; [[ "$output" != "BROKEN" ]]; then
  echo "expected codex version failure to be BROKEN; got: $output" >&2
  exit 1
fi

if output="$(
  env -i \
    PATH="${CORE_PATH}" \
    HOME="$HOME_DIR" \
    CODEX_HOME="$CODEX_HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"; [[ "$output" != "NOT_INSTALLED" ]]; then
  echo "expected missing codex binary to be NOT_INSTALLED; got: $output" >&2
  exit 1
fi

echo "contributor-agent codex contract tests passed"
