#!/bin/bash
# knowledge-export.sh — fetch and refresh the hub knowledge export (agent.md).
# Sourced by bin/contributor-agent.sh (container) and the Justfile
# contribute-hive local branch. Defines functions only.

knowledge_export_looks_valid() {
  local path="$1"
  local first_line

  [[ -s "$path" ]] || return 1
  IFS= read -r first_line < "$path" || return 1
  [[ "$first_line" == "# Agent Knowledge" ]] || return 1
  grep -qx "This file is auto-generated from the hive knowledge base\\." "$path"
}

hub_knowledge_export_urls() {
  local hubs="${1:-$HIVE_HUB}"
  local hub

  while IFS= read -r hub; do
    hub="${hub#"${hub%%[![:space:]]*}"}"
    hub="${hub%"${hub##*[![:space:]]}"}"
    [[ -n "$hub" ]] || continue
    hub="${hub/#wss:\/\//https://}"
    hub="${hub/#ws:\/\//http://}"
    hub="${hub%/contribute}"
    printf '%s/api/knowledge/export\n' "$hub"
  done < <(printf '%s\n' "$hubs" | tr ',' '\n')
}

# hub_registration_tokens splits HIVE_REGISTRATION_TOKEN the same way
# hub_knowledge_export_urls splits HIVE_HUB, so index N of one lines up with
# index N of the other (contributor.env's documented one-token-per-hub
# ordering). Each knowledge fetch must use only its own hub's token
# (hivecommons/hive#9442) - the combined comma-separated value is never a
# valid registration token for any single hub.
hub_registration_tokens() {
  local tokens="${1:-$HIVE_REGISTRATION_TOKEN}"
  local token

  while IFS= read -r token; do
    token="${token#"${token%%[![:space:]]*}"}"
    token="${token%"${token##*[![:space:]]}"}"
    printf '%s\n' "$token"
  done < <(printf '%s\n' "$tokens" | tr ',' '\n')
}

# knowledge_export_token_for_index picks the bearer for the hub at index N of
# KNOWLEDGE_EXPORT_TOKENS, never falling back to the combined comma-separated
# HIVE_REGISTRATION_TOKEN: with exactly one configured token every hub gets it
# (the single-token fleet case), while with several tokens a hub beyond the end
# of the list gets an EMPTY token and its fetch is skipped — sending the joined
# value there would hand every other hub's registration token to that hub.
knowledge_export_token_for_index() {
  local index="$1"
  if [[ "${#KNOWLEDGE_EXPORT_TOKENS[@]}" -eq 1 ]]; then
    printf '%s\n' "${KNOWLEDGE_EXPORT_TOKENS[0]}"
    return 0
  fi
  printf '%s\n' "${KNOWLEDGE_EXPORT_TOKENS[$index]:-}"
}

# KNOWLEDGE_FETCH_REASON is set by fetch_knowledge_export whenever it returns
# non-zero: one line naming the URL, the HTTP status (or the curl exit code
# when no status came back) and why the body was rejected. Before #8294 the
# status was captured and thrown away, so a 302 to the login page, a 200 that
# was really the hub's HTML landing page, a 404 and a network timeout all
# produced the same "Agent knowledge unavailable" line.
KNOWLEDGE_FETCH_REASON=""

# knowledge_fetch_failure_reason prints the one-line diagnosis for a failed
# fetch. Arguments: url, curl exit code, HTTP status, redirect target (empty
# when none), path of the body curl wrote.
knowledge_fetch_failure_reason() {
  local url="$1" curl_rc="$2" http_code="$3" redirect_url="$4" body="$5"
  local status reason first_line=""

  if [[ "$curl_rc" -ne 0 ]]; then
    status="no response (curl exit ${curl_rc})"
    reason="the request failed before a status was received"
  else
    status="HTTP ${http_code}"
    case "$http_code" in
      200)
        if [[ -s "$body" ]]; then
          IFS= read -r first_line < "$body" || true
        fi
        case "$first_line" in
          '<!DOCTYPE'*|'<!doctype'*|'<html'*|'<HTML'*)
            # The hub (hive.hivecommons.dev) has no knowledge-export handler;
            # the request falls through to its single-page-app catch-all and
            # comes back 200 with the landing page. This is what an unset or
            # hub-pointed HIVE_HUB looks like - the default in the Justfile.
            reason="body is an HTML page, not a knowledge export; HIVE_HUB points at the hub, not a hosted spoke; knowledge export is served by the spoke"
            ;;
          *)
            reason="body is not a knowledge export (expected it to start with '# Agent Knowledge')"
            ;;
        esac
        ;;
      301|302|303|307|308)
        # Redirects are deliberately not followed (#3013): the login page must
        # never be written into agent.md. A hosted spoke behind the hub's
        # auth-proxy answers this way when the path is not allowlisted.
        case "$redirect_url" in
          *login*) reason="redirected to the login page instead of serving the export (${redirect_url})" ;;
          "") reason="redirected instead of serving the export" ;;
          *) reason="redirected instead of serving the export (${redirect_url})" ;;
        esac
        ;;
      *)
        reason="status is not 200"
        ;;
    esac
  fi

  printf 'knowledge export fetch failed: %s -> %s: %s\n' "$url" "$status" "$reason"
}

