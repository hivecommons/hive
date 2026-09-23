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
            if self.headers.get("Authorization") != "Bearer test-token":
                self.send_response(401)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/markdown; charset=utf-8")
            self.end_headers()
            self.wfile.write(VALID)
        elif mode == "redirect-body":
            self.send_response(302)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"<html>redirecting</html>\n")
        elif mode == "redirect-login":
            # #8294 variant 1: a hosted spoke behind the hub's auth-proxy,
            # auth-signin redirecting the token-only fetch to the login page.
            self.send_response(302)
            self.send_header("Location", "https://hub.example/login?redirect=%2Fapi%2Fknowledge%2Fexport")
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"<html>redirecting</html>\n")
        elif mode == "hub-html":
            # #8294 variant 2: HIVE_HUB pointed at the hub itself, whose SPA
            # catch-all answers 200 with the landing page.
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(b"<!DOCTYPE html>\n<html lang=\"en\">\n<head></head></html>\n")
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

# Each failure mode must fail, leave agent.md absent, and (#8294) name the
# URL, the HTTP status and the reason on the failure line - before that fix
# every mode printed the same "unavailable" and the operator could not tell a
# login redirect from the hub's landing page from a 404.
expect_failure_reason() {
  local mode="$1" output="$2"
  shift 2
  local needle
  for needle in "http://127.0.0.1:${PORT}/${mode}/api/knowledge/export" "$@"; do
    case "$output" in
      *"$needle"* ) ;;
      *)
        echo "expected ${mode} knowledge fetch failure to mention '${needle}'; got:" >&2
        echo "$output" >&2
        exit 1
        ;;
    esac
  done
}

for mode in redirect-body redirect-login hub-html missing truncated; do
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
  case "$mode" in
    redirect-body)
      expect_failure_reason "$mode" "$output" "HTTP 302" "redirected instead of serving the export"
      ;;
    redirect-login)
      expect_failure_reason "$mode" "$output" "HTTP 302" "redirected to the login page" "https://hub.example/login"
      ;;
    hub-html)
      expect_failure_reason "$mode" "$output" "HTTP 200" "HIVE_HUB points at the hub, not a hosted spoke; knowledge export is served by the spoke"
      ;;
    missing)
      expect_failure_reason "$mode" "$output" "HTTP 404" "status is not 200"
      ;;
    truncated)
      expect_failure_reason "$mode" "$output" "HTTP 200" "body is not a knowledge export"
      ;;
  esac
  case "$output" in
    *"test-token"* )
      echo "${mode} knowledge fetch failure line leaked the registration token" >&2
      exit 1
      ;;
  esac
done

# No response at all: a closed port must report the curl exit code, not a
# made-up status.
rm -f "${HOME_DIR}/agent.md"
if output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_FETCH=1 \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_DEST="${HOME_DIR}/agent.md" \
    HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_URL="http://127.0.0.1:9/api/knowledge/export" \
    KNOWLEDGE_FETCH_MAX_TIME=5 \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" 2>&1
)"; then
  echo "expected unreachable knowledge fetch to fail; got:" >&2
  echo "$output" >&2
  exit 1
fi
case "$output" in
  *"http://127.0.0.1:9/api/knowledge/export"*"no response (curl exit "* ) ;;
  *)
    echo "expected unreachable knowledge fetch to report the URL and curl exit code; got:" >&2
    echo "$output" >&2
    exit 1
    ;;
esac

echo "contributor-agent hook override tests passed"
echo "contributor-agent knowledge fetch tests passed"

# Pi uses one shared provider/model parser for first launch and relay restarts.
# The test hook exits before any tmux/network setup, so these are deterministic
# startup-contract checks rather than a claim that a real provider authenticated.
pi_selection_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    OPENAI_API_KEY="synthetic-invalid-pi-key" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_PI_SELECTION=1 \
    AGENT_BACKEND=pi \
    AGENT_MODEL=openai/gpt-5 \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$pi_selection_output" in
  *'"provider":"openai"'*'"model":"openai/gpt-5"'*'"authentication":"configured_unverified"'* ) ;;
  *)
    echo "expected canonical Pi selection/readiness JSON; got: $pi_selection_output" >&2
    exit 1
    ;;
