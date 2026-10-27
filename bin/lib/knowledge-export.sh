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
    knowledge_refresh_failing=0
    [[ -e "$AGENT_MD" ]] || knowledge_refresh_failing=1
    while true; do
      sleep "$KNOWLEDGE_REFRESH_SECS"
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
