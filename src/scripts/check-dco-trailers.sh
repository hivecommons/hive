#!/usr/bin/env bash
# Check recent protected-branch commits for a DCO trailer whose email matches
# the commit author. GitHub noreply addresses are accepted only when the
# GitHub commits API identifies the commit author as that exact login. A
# noreply AUTHOR (the shape GitHub's squash writes for a private-email PR
# author) signed off under another address is accepted only when the branch's
# own history supplies local evidence that the two identities are one person;
# see same_person_noreply_evidence below (#15401).
# Maintainer override identities can be supplied with DCO_ALLOWLIST_EMAILS, but
# the default is intentionally empty so normal runs require the squash author's
# own sign-off.
#
# Two override instruments, deliberately different in blast radius:
#
#   DCO_ALLOWLIST_EMAILS  accepts an IDENTITY. It only helps a commit that HAS
#                         a trailer under another address, and it keeps
#                         accepting that address on every future commit.
#   DCO_WAIVED_COMMITS    accepts ONE HISTORICAL COMMIT by full SHA. It is the
#                         only instrument that can clear a missing-signoff, and
#                         it cannot widen: a SHA names a commit that already
#                         exists, so a waiver can never pre-approve future work.
#
# A waiver exists because a protected branch cannot be repaired. Policy forbids
# rewriting another contributor's commit or adding a sign-off on their behalf
# (see src/docs/v5-sync-policy.md and hivecommons/hive#6329), so for a commit
# already merged without a trailer there is no fix — only a recorded maintainer
# disposition, which is exactly what v5-sync-policy.md asks for ("listing every
# affected SHA and the reason the override is acceptable"). Without this the
# monitor's only route back to green is waiting for the commit to fall out of
# the window, during which every hourly run pages on a condition nobody can
# act on and a genuinely NEW failure is indistinguishable from the known one.
#
# Waivers are never silent: every applied waiver is printed, and a waiver whose
# commit is in the window but no longer failing is reported as stale so it gets
# removed instead of lingering as an unexamined hole.
set -u -o pipefail

# Shared sign-off identity helpers (lower, GitHub noreply matching, author
# login lookup). Kept in one file so the pre-merge gate and this monitor can
# never drift apart on how they read a sign-off email — the drift that let
# #6971 through the pre-merge gate while this monitor would have caught it.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=src/scripts/lib-dco-identity.sh
. "$SCRIPT_DIR/lib-dco-identity.sh"

limit="${1:-${DCO_COMMIT_LIMIT:-50}}"
ref="${2:-${DCO_REF:-HEAD}}"
allowlist_raw="${DCO_ALLOWLIST_EMAILS:-}"
waived_raw="${DCO_WAIVED_COMMITS:-}"
skip_bots="${DCO_SKIP_BOT_AUTHORS:-true}"
same_person_window="${DCO_SAME_PERSON_WINDOW:-500}"
same_person_extra_refs="${DCO_SAME_PERSON_EVIDENCE_REFS:-}"

usage() {
  cat >&2 <<'USAGE'
Usage: check-dco-trailers.sh [commit-limit] [ref-or-range]

  ref-or-range may be a commit-ish (inspect that many recent commits) or a
  two-dot range <before>..<after> (inspect exactly the commits in the range).

Environment:
  DCO_COMMIT_LIMIT       Number of recent commits to inspect (default: 50)
  DCO_REF                Ref to inspect when no ref argument is supplied (default: HEAD)
  DCO_ALLOWLIST_EMAILS   Optional comma/space/newline-separated sign-off emails accepted
                         in addition to the author email. Empty by default.
  DCO_WAIVED_COMMITS     Optional comma/space/newline-separated FULL 40-character commit
                         SHAs with a recorded maintainer DCO disposition. A waived commit
                         is reported as WAIVED instead of FAIL and does not fail the run.
                         Abbreviated SHAs are rejected. Empty by default.
  DCO_SKIP_BOT_AUTHORS   true/false; skip bot-authored commits by default.
  DCO_GITHUB_REPOSITORY  Optional owner/repo used for GitHub noreply author
                         lookups. Defaults to GITHUB_REPOSITORY or origin.
  DCO_AUTHOR_LOGIN_MAP   Optional comma/space/newline-separated SHA=login map
                         used before the GitHub API, mainly for tests/offline
                         runs. Empty by default.
  DCO_SAME_PERSON_WINDOW Number of commits of history searched for LOCAL evidence
                         that a GitHub-noreply author and a differently-addressed
                         sign-off are the same person (default: 500; 0 disables).
  DCO_SAME_PERSON_EVIDENCE_REFS
                         Optional extra refs (space-separated) whose history is
                         searched for that evidence in addition to the inspected
                         ref, e.g. a fetched refs/pull/N/head. Empty by default.
USAGE
}