esac
case "$pi_selection_output" in
  *"synthetic-invalid-pi-key"* )
    echo "Pi readiness leaked a provider credential" >&2
    exit 1
    ;;
esac
if env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_PI_SELECTION=1 \
    AGENT_BACKEND=pi \
    AGENT_MODEL=bare-model \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" >/dev/null 2>&1; then
  echo "expected Pi startup to reject an unqualified model" >&2
  exit 1
fi
echo "contributor-agent Pi selection tests passed"

rm -f "${HOME_DIR}/AGENTS.md" "${HOME_DIR}/CLAUDE.md" "${HOME_DIR}/agent.pi-context.md"
PI_AGENT_MD="${HOME_DIR}/agent.md"
cat >"$PI_AGENT_MD" <<'EOF'
# Agent Knowledge

This file is auto-generated from the hive knowledge base.
It refreshes periodically — do not edit manually.

## Patterns

### Generic background

generic filler generic filler generic filler generic filler generic filler generic filler
generic filler generic filler generic filler generic filler generic filler generic filler
generic filler generic filler generic filler generic filler generic filler generic filler
generic filler generic filler generic filler generic filler generic filler generic filler

### Target repo task guidance

target-repo guidance for failing contributor context overflow and pi startup.

### Another global fact

global filler global filler global filler global filler global filler global filler
global filler global filler global filler global filler global filler global filler
global filler global filler global filler global filler global filler global filler
EOF
env -i \
  PATH="${PATH}" \
  HOME="$HOME_DIR" \
  HIVE_REGISTRATION_TOKEN="test-token" \
  HIVE_CONTRIBUTOR_AGENT_TEST_LINK_KNOWLEDGE=1 \
  HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_DEST="$PI_AGENT_MD" \
  HIVE_CONTRIBUTOR_KNOWLEDGE_TOKEN_BUDGET=120 \
  HIVE_REPO=target-repo \
  HIVE_TASK_TITLE="Fix contributor context overflow" \
  AGENT_BACKEND=pi \
  bash "${ROOT_DIR}/bin/contributor-agent.sh"

if [[ "$(readlink "${HOME_DIR}/AGENTS.md")" != "${HOME_DIR}/agent.pi-context.md" ]]; then
  echo "expected pi AGENTS.md to link to the budgeted Hive knowledge context" >&2
  exit 1
fi
if [[ "$(readlink "${HOME_DIR}/CLAUDE.md")" != "${HOME_DIR}/agent.pi-context.md" ]]; then
  echo "expected pi CLAUDE.md compatibility link to use the budgeted context" >&2
  exit 1
fi
if [[ ! -s "${HOME_DIR}/agent.pi-context.md" ]]; then
  echo "expected pi budgeted context file to be written" >&2
  exit 1
fi
if [[ "$(wc -c < "${HOME_DIR}/agent.pi-context.md")" -gt 480 ]]; then
  echo "expected pi budgeted context to stay within the 120-token estimate" >&2
  exit 1
fi
grep -q "Target repo task guidance" "${HOME_DIR}/agent.pi-context.md" || {
  echo "expected repo/task-scoped knowledge to survive budgeting" >&2
  exit 1
}
grep -q "Hive knowledge truncated: token budget reached" "${HOME_DIR}/agent.pi-context.md" || {
  echo "expected pi budgeted context to carry the truncation marker" >&2
  exit 1
}
grep -q "hive knowledge" "${HOME_DIR}/agent.pi-context.md" || {
  echo "expected pi budgeted context to point at on-demand knowledge fetching" >&2
  exit 1
}
target_line="$(grep -n "Target repo task guidance" "${HOME_DIR}/agent.pi-context.md" | head -n1 | cut -d: -f1)"
generic_line="$(grep -n "Generic background" "${HOME_DIR}/agent.pi-context.md" | head -n1 | cut -d: -f1 || true)"
if [[ -n "$generic_line" && "$generic_line" -lt "$target_line" ]]; then
  echo "expected repo/task-scoped knowledge to be ordered before global knowledge" >&2
  exit 1
fi
echo "contributor-agent pi knowledge budget tests passed"

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

