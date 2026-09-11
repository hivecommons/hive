#!/usr/bin/env bash
# issue-coauthor.sh — print the `Co-authored-by:` trailer that credits the
# author of a GitHub issue on the commit that resolves it (hivecommons/hive#6588).
#
# Hive prefers that its own maintainers and agents write the code, and steers
# outside contributors toward filing issues. That only works as a contribution
# path if filing an accepted issue actually earns credit: a `Co-authored-by:`
# trailer puts the filer in the repository's contributor list and on their own
# GitHub contribution graph, which "thanks, closing" does not.
#
# Getting the trailer right by hand is the part people get wrong, and a wrong
# trailer is worse than none — GitHub silently ignores an address it cannot
# resolve, so the credit looks recorded while nothing was credited. This script
# resolves the identity through the API and emits exactly one well-formed line,
# or nothing at all, or an error. It never guesses.
#
#   issue-coauthor.sh 6588                 # print the trailer
#   issue-coauthor.sh --amend 6588         # add it to HEAD's message
#   issue-coauthor.sh --append MSG 6588    # add it to a commit-message file
#
# Exit codes:
#   0  a trailer was produced (stdout), OR none was needed (empty stdout, with
#      the reason on stderr — a bot filed the issue, or the filer is you)
#   1  the issue or its author could not be resolved; nothing was emitted
#   2  usage error
#
# "None needed" is exit 0 with empty stdout on purpose, so the common shape
#
#     trailer=$(issue-coauthor.sh "$n") || exit 1
#     [ -n "$trailer" ] && ...
#
# works without treating "nobody to credit" as a failure.
set -u -o pipefail

# GH is the GitHub CLI. Overridable so the self-test can stub the API without a
# network or a token; nothing outside tests sets it.
GH="${HIVE_GH_BIN:-gh}"
REPO="${HIVE_COAUTHOR_REPO:-hivecommons/hive}"

usage() {
  cat >&2 <<'USAGE'
Usage: issue-coauthor.sh [--amend | --append FILE] <issue-number>

Prints the Co-authored-by: trailer crediting the author of <issue-number>.

Options:
  --amend         append the trailer to HEAD's commit message in place
  --append FILE   append the trailer to a commit-message FILE

Environment:
  HIVE_COAUTHOR_REPO  repository to resolve the issue in (default hivecommons/hive)
  HIVE_GH_BIN         gh binary to use (test seam)
USAGE
}

mode="print"
target=""
issue=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --amend)
      mode="amend"
      shift
      ;;
    --append)
      mode="append"
      shift
      if [ "$#" -eq 0 ]; then
        echo "--append requires a file" >&2
        usage
        exit 2
      fi
      target="$1"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "unknown option: $1" >&2
      usage
      exit 2
      ;;
    *)
      if [ -n "$issue" ]; then
        echo "unexpected extra argument: $1" >&2
        usage
        exit 2
      fi
      issue="$1"
      shift
      ;;
  esac
done

case "$issue" in
  ''|*[!0-9]*)
    echo "issue number must be a positive integer: '${issue}'" >&2
    usage
    exit 2
    ;;
esac
if [ "$issue" -eq 0 ]; then
  echo "issue number must be greater than zero" >&2
  exit 2
fi
if [ "$mode" = "append" ] && [ ! -f "$target" ]; then
  echo "commit-message file does not exist: $target" >&2
  exit 2
fi

# One API call for the identity. login/id/type/name all come from the same
# response so they cannot describe different accounts.
if ! raw=$("$GH" api "repos/${REPO}/issues/${issue}" \
  --jq '[.user.login, (.user.id|tostring), .user.type, (.user.name // "")] | @tsv' 2>&1); then
  echo "cannot resolve issue ${REPO}#${issue}: ${raw}" >&2
  exit 1
fi

IFS="$(printf '\t')" read -r login id utype name <<EOF_IDENT
${raw}
EOF_IDENT

if [ -z "${login:-}" ] || [ -z "${id:-}" ]; then
  # A partial answer is not something to build a credit line out of.
  echo "issue ${REPO}#${issue} returned no usable author identity" >&2
  exit 1
fi
case "$id" in
  ''|*[!0-9]*)
    echo "issue ${REPO}#${issue} returned a non-numeric author id: '${id}'" >&2
    exit 1
    ;;
esac