case "$limit" in
  ''|*[!0-9]*) echo "commit limit must be a positive integer: $limit" >&2; usage; exit 2 ;;
esac
if [ "$limit" -eq 0 ]; then
  echo "commit limit must be greater than zero" >&2
  usage
  exit 2
fi
case "$same_person_window" in
  ''|*[!0-9]*) echo "DCO_SAME_PERSON_WINDOW must be a non-negative integer: $same_person_window" >&2; exit 2 ;;
esac

# `ref` may be a single commit-ish (the rolling-window form used by the hourly
# monitor) or a two-dot RANGE like <before>..<after>. The range form exists for
# the push gate (#6756): `git rev-list -n N <tip>` walks back from the tip in
# commit order, which is NOT the set a push introduced. On a merge push — every
# v4->v5 sync is one — that walk interleaves commits that were already on the
# branch, so a tip-plus-count scan reports history the push did not touch.
# Passing the real <before>..<after> makes the scan exactly the pushed commits.
case "$ref" in
  *...*)
    echo "three-dot (symmetric difference) ranges are not supported: $ref" >&2
    exit 2
    ;;
  *..*)
    range_left="${ref%%..*}"
    range_right="${ref##*..}"
    # An open-ended range silently means HEAD, which would hide a typo.
    if [ -z "$range_left" ] || [ -z "$range_right" ]; then
      echo "range endpoints must both be specified: $ref" >&2
      exit 2
    fi
    for range_end in "$range_left" "$range_right"; do
      if ! git rev-parse --verify --quiet "${range_end}^{commit}" >/dev/null; then
        echo "ref does not resolve to a commit: $range_end (in range $ref)" >&2
        exit 2
      fi
    done
    ;;
  *)
    if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
      echo "ref does not resolve to a commit: $ref" >&2
      exit 2
    fi
    ;;
esac

# The history searched for same-person evidence is the inspected branch
# itself (the tip of a range, or the ref), plus any refs the operator fetched
# on purpose. In CI only the protected branch is checked out, so the default
# is exactly the branch's own history and nothing a PR could smuggle in.
case "$ref" in
  *..*) evidence_tip="${ref##*..}" ;;
  *)    evidence_tip="$ref" ;;
esac
evidence_refs="$evidence_tip"
for extra_ref in $same_person_extra_refs; do
  if ! git rev-parse --verify --quiet "${extra_ref}^{commit}" >/dev/null; then
    echo "DCO_SAME_PERSON_EVIDENCE_REFS entry does not resolve to a commit: $extra_ref" >&2
    exit 2
  fi
  evidence_refs="$evidence_refs $extra_ref"
done

email_allowed() {
  needle=$(printf '%s' "$1" | lower)
  [ -n "$needle" ] || return 1
  while IFS= read -r allowed; do
    allowed_lc=$(printf '%s' "$allowed" | lower)
    if [ -n "$allowed_lc" ] && [ "$needle" = "$allowed_lc" ]; then
      return 0
    fi
  done <<EOF_ALLOWLIST
$(printf '%s\n' "$allowlist_raw" | tr ',;[:space:]' '\n')
EOF_ALLOWLIST
  return 1
}

# waived_shas is the normalised waiver list, one lower-case full SHA per line.
# Built once so the per-commit lookup does not re-parse the raw value 50 times.
waived_shas=$(printf '%s\n' "$waived_raw" | tr ',;[:space:]' '\n' | lower | grep -v '^$' || true)