# OMP reads both of Hive's conventional knowledge-link names. The test hook
# exits before launching tmux or the relay, so it proves the agent-owned seam
# without reproducing any of its lifecycle.
rm -f "${HOME_DIR}/AGENTS.md" "${HOME_DIR}/CLAUDE.md"
OMP_AGENT_MD="${HOME_DIR}/agent.md"
env -i \
  PATH="${PATH}" \
  HOME="$HOME_DIR" \
  HIVE_REGISTRATION_TOKEN="test-token" \
  HIVE_CONTRIBUTOR_AGENT_TEST_LINK_KNOWLEDGE=1 \
  HIVE_CONTRIBUTOR_AGENT_TEST_KNOWLEDGE_DEST="$OMP_AGENT_MD" \
  AGENT_BACKEND=omp \
  bash "${ROOT_DIR}/bin/contributor-agent.sh"

for link in "${HOME_DIR}/AGENTS.md" "${HOME_DIR}/CLAUDE.md"; do
  if [[ "$(readlink "$link")" != "$OMP_AGENT_MD" ]]; then
    echo "expected OMP knowledge link $link to target the Hive export" >&2
    exit 1
  fi
done

mkdir -p "${WORK_DIR}/omp-bin"
cat >"${WORK_DIR}/omp-bin/omp" <<'OMP'
#!/bin/sh
exit 0
OMP
chmod +x "${WORK_DIR}/omp-bin/omp"
omp_detect_output="$(
  env -i \
    PATH="${WORK_DIR}/omp-bin:${CORE_PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 \
    AGENT_BACKEND=omp \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
if [[ "$omp_detect_output" != "UNVERIFIED" ]]; then
  echo "expected OMP preflight to report an installed CLI without claiming authentication; got: $omp_detect_output" >&2
  exit 1
fi
echo "contributor-agent OMP knowledge and preflight tests passed"

# ── NOT_INSTALLED names the image, not auth (#7661) ─────────────────────
#
# `just contribute-hive omp` (container mode) died with `omp CLI not found.
# Install it and try again.` because the image did not ship omp; the host
# copy `contribute-setup` had probed is not mounted. "Install it" is not
# something the operator can do inside the image, so inside the container
# the entrypoint must name the image as the cause and print the one command
# that does run — local mode with the backend's unconfined opt-in. Outside
# the container (a derived image's host-side reuse, k8s pods built from
# another base) the plain message stays. The container predicate is stubbed
# through the same HIVE_PRE_AGENT_HOOK seam the codex sandbox probe uses.
run_missing_cli_startup() {
  # $1: stub for codex_inside_contributor_container's return
  local in_container="$1"
  env -i \
    PATH="${CORE_PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK="codex_inside_contributor_container(){ return ${in_container}; }" \
    AGENT_BACKEND=omp \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" 2>&1
}

missing_in_container_rc=0
missing_in_container_output="$(run_missing_cli_startup 0)" || missing_in_container_rc=$?
if [[ "$missing_in_container_rc" -ne 1 ]]; then
  echo "expected a missing omp binary to exit 1 at startup; got rc=${missing_in_container_rc}:" >&2
  echo "$missing_in_container_output" >&2
  exit 1
fi
for want in \
  "ERROR: omp CLI not found." \
  "omp is not in the contributor image" \
  "HIVE_OMP_DANGEROUSLY_RUN_UNCONFINED=1 just contribute-hive omp local"; do
  case "$missing_in_container_output" in
    *"$want"* ) ;;
    *)
      echo "expected the in-container missing-CLI error to say '${want}'; got:" >&2
      echo "$missing_in_container_output" >&2
      exit 1
      ;;
  esac
done
case "$missing_in_container_output" in
  *"Install it and try again."* )
    echo "in-container missing-CLI error must not tell the operator to install into the image; got:" >&2
    echo "$missing_in_container_output" >&2
    exit 1
    ;;
esac

missing_on_host_rc=0
missing_on_host_output="$(run_missing_cli_startup 1)" || missing_on_host_rc=$?
if [[ "$missing_on_host_rc" -ne 1 ]]; then
  echo "expected a missing omp binary outside the container to exit 1; got rc=${missing_on_host_rc}" >&2
  exit 1