# A bot does not need to be thanked, and crediting one would put automation in
# the human contributor list. `type` is the authoritative signal; the "[bot]"
# suffix is the belt-and-braces one for accounts the API types as User.
case "${utype:-}" in
  Bot) echo "issue ${REPO}#${issue} was filed by a bot (${login}); no co-author trailer" >&2; exit 0 ;;
esac
case "$login" in
  *'[bot]'|github-actions*|*-bot) echo "issue ${REPO}#${issue} was filed by a bot (${login}); no co-author trailer" >&2; exit 0 ;;
esac

# Do not co-author yourself. Comparing LOGINS, not emails: the whole reason the
# noreply form exists below is that one person's commit email and their GitHub
# identity routinely differ, so an email comparison would miss the match and
# credit you as your own co-author.
self=""
if self_raw=$("$GH" api user --jq '.login' 2>/dev/null); then
  self="$self_raw"
fi
if [ -n "$self" ] && [ "$(printf '%s' "$self" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$login" | tr '[:upper:]' '[:lower:]')" ]; then
  echo "issue ${REPO}#${issue} was filed by you (${login}); no co-author trailer" >&2
  exit 0
fi

# The display name is free text an account holder controls, so it is stripped
# of everything that could be mistaken for address syntax before being pasted
# into a structured trailer. The rule is deliberately blunt — drop "<", ">",
# "@" and control characters — because each one is a way for a name to say
# something the trailer does not mean:
#
#   "<" / ">"     forge the address field itself
#   newline / CR  forge an entire additional trailer
#   "@"           leaves a name that still READS as an email address in
#                 `git log` (a name of "Eve <evil@x>" sanitised only for
#                 angle brackets renders as "Eve evil@x", which attributes
#                 correctly but tells a reader something false)
#
# Everything else — letters, spaces, punctuation, non-ASCII — passes through,
# so ordinary names are unaffected. Falls back to the login rather than
# emitting an empty name.
clean_name=$(printf '%s' "${name:-}" | tr -d '<>@\n\r\t' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//; s/[[:space:]][[:space:]]*/ /g')
if [ -z "$clean_name" ]; then
  clean_name="$login"
fi

# The id-prefixed noreply address, always — never a profile email.
#
# It is the one form guaranteed to resolve to the account: GitHub links a
# co-author by matching the address to an account, and the numeric id makes
# that match independent of what the person has configured. A profile email can
# be absent, can be a personal address the filer did not offer for republishing
# in this repository's git history, and can stop resolving if they remove it.
# Choosing this once, here, is the point of having a script.
email="${id}+${login}@users.noreply.github.com"
trailer="Co-authored-by: ${clean_name} <${email}>"

# add_trailer folds the trailer into a commit-message file. git interpret-trailers
# owns the placement so the line lands in the message's trailer block rather
# than after the prose — which also keeps an existing `Signed-off-by:` inside
# that same block, where DCO tooling can still see it.
#
# --if-exists must be addIfDifferent, NOT doNothing. doNothing keys on the
# trailer NAME, not the whole line, so it suppresses the new trailer whenever
# the message already carries ANY `Co-authored-by:`. Every commit in this
# repository carries `Co-authored-by: Copilot ...` by convention, so doNothing
# made this script a silent no-op on essentially every real commit — the exact
# "the credit looks recorded while nothing was credited" failure this file's
# header warns about. addIfDifferent still makes a re-run idempotent, because
# it compares the entire trailer line.
add_trailer() {
  file="$1"
  tmp="${file}.coauthor.$$"
  if ! git interpret-trailers --if-exists addIfDifferent --trailer "$trailer" "$file" > "$tmp"; then
    rm -f "$tmp"
    echo "failed to add the trailer to ${file}" >&2
    exit 1
  fi
  mv "$tmp" "$file"
}

case "$mode" in
  print)
    printf '%s\n' "$trailer"
    ;;
  append)
    add_trailer "$target"
    ;;
  amend)
    if ! git rev-parse --verify --quiet HEAD >/dev/null; then
      echo "no HEAD commit to amend" >&2
      exit 1
    fi
    msg=$(mktemp)
    # shellcheck disable=SC2064 # expand the path now: $msg is fixed from here
    trap "rm -f '$msg'" EXIT
    git log -1 --format=%B HEAD > "$msg"
    add_trailer "$msg"
    # --no-edit is not passed with -F: -F already supplies the message, and the
    # author/date of HEAD are preserved by --amend.
    if ! git commit --amend --quiet -F "$msg"; then
      echo "git commit --amend failed" >&2
      exit 1
    fi
    printf '%s\n' "$trailer"
    ;;
esac