# A malformed waiver must be an ERROR, not a no-op. A typo'd or abbreviated SHA
# that silently matches nothing would leave the monitor red with the maintainer
# believing a disposition was recorded — the exact failure this instrument is
# meant to end. Abbreviations are refused outright: a prefix is ambiguous, and
# an ambiguous waiver on a security-adjacent check could come to cover a commit
# nobody dispositioned.
while IFS= read -r waived; do
  # An unset/blank DCO_WAIVED_COMMITS still yields one empty line here; an
  # empty entry is "no waivers", not a malformed one.
  [ -n "$waived" ] || continue
  if [ "${#waived}" -ne 40 ]; then
    echo "DCO_WAIVED_COMMITS entries must be full 40-character SHAs (got ${#waived} chars): $waived" >&2
    exit 2
  fi
  case "$waived" in
    *[!0-9a-f]*) echo "DCO_WAIVED_COMMITS entry is not a hex SHA: $waived" >&2; exit 2 ;;
  esac
done <<EOF_WAIVER_VALIDATE
$waived_shas
EOF_WAIVER_VALIDATE

commit_waived() {
  needle=$(printf '%s' "$1" | lower)
  while IFS= read -r waived; do
    if [ -n "$waived" ] && [ "$needle" = "$waived" ]; then
      return 0
    fi
  done <<EOF_WAIVED
$waived_shas
EOF_WAIVED
  return 1
}

is_bot_author() {
  ident_lc=$(printf '%s <%s>' "$1" "$2" | lower)
  case "$ident_lc" in
    *'[bot]'*|*'hive-release-bot'*|*'github-actions'*) return 0 ;;
    *) return 1 ;;
  esac
}

# Scan the WHOLE commit body for Signed-off-by lines, not just the final
# trailer block. `git interpret-trailers --parse` reads only the last
# paragraph, so a body like "Signed-off-by: ...\n\nCo-authored-by: ..." —
# which the pre-merge DCO app accepts, because it greps every line — was
# reported here as missing-signoff (#6605/8fe6bb34 waived for exactly this;
# v5 639f49e9 re-triggered it). Matching the pre-merge check's line-based
# rule keeps this monitor from paging on sign-offs already accepted at merge.
extract_signoff_emails() {
  git log -1 --format=%B "$1" |
    awk '
      tolower($0) ~ /^signed-off-by:[[:space:]]*/ {
        line=$0
        sub(/^[^:]+:[[:space:]]*/, "", line)
        if (match(line, /<[^<>[:space:]]+@[^<>[:space:]]+>/)) {
          email=substr(line, RSTART + 1, RLENGTH - 2)
          print tolower(email)
        }
      }'
}