fi
case "$missing_on_host_output" in
  *"Install it and try again."* ) ;;
  *)
    echo "expected the outside-container missing-CLI error to keep 'Install it and try again.'; got:" >&2
    echo "$missing_on_host_output" >&2
    exit 1
    ;;
esac
case "$missing_on_host_output" in
  *"not in the contributor image"* )
    echo "outside the container the error must not blame the image; got:" >&2
    echo "$missing_on_host_output" >&2
    exit 1
    ;;
esac
echo "contributor-agent missing-CLI cause tests passed"

# The codex --sandbox VALUE is PROBED at launch (kubestellar/hive#6653; see
# codex_default_sandbox_mode in config/backends.conf), so a bare assertion of
# "workspace-write" would silently mean "whatever the machine running this
# suite happens to allow". These stubs pin both halves of the probe through the
# HIVE_PRE_AGENT_HOOK seam — which is eval'd after backends.conf is sourced and
# before backend_perm_flag runs — so the assertions below are about FLAG
# ASSEMBLY and nothing else. The probe's own behaviour is tested separately at
# the end of this block.
#
# This matters concretely: the suite is routinely run INSIDE the contributor
# container, which is exactly the environment that resolves to
# danger-full-access.
CODEX_SANDBOX_OK_HOOK='codex_inside_contributor_container(){ return 1; }; codex_userns_available(){ return 0; }'

codex_flags_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK="$CODEX_SANDBOX_OK_HOOK" \
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
    HIVE_PRE_AGENT_HOOK="$CODEX_SANDBOX_OK_HOOK" \
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
    HIVE_PRE_AGENT_HOOK="$CODEX_SANDBOX_OK_HOOK" \
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
    HIVE_PRE_AGENT_HOOK="$CODEX_SANDBOX_OK_HOOK" \
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

# ── codex sandbox-mode probe (kubestellar/hive#6653) ────────────────────
#
# Codex's workspace-write sandbox is bubblewrap, and bubblewrap needs an
# unprivileged user namespace. The contributor container denies that syscall
# under the runtime's default seccomp profile, so asking for workspace-write
# there failed every model-generated command — including both of Codex's
# patch-application paths — with a bwrap error naming a HOST sysctl.
#
# The resolution is a probe over two independent predicates, and each of the
# four combinations below is a distinct claim about what hive will launch.
run_codex_sandbox_probe() {
  # $1: stub for codex_inside_contributor_container's return
  # $2: stub for codex_userns_available's return
  # $3...: extra env assignments
  local in_container="$1" userns_ok="$2"
  shift 2
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK="codex_inside_contributor_container(){ return ${in_container}; }; codex_userns_available(){ return ${userns_ok}; }" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    "$@" \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" 2>/dev/null
}

# 1. Inside the contributor container with user namespaces BLOCKED — the bug.
#    The container is already the boundary, so stop asking for a nested one.
#    The workspace grant must survive: --add-dir is how build/test tooling
#    reaches the assigned repo and it is orthogonal to the sandbox mode.
codex_probe_blocked="$(run_codex_sandbox_probe 0 1)"
case "$codex_probe_blocked" in
  *"backend_perm_flag=--ask-for-approval on-request --sandbox danger-full-access -c approvals_reviewer=auto_review --add-dir ${WORK_DIR}/workspace"* ) ;;
  *)
    echo "expected a userns-blocked contributor container to drop the unusable nested sandbox; got:" >&2
    echo "$codex_probe_blocked" >&2
    exit 1
    ;;
esac

# 2. Inside the container but user namespaces WORK (operator relaxed seccomp).
#    Nothing is broken, so nothing is downgraded — the nested sandbox is kept
#    with no flag for the operator to remember.
codex_probe_container_ok="$(run_codex_sandbox_probe 0 0)"
case "$codex_probe_container_ok" in
  *"--sandbox workspace-write"* ) ;;
  *)
    echo "expected a container with working user namespaces to keep workspace-write; got:" >&2
    echo "$codex_probe_container_ok" >&2
    exit 1
    ;;
esac

