#!/usr/bin/env bash
# Regression coverage for kubestellar/hive#2833.
#
# Run: bash bin/contributor-agent.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK_DIR="${ROOT_DIR}/.contributor-agent-test-work-$$"
HOOK_DIR="${WORK_DIR}/entrypoint.d"
HOME_DIR="${WORK_DIR}/home"
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