# same_person_noreply_evidence <sha> <author-name> <author-email-lc> <signoff-email-lc>
#
# GitHub's squash merge writes the PR author's GitHub identity on the landing
# commit. For an account with a private email that is
# `<id>+<login>@users.noreply.github.com`, while the Signed-off-by carried over
# from the branch is the contributor's real address — the one the pre-merge DCO
# app already accepted on the branch commits (#15401, 687c3cb0). The two
# addresses belong to one person, but the trailer alone cannot show it.
#
# This looks for LOCAL evidence of the link, with no GitHub API call, in the
# last DCO_SAME_PERSON_WINDOW commits reachable from the inspected branch (and
# any DCO_SAME_PERSON_EVIDENCE_REFS). Accepts when another commit:
#
#   authored-under-signoff-email  was authored or committed under the sign-off
#                                 address by the same name (case-insensitive) as
#                                 this commit's author — the person has written
#                                 to this repository under that address before;
#   login-and-email-cooccur       has the sign-off address as author/committer
#                                 AND names <login>'s noreply address in its own
#                                 identities or Signed-off-by lines — the two
#                                 identities are tied together on a commit that
#                                 passed in the opposite orientation.
#
# A commit of the SAME shape as the one under test (noreply author, foreign
# sign-off) is deliberately not evidence: repetition of the ambiguity is not a
# resolution of it. Prints the evidence as `<kind>:<short-sha>` on success.
same_person_noreply_evidence() {
  sp_sha="$1"
  sp_author_name_lc=$(printf '%s' "$2" | lower)
  sp_author_lc="$3"
  sp_signoff_lc="$4"

  [ "$same_person_window" -gt 0 ] || return 1
  sp_login=$(github_noreply_login "$sp_author_lc") || return 1
  # A noreply sign-off is the login-lookup path's business, not this one's.
  case "$sp_signoff_lc" in
    *@users.noreply.github.com) return 1 ;;
  esac
  [ -n "$sp_author_name_lc" ] || return 1

  # shellcheck disable=SC2086 # evidence_refs is a deliberate word list
  while IFS=$'\x1f' read -r ev_sha ev_an ev_ae ev_cn ev_ce; do
    [ -n "$ev_sha" ] || continue
    [ "$ev_sha" != "$sp_sha" ] || continue
    ev_ae_lc=$(printf '%s' "$ev_ae" | lower)
    ev_ce_lc=$(printf '%s' "$ev_ce" | lower)
    ev_an_lc=$(printf '%s' "$ev_an" | lower)
    ev_cn_lc=$(printf '%s' "$ev_cn" | lower)

    if { [ "$ev_ae_lc" = "$sp_signoff_lc" ] && [ "$ev_an_lc" = "$sp_author_name_lc" ]; } ||
       { [ "$ev_ce_lc" = "$sp_signoff_lc" ] && [ "$ev_cn_lc" = "$sp_author_name_lc" ]; }; then
      printf 'authored-under-signoff-email:%s' "$(git rev-parse --short "$ev_sha")"
      return 0
    fi

    if [ "$ev_ae_lc" = "$sp_signoff_lc" ] || [ "$ev_ce_lc" = "$sp_signoff_lc" ]; then
      if github_noreply_matches_login "$ev_ae_lc" "$sp_login" ||
         github_noreply_matches_login "$ev_ce_lc" "$sp_login"; then
        printf 'login-and-email-cooccur:%s' "$(git rev-parse --short "$ev_sha")"
        return 0
      fi
      while IFS= read -r ev_signoff; do
        [ -n "$ev_signoff" ] || continue
        if github_noreply_matches_login "$ev_signoff" "$sp_login"; then
          printf 'login-and-email-cooccur:%s' "$(git rev-parse --short "$ev_sha")"
          return 0
        fi
      done <<EOF_EV_SIGNOFFS
$(extract_signoff_emails "$ev_sha")
EOF_EV_SIGNOFFS
    fi
  done < <(git log -n "$same_person_window" --format=$'%H\x1f%an\x1f%ae\x1f%cn\x1f%ce' $evidence_refs --)
  return 1
}

failures=""
same_person_report=""
same_person_count=0
waived_report=""
waived_count=0
stale_waivers=""
checked=0
skipped_merges=0
skipped_bots=0

# record_failure books one bad commit, routing it to the waived report instead
# of the failure list when a maintainer disposition covers that exact SHA.
record_failure() {
  sha_arg="$1"
  detail="$2"
  if commit_waived "$sha_arg"; then
    waived_count=$((waived_count + 1))
    waived_report="${waived_report}WAIVED ${sha_arg} ${detail}\n"
    return
  fi
  failures="${failures}FAIL ${sha_arg} ${detail}\n"
}