# 3. NOT in the contributor container, user namespaces blocked. This is the
#    one that must never downgrade: on the operator's own host there is no
#    outer boundary, and workspace-write is the only thing between an assigned
#    third-party test suite and their home directory (#4918). A blocked probe
#    is a reason to fail loudly at launch, never a reason to widen access.
codex_probe_host_blocked="$(run_codex_sandbox_probe 1 1)"
case "$codex_probe_host_blocked" in
  *"danger-full-access"* )
    echo "local mode must NEVER be downgraded by the userns probe; got:" >&2
    echo "$codex_probe_host_blocked" >&2
    exit 1
    ;;
esac
case "$codex_probe_host_blocked" in
  *"--sandbox workspace-write"* ) ;;
  *)
    echo "expected local mode to keep workspace-write regardless of the probe; got:" >&2
    echo "$codex_probe_host_blocked" >&2
    exit 1
    ;;
esac

# 4. An explicit HIVE_CODEX_SANDBOX_MODE pins the value and skips the probe
#    entirely — otherwise the escape hatch would be silently overridden in the
#    exact environment an operator is most likely to be debugging.
codex_probe_pinned="$(run_codex_sandbox_probe 0 1 HIVE_CODEX_SANDBOX_MODE=read-only)"
case "$codex_probe_pinned" in
  *"--sandbox read-only"* ) ;;
  *)
    echo "expected an explicit HIVE_CODEX_SANDBOX_MODE to outrank the probe; got:" >&2
    echo "$codex_probe_pinned" >&2
    exit 1
    ;;
esac

# The probe must never leak onto stdout: backend_perm_flag's output is
# word-split into argv by agent-launch.sh, so an explanatory line landing there
# would become a bogus flag. The note is stderr-only, which the 2>/dev/null in
# run_codex_sandbox_probe above has been discarding — assert both halves here.
codex_probe_note="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_PRE_AGENT_HOOK="codex_inside_contributor_container(){ return 0; }; codex_userns_available(){ return 1; }" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=codex \
    bash "${ROOT_DIR}/bin/contributor-agent.sh" 2>&1 >/dev/null
)"
case "$codex_probe_note" in
  *"danger-full-access"*"6653"* ) ;;
  *)
    echo "expected the downgrade to announce the posture actually in effect on stderr; got:" >&2
    echo "$codex_probe_note" >&2
    exit 1
    ;;
esac

# The probe FAILS SAFE, in the direction that keeps the sandbox. If `unshare`
# is not on PATH we cannot DISPROVE namespace availability, and a missing probe
# tool must never be the reason a sandbox is dropped — so codex_userns_available
# reports available and workspace-write stands. Exercised against the real
# function (no stub) with an empty PATH, which is the only honest way to assert
# "the tool is missing" rather than "we said it was".
codex_probe_no_unshare="$(
  bash -c "source '${ROOT_DIR}/config/backends.conf'
           # emptied AFTER sourcing so the shell itself is still resolvable
           PATH='${WORK_DIR}/nonexistent-bin'
           codex_userns_available && echo available || echo blocked"
)"
if [[ "$codex_probe_no_unshare" != "available" ]]; then
  echo "expected a missing unshare to report the namespace AVAILABLE (fail safe, keep the" >&2
  echo "sandbox) rather than blocked; got: $codex_probe_no_unshare" >&2
  exit 1
fi

# ...and the two predicates compose the way the resolution claims: with an
# outer boundary and a blocked namespace the default is danger-full-access,
# and flipping either input alone puts workspace-write back.
codex_probe_matrix="$(
  bash -c "source '${ROOT_DIR}/config/backends.conf'
           for c in 0 1; do
             for u in 0 1; do
               codex_inside_contributor_container(){ return \$c; }
               codex_userns_available(){ return \$u; }
               printf '%s%s=%s ' \"\$c\" \"\$u\" \"\$(codex_default_sandbox_mode 2>/dev/null)\"
             done
           done"
)"
if [[ "$codex_probe_matrix" != "00=workspace-write 01=danger-full-access 10=workspace-write 11=workspace-write " ]]; then
  echo "codex sandbox resolution matrix changed (container,userns => mode); got:" >&2
  echo "$codex_probe_matrix" >&2
  exit 1