fetch_knowledge_export() {
  local url="$1"
  local dest="$2"
  local token="${3:-}"
  local tmp_body tmp_status http_code redirect_url curl_rc
  local -a proto_redir_args

  KNOWLEDGE_FETCH_REASON=""
  if [[ -z "$token" ]]; then
    # Never substitute the combined comma-separated HIVE_REGISTRATION_TOKEN
    # here: it carries every hub's registration token, and presenting it to
    # one hub discloses the others' credentials to that hub.
    KNOWLEDGE_FETCH_REASON="no registration token configured for this hub (HIVE_REGISTRATION_TOKEN has fewer entries than HIVE_HUB)"
    return 1
  fi
  mkdir -p "$(dirname "$dest")"
  tmp_body=$(mktemp "${dest}.tmp.XXXXXX") || return 1
  tmp_status=$(mktemp "${dest}.status.XXXXXX") || {
    rm -f "$tmp_body"
    return 1
  }

  proto_redir_args=(--proto-redir "=http,https")
  if [[ "$url" == https://* ]]; then
    proto_redir_args=(--proto-redir "=https")
  fi

  curl_rc=0
  curl --silent --show-error "${proto_redir_args[@]}" \
    --max-time "${KNOWLEDGE_FETCH_MAX_TIME:-15}" \
    --header "Authorization: Bearer ${token}" \
    --output "$tmp_body" \
    --write-out "%{http_code} %{redirect_url}" \
    "$url" > "$tmp_status" 2>/dev/null || curl_rc=$?

  http_code=""
  redirect_url=""
  read -r http_code redirect_url < "$tmp_status" 2>/dev/null || true
  http_code="${http_code:-000}"
  if [[ "$curl_rc" -eq 0 && "$http_code" == "200" ]] && knowledge_export_looks_valid "$tmp_body"; then
    if [[ -e "$dest" ]] && cmp -s "$tmp_body" "$dest"; then
      rm -f "$tmp_body"
    else
      mv "$tmp_body" "$dest"
    fi
    rm -f "$tmp_status"
    return 0
  fi

  KNOWLEDGE_FETCH_REASON="$(knowledge_fetch_failure_reason "$url" "$curl_rc" "$http_code" "$redirect_url" "$tmp_body")"
  rm -f "$tmp_body" "$tmp_status"
  return 1
}


# knowledge_export_sync_start fetches the knowledge export into $1 and keeps
# refreshing it in the background every KNOWLEDGE_REFRESH_SECS (10 minutes).
# Shared by the container entrypoint (bin/contributor-agent.sh) and
# `just contribute-hive local`, so both paths use the same endpoint, the same
# per-hub bearer and the same refresh cadence. Needs HIVE_HUB and
# HIVE_REGISTRATION_TOKEN in the environment. Failed fetches are logged with
# the diagnosis; the file is left absent (never a placeholder) when no hub
# has served a valid export. Sets KNOWLEDGE_REFRESH_PID.
knowledge_export_sync_start() {
  local AGENT_MD="$1"
  local KNOWLEDGE_EXPORT_URL="" KNOWLEDGE_EXPORT_CANDIDATE_URL KNOWLEDGE_EXPORT_INDEX
  local -a KNOWLEDGE_EXPORT_URLS=() KNOWLEDGE_EXPORT_TOKENS=()

  while IFS= read -r KNOWLEDGE_EXPORT_CANDIDATE_URL; do
    KNOWLEDGE_EXPORT_URLS+=("$KNOWLEDGE_EXPORT_CANDIDATE_URL")
  done < <(hub_knowledge_export_urls "$HIVE_HUB")
  while IFS= read -r KNOWLEDGE_EXPORT_CANDIDATE_URL; do
    KNOWLEDGE_EXPORT_TOKENS+=("$KNOWLEDGE_EXPORT_CANDIDATE_URL")
  done < <(hub_registration_tokens "$HIVE_REGISTRATION_TOKEN")

  for KNOWLEDGE_EXPORT_INDEX in "${!KNOWLEDGE_EXPORT_URLS[@]}"; do
    KNOWLEDGE_EXPORT_CANDIDATE_URL="${KNOWLEDGE_EXPORT_URLS[$KNOWLEDGE_EXPORT_INDEX]}"
    if fetch_knowledge_export "$KNOWLEDGE_EXPORT_CANDIDATE_URL" "$AGENT_MD" "$(knowledge_export_token_for_index "$KNOWLEDGE_EXPORT_INDEX")"; then
      KNOWLEDGE_EXPORT_URL="$KNOWLEDGE_EXPORT_CANDIDATE_URL"
      echo "Agent knowledge downloaded from ${KNOWLEDGE_EXPORT_URL} ($(wc -l < "$AGENT_MD") lines)"
      break
    fi
    echo "Agent knowledge unavailable from ${KNOWLEDGE_EXPORT_CANDIDATE_URL}. ${KNOWLEDGE_FETCH_REASON}"
  done
  if [[ -z "$KNOWLEDGE_EXPORT_URL" ]]; then
    rm -f "$AGENT_MD"
    echo "Agent knowledge unavailable; ${AGENT_MD} left absent."
  fi

  # Every failed refresh is logged with the same diagnosis as the startup
  # fetch, and a refresh that succeeds after a failure says so - before #8294
  # this loop swallowed both, so a contributor could run for days with no
  # knowledge and nothing in the log after the one startup line.
  local KNOWLEDGE_REFRESH_SECS=600
  (
    # Killing this subshell must take its in-flight sleep with it: a bare
    # `sleep` is a separate process that would otherwise outlive the
    # contributor for up to KNOWLEDGE_REFRESH_SECS after shutdown.
    knowledge_refresh_sleep_pid=""
    trap '[[ -n "$knowledge_refresh_sleep_pid" ]] && kill "$knowledge_refresh_sleep_pid" 2>/dev/null; exit 0' TERM INT
    knowledge_refresh_failing=0
    [[ -e "$AGENT_MD" ]] || knowledge_refresh_failing=1
    while true; do
      sleep "$KNOWLEDGE_REFRESH_SECS" &
      knowledge_refresh_sleep_pid=$!
      wait "$knowledge_refresh_sleep_pid" || true
      knowledge_refresh_sleep_pid=""
      knowledge_refresh_succeeded=0
      for KNOWLEDGE_EXPORT_INDEX in "${!KNOWLEDGE_EXPORT_URLS[@]}"; do
        KNOWLEDGE_EXPORT_CANDIDATE_URL="${KNOWLEDGE_EXPORT_URLS[$KNOWLEDGE_EXPORT_INDEX]}"
        if fetch_knowledge_export "$KNOWLEDGE_EXPORT_CANDIDATE_URL" "$AGENT_MD" "$(knowledge_export_token_for_index "$KNOWLEDGE_EXPORT_INDEX")"; then
          KNOWLEDGE_EXPORT_URL="$KNOWLEDGE_EXPORT_CANDIDATE_URL"
          knowledge_refresh_succeeded=1
          if [[ "$knowledge_refresh_failing" -eq 1 ]]; then
            echo "Agent knowledge refresh recovered from ${KNOWLEDGE_EXPORT_URL}; ${AGENT_MD} installed ($(wc -l < "$AGENT_MD") lines)"
            knowledge_refresh_failing=0
          fi
          break
        fi
        echo "Agent knowledge refresh unavailable from ${KNOWLEDGE_EXPORT_CANDIDATE_URL}. ${KNOWLEDGE_FETCH_REASON}" >&2
      done
      if [[ "$knowledge_refresh_succeeded" -eq 0 ]]; then
        if [[ -e "$AGENT_MD" ]]; then
          echo "Agent knowledge refresh failed; keeping the previous ${AGENT_MD}." >&2
        else
          echo "Agent knowledge refresh failed; ${AGENT_MD} still absent." >&2
        fi
        knowledge_refresh_failing=1
      fi
    done
  ) &
  KNOWLEDGE_REFRESH_PID=$!
}

DEFAULT_CONTRIBUTOR_KNOWLEDGE_TOKEN_BUDGET="${DEFAULT_CONTRIBUTOR_KNOWLEDGE_TOKEN_BUDGET:-12000}"

# budget_backend_knowledge writes the pi-budgeted view of $2 next to the
# links, in $3 (default $HOME), and prints the path the links should target.
budget_backend_knowledge() {
  local backend="$1"
  local agent_md="$2"
  local dest_dir="${3:-$HOME}"
  local budgeted_agent_md

  if [[ "$backend" != "pi" || ! -s "$agent_md" ]]; then
    printf '%s\n' "$agent_md"
    return 0
  fi

  budgeted_agent_md="${dest_dir}/agent.pi-context.md"
  HIVE_KNOWLEDGE_SOURCE="$agent_md" \
  HIVE_KNOWLEDGE_DEST="$budgeted_agent_md" \
  HIVE_KNOWLEDGE_FULL_EXPORT_PATH="${agent_md/#$HOME/\~}" \
  HIVE_KNOWLEDGE_TOKEN_BUDGET="${HIVE_CONTRIBUTOR_KNOWLEDGE_TOKEN_BUDGET:-$DEFAULT_CONTRIBUTOR_KNOWLEDGE_TOKEN_BUDGET}" \
  python3 <<'PY'
import os
import re
from pathlib import Path

DEFAULT_TOKEN_BUDGET = 12000
CHARS_PER_TOKEN = 4
TRUNCATION_MARKER = (
    "\n\n[Hive knowledge truncated: token budget reached. Full export: {full_path}. "
    "Fetch more on demand with `hive knowledge` or the Hive MCP knowledge tool.]\n"
)

def parse_budget(raw):
    try:
        value = int(str(raw).strip())
    except Exception:
        return DEFAULT_TOKEN_BUDGET
    return value if value > 0 else DEFAULT_TOKEN_BUDGET

def estimate_tokens(text):
    return (len(text) + CHARS_PER_TOKEN - 1) // CHARS_PER_TOKEN

def task_terms(env):
    text = " ".join(env.get(name, "") for name in (
        "HIVE_TASK_TITLE",
        "HIVE_TASK_BODY",
        "HIVE_ISSUE_TITLE",
        "HIVE_ISSUE_BODY",
        "HIVE_ASSIGNMENT_TITLE",
        "HIVE_ASSIGNMENT_BODY",
    ))
    return {
        token.lower()
        for token in re.findall(r"[A-Za-z0-9][A-Za-z0-9_.-]{3,}", text)
        if not token.isdigit()
    }

def repo_needles(env):
    out = []
    for name in ("HIVE_REPO", "HIVE_REPOSITORY", "HIVE_TASK_REPO", "GITHUB_REPOSITORY"):
        value = env.get(name, "").strip().lower()
        if not value:
            continue
        out.append(value)
        if "/" in value:
            out.append(value.rsplit("/", 1)[-1])
    return [value for i, value in enumerate(out) if value and value not in out[:i]]

def split_entries(text):
    lines = text.splitlines(keepends=True)
    prefix_lines = []
    entries = []
    current_section = ""
    current_entry = None
    for line in lines:
        if line.startswith("## ") and not line.startswith("### "):
            if current_entry is not None:
                entries.append((len(entries), current_section, "".join(current_entry)))
                current_entry = None
            current_section = line
            continue
        if line.startswith("### "):
            if current_entry is not None:
                entries.append((len(entries), current_section, "".join(current_entry)))
            current_entry = [line]
            continue
        if current_entry is None:
            prefix_lines.append(line)
        else:
            current_entry.append(line)
    if current_entry is not None:
        entries.append((len(entries), current_section, "".join(current_entry)))
    prefix = "".join(prefix_lines)
    if entries:
        return prefix, entries
    return text, []

def render():
    source = Path(os.environ["HIVE_KNOWLEDGE_SOURCE"])
    dest = Path(os.environ["HIVE_KNOWLEDGE_DEST"])
    full_path = os.environ.get("HIVE_KNOWLEDGE_FULL_EXPORT_PATH", "~/agent.md")
    source_text = source.read_text(encoding="utf-8")
    budget = parse_budget(os.environ.get("HIVE_KNOWLEDGE_TOKEN_BUDGET"))
    max_chars = budget * CHARS_PER_TOKEN
    marker = TRUNCATION_MARKER.format(full_path=full_path)
    pointer = (
        "\nStartup context is budgeted for this backend. "
        f"Budget: ~{budget} tokens. "
        f"Full export: {full_path}; fetch more with `hive knowledge` or the Hive MCP knowledge tool.\n\n"
    )
    prefix, entries = split_entries(source_text)
    header = prefix.rstrip() + pointer
    needles = repo_needles(os.environ)
    terms = task_terms(os.environ)

    def score(entry):
        idx, _section, body = entry
        lower = body.lower()
        repo_score = 10000 if any(needle in lower for needle in needles) else 0
        term_score = sum(1 for term in terms if term in lower)
        return (-repo_score, -term_score, idx)

    entries = sorted(entries, key=score)
    out = header
    seen_sections = set()
    truncated = False
    for _idx, section, body in entries:
        candidate = ""
        if section and section not in seen_sections:
            candidate += section
        candidate += body
        if len(out) + len(candidate) <= max_chars:
            out += candidate
            if section:
                seen_sections.add(section)
            continue
        remaining = max_chars - len(out) - len(marker)
        if remaining > 80:
            out += candidate[:remaining].rstrip()
        truncated = True
        break
    if not entries and len(out) > max_chars:
        remaining = max_chars - len(marker)
        out = out[:max(0, remaining)].rstrip()
        truncated = True
    if truncated or len(out) > max_chars:
        marker_budget = max_chars - len(marker)
        out = out[:max(0, marker_budget)].rstrip() + marker
    if estimate_tokens(out) > budget:
        marker_budget = max_chars - len(marker)
        out = out[:max(0, marker_budget)].rstrip() + marker
    dest.write_text(out, encoding="utf-8")

render()
PY
  printf '%s\n' "$budgeted_agent_md"
}

# link_backend_knowledge symlinks the knowledge export $2 under every context
# filename backend $1 reads, inside $3. The container entrypoint links into
# $HOME (the CLI's home inside the image); `just contribute-hive local` links
# into $HIVE_AGENT_CWD, the directory the CLI is started from, because on the
# host $HOME is the user's real home and the CLIs read these names from their
# working directory (#14521). Backend config files are only seeded when the
# destination is $HOME, i.e. in the container.
link_backend_knowledge() {
  local backend="$1"
  local agent_md="$2"
  local dest_dir="${3:-$HOME}"
  local backend_agent_md="$agent_md"

  mkdir -p "$dest_dir"

  case "$backend" in
    claude|litellm)
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    copilot)
      [[ "$dest_dir" == "$HOME" ]] && mkdir -p "${HOME}/.copilot"
      ln -sf "$agent_md" "${dest_dir}/copilot-instructions.md"
      ln -sf "$agent_md" "${dest_dir}/COPILOT.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    goose)
      # Goose reads AGENTS.md and .goosehints; .goose-instructions.md / CLAUDE.md
      # are NOT names it looks for (unless CONTEXT_FILE_NAMES is overridden), so
      # without these two the knowledge export downloaded above never reached the
      # model. Keep the old names for backward-compat, but add the ones Goose
      # actually reads. (kubestellar/hive#2393 item 1.)
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/.goosehints"
      ln -sf "$agent_md" "${dest_dir}/.goose-instructions.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      # The provider/model seed is container-only: on the host the user's own
      # ~/.config/goose is theirs, and the links above live in HIVE_AGENT_CWD.
      if [[ "$dest_dir" == "$HOME" ]]; then
        mkdir -p "${HOME}/.config/goose"
        # Write the config only if the contributor/derived image hasn't provided one,
        # so a downstream can add Goose extensions (MCP servers) or other settings
        # without it being clobbered on every start. (kubestellar/hive#2393 item 3.)
        if [ ! -f "${HOME}/.config/goose/config.yaml" ]; then
          cat > "${HOME}/.config/goose/config.yaml" <<GOOSECFG
GOOSE_PROVIDER: ${GOOSE_PROVIDER:-ollama}
GOOSE_MODEL: ${GOOSE_MODEL:-phi4}
GOOSECFG
          echo "Goose config: provider=${GOOSE_PROVIDER:-ollama} model=${GOOSE_MODEL:-phi4}"
        else
          echo "Goose config: keeping existing ${HOME}/.config/goose/config.yaml"
        fi
      fi
      ;;
    omp)
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    codex)
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    pi)
      backend_agent_md="$(budget_backend_knowledge "$backend" "$agent_md" "$dest_dir")"
      ln -sf "$backend_agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$backend_agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    bob)
      # Bobshell 1.0.6 defaults to AGENTS.md and documents ~/.bob/AGENTS.md as
      # its global context file. It does not load ~/CLAUDE.md. Keep the latter
      # compatibility link while also installing the export where Bob reads it.
      mkdir -p "${dest_dir}/.bob"
      ln -sf "$agent_md" "${dest_dir}/.bob/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    agy)
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    opencode)
      # opencode reads AGENTS.md (https://opencode.ai/docs/rules/). Keep the
      # CLAUDE.md compatibility link too, matching codex/pi above.
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    kilo)
      # Kilo reads AGENTS.md-compatible project instructions.
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    openhands)
      # OpenHands reads AGENTS.md at the repository root and also recognizes
      # CLAUDE.md as model-specific context (docs.openhands.dev, Skills).
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    muse)
      # Muse Code reads AGENTS.md (its settings panel names "Agents.md", and
      # AGENTS.md dominates the binary's own instruction-file strings). Keep
      # the CLAUDE.md compatibility link too, matching codex/opencode/kilo.
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    omnigent)
      # Omnigent orchestrates Claude, Codex, Cursor, Pi, and custom harnesses.
      # Link both AGENTS.md and CLAUDE.md for harness compatibility.
      ln -sf "$agent_md" "${dest_dir}/AGENTS.md"
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
    *)
      ln -sf "$agent_md" "${dest_dir}/CLAUDE.md"
      ;;
  esac
}