while IFS= read -r sha; do
  parent_count=$(git rev-list --parents -n 1 "$sha" | awk '{print NF-1}')
  if [ "$parent_count" -gt 1 ]; then
    skipped_merges=$((skipped_merges + 1))
    continue
  fi

  author_name=$(git log -1 --format=%an "$sha")
  author_email=$(git log -1 --format=%ae "$sha")
  if [ "$skip_bots" = "true" ] && is_bot_author "$author_name" "$author_email"; then
    skipped_bots=$((skipped_bots + 1))
    continue
  fi

  checked=$((checked + 1))
  author_lc=$(printf '%s' "$author_email" | lower)
  signoff_emails=$(extract_signoff_emails "$sha")
  author_login=""

  if [ -z "$signoff_emails" ]; then
    record_failure "$sha" "missing-signoff author=${author_name} <${author_email}>"
    continue
  fi

  matched=0
  while IFS= read -r signoff_email; do
    if [ "$signoff_email" = "$author_lc" ] || email_allowed "$signoff_email"; then
      matched=1
      break
    fi
    if github_noreply_matches_login "$signoff_email" "${author_login:-}"; then
      matched=1
      break
    fi
    case "$signoff_email" in
      *@users.noreply.github.com)
        if [ -z "${author_login:-}" ]; then
          author_login=$(author_login_for_commit "$sha")
        fi
        if github_noreply_matches_login "$signoff_email" "$author_login"; then
          matched=1
          break
        fi
        ;;
    esac
  done <<EOF_SIGNOFFS
$signoff_emails
EOF_SIGNOFFS

  if [ "$matched" -ne 1 ]; then
    while IFS= read -r signoff_email; do
      [ -n "$signoff_email" ] || continue
      if evidence=$(same_person_noreply_evidence "$sha" "$author_name" "$author_lc" "$signoff_email"); then
        matched=1
        same_person_count=$((same_person_count + 1))
        same_person_report="${same_person_report}same-person-noreply ${sha} author=${author_name} <${author_email}> signed-off-by=${signoff_email} evidence=${evidence}\n"
        break
      fi
    done <<EOF_SAME_PERSON
$signoff_emails
EOF_SAME_PERSON
  fi

  if [ "$matched" -ne 1 ]; then
    rendered=$(printf '%s' "$signoff_emails" | paste -sd, -)
    record_failure "$sha" "mismatched-signoff author=${author_name} <${author_email}> signed-off-by=${rendered}"
  else
    # A waiver on a commit that now PASSES is dead weight: it no longer
    # suppresses anything, but it stays in the config as an unexamined
    # standing exception. Only reportable when the commit is inside the
    # inspected window — a waiver for older history is simply out of range,
    # not stale.
    if commit_waived "$sha"; then
      stale_waivers="${stale_waivers:-}${sha}
"
    fi
  fi
done < <(git rev-list -n "$limit" "$ref")

# "recent commits on <tip>" would misdescribe a range scan, where the commits
# are the push delta rather than the newest N reachable from a tip.
case "$ref" in
  *..*) scanned_scope='commits in' ;;
  *)    scanned_scope='recent commits on' ;;
esac
printf 'DCO trailer check inspected %s %s %s (%s non-merge, non-bot checked; %s merge skipped; %s bot skipped; %s waived; %s same-person-noreply).\n' \
  "$limit" "$scanned_scope" "$ref" "$checked" "$skipped_merges" "$skipped_bots" "$waived_count" "$same_person_count"

# Waived commits print on every run, above the failures. A disposition that
# produced no output would be an invisible hole in a compliance check; keeping
# it in the report means each run restates exactly which history is being
# accepted and why it had to be.
if [ -n "$waived_report" ]; then
  printf '%b' "$waived_report"
fi

# Same-person acceptances are informational, never hidden: each one names the
# commit in the branch history that supplied the evidence, so a reader can
# audit the inference rather than take it on trust.
if [ -n "$same_person_report" ]; then
  printf '%b' "$same_person_report"
fi

# Stale waivers are advisory, not fatal: failing the monitor over config
# housekeeping would page the same people this change is trying to stop paging.
if [ -n "${stale_waivers:-}" ]; then
  while IFS= read -r stale; do
    [ -n "$stale" ] && printf 'STALE-WAIVER %s commit now passes; remove it from DCO_WAIVED_COMMITS\n' "$stale"
  done <<EOF_STALE
${stale_waivers}
EOF_STALE
fi

if [ -n "$failures" ]; then
  printf '%b' "$failures"
  exit 1
fi

if [ -n "$waived_report" ]; then
  echo "DCO trailer check passed (with recorded waivers)."
else
  echo "DCO trailer check passed."
fi