fi
echo "contributor-agent codex sandbox probe tests passed"

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

# ── claude host-state denials (#4918) ───────────────────────────────────
#
# The claude family runs permissions-bypassed on the operator's own host. #4918
# is what that cost: an agent running an assigned repo's test suite issued
# `rpm-ostree kargs --append-if-missing=...` against the operator's real
# deployment, and was stopped only by lacking privilege. These cases pin the
# denials that now sit on that path.

claude_flags_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=claude \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
# The bypass flags stay — an unattended agent that stops to ask permission just
# hangs — but the host-state denials ride alongside them.
case "$claude_flags_output" in
  *"backend_perm_flag=--dangerously-skip-permissions --permission-mode bypassPermissions --disallowed-tools "* ) ;;
  *)
    echo "expected claude to carry host-state denials by default; got:" >&2
    echo "$claude_flags_output" >&2
    exit 1
    ;;
esac
# The command from the incident specifically.
case "$claude_flags_output" in
  *"Bash(rpm-ostree:*)"* ) ;;
  *)
    echo "expected claude denials to cover rpm-ostree, the command in #4918; got:" >&2
    echo "$claude_flags_output" >&2
    exit 1
    ;;
esac
# rpm-ostree reached polkit without sudo, so escalation denials alone are not
# the fix — but they must be there too, or `sudo rpm-ostree` walks around it.
case "$claude_flags_output" in
  *"Bash(sudo:*)"*"Bash(pkexec:*)"* ) ;;
  *)
    echo "expected claude denials to cover privilege escalation; got:" >&2
    echo "$claude_flags_output" >&2
    exit 1
    ;;
esac

# THE WORD-SPLIT CONTRACT. agent-launch.sh does `read -r -a PERM_ARGS <<< "$PERM_FLAG"`,
# so the pattern list must be ONE argv word. A space anywhere in it would arrive
# as separate arguments and silently deny nothing — flag present, policy absent.
claude_perm_line="$(printf '%s\n' "$claude_flags_output" | grep '^backend_perm_flag=' | head -1)"
claude_perm_value="${claude_perm_line#backend_perm_flag=}"
read -r -a claude_perm_args <<< "$claude_perm_value"
if [[ "${#claude_perm_args[@]}" -ne 5 ]]; then
  echo "expected claude perm flags to word-split into exactly 5 argv words; got ${#claude_perm_args[@]}:" >&2
  printf '  [%s]\n' "${claude_perm_args[@]}" >&2
  exit 1
fi
case "${claude_perm_args[4]}" in
  *"Bash(rpm-ostree:*)"*"Bash(efibootmgr:*)"* ) ;;
  *)
    echo "expected the whole deny list to survive word-splitting as one argv word; got:" >&2
    echo "  ${claude_perm_args[4]}" >&2
    exit 1
    ;;
esac

# litellm launches the claude binary, so it must be confined identically.
# HIVE_LITELLM_ENDPOINT is required for this backend to resolve at all — without
# it contributor-agent.sh errors out before it ever prints a flag string.
litellm_flags_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_LITELLM_ENDPOINT="https://litellm.test:4000" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=litellm \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$litellm_flags_output" in
  *"Bash(rpm-ostree:*)"* ) ;;
  *)
    echo "expected litellm (which launches the claude binary) to carry the same denials; got:" >&2
    echo "$litellm_flags_output" >&2
    exit 1
    ;;
esac

# The opt-out restores the pre-#4918 posture, and must drop the denials
# entirely rather than leaving a dangling --disallowed-tools with no argument.
claude_bypass_output="$(
  env -i \
    PATH="${PATH}" \
    HOME="$HOME_DIR" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_ENTRYPOINT_HOOK_DIR="${WORK_DIR}/empty-entrypoint.d" \
    HIVE_CLAUDE_DANGEROUSLY_ALLOW_HOST_STATE=1 \
    HIVE_WORKSPACE_DIR="${WORK_DIR}/workspace" \
    HIVE_CONTRIBUTOR_AGENT_TEST_RESOLVE_BACKEND=1 \
    AGENT_BACKEND=claude \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
)"
case "$claude_bypass_output" in
  *"backend_perm_flag=--dangerously-skip-permissions --permission-mode bypassPermissions"* ) ;;
  *)
    echo "expected the claude opt-out to restore the plain bypass posture; got:" >&2
    echo "$claude_bypass_output" >&2
    exit 1
    ;;
esac
case "$claude_bypass_output" in
  *"--disallowed-tools"* )
    echo "claude opt-out must drop --disallowed-tools entirely; got:" >&2
    echo "$claude_bypass_output" >&2
    exit 1
    ;;
esac

# THE SHELL-LINE CONTRACT. Not every consumer word-splits into argv: the
# Justfile's contribute-hive local mode, this script's own interactive tmux
# launch, and supervisor.sh's generated launcher all paste the flag string
# into text a shell re-PARSES. There the raw deny list's (),* are shell
# syntax, and the launch died at the first paren before the CLI ever started:
#   -bash: syntax error near unexpected token `('
# backend_perm_flag_shell is the spelling for those consumers. Three pins:
# it must parse as shell source, it must reduce to the SAME argv as the raw
# word-split contract above, and the raw spelling must NOT parse — if it
# ever does, the two variants have converged and one should be deleted.
source "${ROOT_DIR}/config/backends.conf"

shell_perm_value="$(backend_perm_flag_shell claude)"
if ! bash -n -c "true ${shell_perm_value}" 2>/dev/null; then
  echo "backend_perm_flag_shell output must parse as shell source; got:" >&2
  echo "  ${shell_perm_value}" >&2
  exit 1
fi

mapfile -t shell_parsed < <(eval "printf '%s\n' ${shell_perm_value}")
if [[ "${#shell_parsed[@]}" -ne 5 || "${shell_parsed[4]}" != "${claude_perm_args[4]}" ]]; then
  echo "shell-quoted flags must reduce to the same argv as the raw word-split contract; got ${#shell_parsed[@]} words:" >&2
  printf '  [%s]\n' "${shell_parsed[@]}" >&2
  exit 1
fi

if bash -n -c "true ${claude_perm_value}" 2>/dev/null; then
  echo "the RAW claude perm flags unexpectedly parse as shell source — the _shell variant is redundant; got:" >&2
  echo "  ${claude_perm_value}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# seed_claude_config must run for OAuth/subscription claude, not only when an
# ANTHROPIC_API_KEY is delivered.
#
# Claude Code needs BOTH halves of its auth state: the token in
# ${HOME}/.claude/.credentials.json and the session state in
# ${HOME}/.claude.json. A contributor container mounts a staged ${HOME}/.claude
# — so the token is fine — and the CLI writes its own ${HOME}/.claude.json,
# which carries oauthAccount but no hasCompletedOnboarding. Without the seed
# the CLI re-runs onboarding and parks the pane on "Select login method" with a
# perfectly valid credential beside it. The old condition seeded litellm and
# API-key claude only, so the default `just contribute-hive claude` path was
# the single configuration that never got the flag.
# ---------------------------------------------------------------------------

seed_home="${WORK_DIR}/seed-home"
seed_config="${seed_home}/.config/hive"
mkdir -p "$seed_config"

run_seed() {
  # $1: AGENT_BACKEND. Any remaining args are extra `env` assignments.
  local backend="$1"; shift
  env -i \
    PATH="${PATH}" \
    HOME="$seed_home" \
    HIVE_REGISTRATION_TOKEN="test-token" \
    HIVE_CONTRIBUTOR_AGENT_TEST_SEED_CLAUDE_CONFIG=1 \
    AGENT_BACKEND="$backend" \
    "$@" \
    bash "${ROOT_DIR}/bin/contributor-agent.sh"
}

seed_key() {
  python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2]))" \
    "${seed_home}/.claude.json" "$1" 2>/dev/null
}

# 1. Subscription/OAuth claude — no ANTHROPIC_API_KEY anywhere. This is the
#    regression: it used to leave .claude.json untouched.
rm -f "${seed_home}/.claude.json"
printf '%s' '{"oauthAccount":{"emailAddress":"c@example.invalid"},"userID":"u1"}' \
  >"${seed_home}/.claude.json"
run_seed claude >/dev/null 2>&1 || true

if [[ "$(seed_key hasCompletedOnboarding)" != "True" ]]; then
  echo "OAuth claude must get hasCompletedOnboarding seeded; got:" >&2
  cat "${seed_home}/.claude.json" >&2
  exit 1
fi

# The CLI's own keys survive the merge — the seed adds, it does not replace.
if [[ "$(seed_key userID)" != "u1" ]]; then
  echo "seeding must MERGE into an existing .claude.json, not overwrite it" >&2
  cat "${seed_home}/.claude.json" >&2
  exit 1
fi

# No key delivered means no customApiKeyResponses invented for one.
if [[ "$(seed_key customApiKeyResponses)" != "None" ]]; then
  echo "customApiKeyResponses must stay absent when no ANTHROPIC_API_KEY is set" >&2
  cat "${seed_home}/.claude.json" >&2
  exit 1
fi

# 2. An operator-supplied claude-config.json is still copied in first, and the
#    seed merges on top of it rather than discarding it.
rm -f "${seed_home}/.claude.json"
printf '%s' '{"operatorKey":"kept"}' >"${seed_config}/claude-config.json"
run_seed claude >/dev/null 2>&1 || true

if [[ "$(seed_key operatorKey)" != "kept" ]] || [[ "$(seed_key hasCompletedOnboarding)" != "True" ]]; then
  echo "claude-config.json must be copied in and then merged with the seed; got:" >&2
  cat "${seed_home}/.claude.json" >&2
  exit 1
fi
rm -f "${seed_config}/claude-config.json"

# 3. The API-key path (#5103) is unchanged: the key is pre-approved, in full
#    and as its last 20 chars, because matching differs across CLI versions.
rm -f "${seed_home}/.claude.json"
api_key="sk-ant-test-0123456789abcdefghij"
run_seed claude ANTHROPIC_API_KEY="$api_key" >/dev/null 2>&1 || true

approved="$(python3 -c "
import json
d = json.load(open('${seed_home}/.claude.json'))
print(','.join(d.get('customApiKeyResponses', {}).get('approved', [])))
" 2>/dev/null)"
if [[ "$approved" != "${api_key},${api_key: -20}" ]]; then
  echo "ANTHROPIC_API_KEY must be approved in full and as its last 20 chars; got: ${approved}" >&2
  exit 1
fi

# 4. litellm keeps its seeding too. It refuses to start without an endpoint,
#    so supply one; the seeding itself never consults it.
rm -f "${seed_home}/.claude.json"
run_seed litellm HIVE_LITELLM_ENDPOINT="https://litellm.invalid:4000" >/dev/null 2>&1 || true
if [[ "$(seed_key hasCompletedOnboarding)" != "True" ]]; then
  echo "litellm must still get hasCompletedOnboarding seeded" >&2
  exit 1
fi

# 5. hivecommons/hive#7866: the interactive tmux pane must advertise true colour.
#    The test hook exits before the tmux session is created, so pin the contract
#    at the source: COLORTERM is exported (operator value honoured) before
#    `tmux new-session`, and tmux is told to pass RGB through.
agent_src="$(dirname "$0")/contributor-agent.sh"
colorterm_line="$(grep -n 'export COLORTERM="\${COLORTERM:-truecolor}"' "$agent_src" | cut -d: -f1 | head -1)"
new_session_line="$(grep -n 'tmux new-session -d -s "\$TMUX_SESSION"' "$agent_src" | cut -d: -f1 | head -1)"
if [[ -z "$colorterm_line" || -z "$new_session_line" || "$colorterm_line" -ge "$new_session_line" ]]; then
  echo "#7866: COLORTERM must be exported (defaulting to truecolor) BEFORE tmux new-session; got colorterm=${colorterm_line:-none} new-session=${new_session_line:-none}" >&2
  exit 1
fi
if ! grep -q "tmux set-option -s -a terminal-overrides ',\*:Tc'" "$agent_src"; then
  echo "#7866: tmux must be told to pass RGB through (terminal-overrides ,*:Tc)" >&2
  exit 1
fi

echo "contributor-agent codex + claude contract tests passed"
