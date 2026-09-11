#!/usr/bin/env bash
# test-issue-coauthor.sh — exercises issue-coauthor.sh against a stubbed GitHub
# API and a throwaway git repository (hivecommons/hive#6588).
#
# The stub is the point: the script's job is to turn one API answer into one
# exactly-shaped trailer, and every interesting case (a bot filed it, you filed
# it, the display name contains a "<", the id is missing) is an API answer that
# cannot be produced on demand against the real service.
#
# The last section is the one that must never regress: co-authoring a commit is
# ATTRIBUTION, not certification. It must leave the DCO checker's verdict
# exactly as it found it, and it must never let a commit pass DCO on the
# strength of someone else's name.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/issue-coauthor.sh"
DCO_CHECKER="${HERE}/check-dco-trailers.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/issue-coauthor.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

# --- the gh stub -------------------------------------------------------------
#
# Answers `api repos/<repo>/issues/<n> --jq ...` from a fixture file per issue
# number, and `api user --jq .login` from STUB_SELF. Anything else is an error,
# so a script change that starts calling an unexpected endpoint is caught here
# rather than silently hitting the network in CI.
STUB_DIR="$TMP/stub"
mkdir -p "$STUB_DIR"
cat > "$STUB_DIR/gh" <<'STUB'
#!/usr/bin/env bash
set -u
if [ "${1:-}" = "api" ] && [ "${2:-}" = "user" ]; then
  if [ -z "${STUB_SELF:-}" ]; then
    echo "stub: no authenticated user" >&2
    exit 1
  fi
  printf '%s\n' "$STUB_SELF"
  exit 0
fi
if [ "${1:-}" = "api" ]; then
  case "${2:-}" in
    */issues/*)
      n="${2##*/}"
      f="${STUB_FIXTURES}/${n}.tsv"
      if [ ! -f "$f" ]; then
        echo "gh: Not Found (HTTP 404)" >&2
        exit 1
      fi
      cat "$f"
      exit 0
      ;;
  esac
fi
echo "stub: unexpected gh invocation: $*" >&2
exit 99
STUB
chmod +x "$STUB_DIR/gh"

FIXTURES="$TMP/fixtures"
mkdir -p "$FIXTURES"
# login <TAB> id <TAB> type <TAB> name
printf 'filer\t12345\tUser\tFiona Filer\n'            > "$FIXTURES/100.tsv"
printf 'noname\t222\tUser\t\n'                        > "$FIXTURES/101.tsv"
printf 'dependabot[bot]\t333\tBot\tDependabot\n'      > "$FIXTURES/102.tsv"
printf 'sneaky\t444\tUser\tEve <evil@x> \n'           > "$FIXTURES/103.tsv"
printf 'selffiler\t555\tUser\tSelf Filer\n'           > "$FIXTURES/104.tsv"
printf 'weird\tnotanumber\tUser\tWeird\n'             > "$FIXTURES/105.tsv"
printf '\t\tUser\t\n'                                 > "$FIXTURES/106.tsv"
# Typed User, but the login carries the bot suffix anyway.
printf 'renovate[bot]\t777\tUser\tRenovate\n'         > "$FIXTURES/107.tsv"

run() { # run <issue-or-args...>; sets out/err/rc
  set +e
  out=$(PATH="$STUB_DIR:$PATH" HIVE_GH_BIN="$STUB_DIR/gh" \
        STUB_FIXTURES="$FIXTURES" STUB_SELF="${STUB_SELF:-}" \
        bash "$SCRIPT" "$@" 2>"$TMP/err")
  rc=$?
  err=$(cat "$TMP/err")
  set -e
}

# --- the trailer itself ------------------------------------------------------

STUB_SELF="maintainer"
run 100
if [ "$rc" -eq 0 ] && [ "$out" = "Co-authored-by: Fiona Filer <12345+filer@users.noreply.github.com>" ]; then
  pass "emits the id-prefixed noreply trailer"
else
  bad "trailer wrong (rc=${rc}): ${out}"
fi

# The id-prefixed noreply form is the whole reason this is a script rather than
# a doc line: it is the only address guaranteed to resolve to the account.
case "$out" in
  *"12345+filer@users.noreply.github.com"*) pass "uses the numeric-id noreply address" ;;
  *) bad "address is not the id-prefixed noreply form: ${out}" ;;
esac

run 101
if [ "$out" = "Co-authored-by: noname <222+noname@users.noreply.github.com>" ]; then
  pass "falls back to the login when the profile has no display name"
else
  bad "empty-name fallback wrong: ${out}"
fi

# A display name is free text the account holder controls. "<" or ">" in it
# would forge the address, a newline would forge a whole extra trailer, and a
# surviving "@" would leave a name that still READS as an address even though
# attribution went to the noreply one.
run 103
if [ "$rc" -ne 0 ]; then
  bad "hostile display name errored instead of being sanitised (rc=${rc}): ${err}"
elif [ "$(printf '%s\n' "$out" | wc -l)" -ne 1 ]; then
  bad "hostile display name produced more than one line: ${out}"
elif [ "$out" != "Co-authored-by: Eve evilx <444+sneaky@users.noreply.github.com>" ]; then
  bad "sanitised trailer not as expected: ${out}"
else
  pass "address syntax in a display name is stripped, not honoured"
fi
# Whatever the name said, the address credited is still the resolved account's.
case "$out" in
  *"<444+sneaky@users.noreply.github.com>") pass "attribution still goes to the resolved account" ;;
  *) bad "hostile name changed who gets credited: ${out}" ;;
esac
# Exactly one "@" survives anywhere in the line: the one in the real address.
if [ "$(printf '%s' "$out" | tr -cd '@' | wc -c)" -eq 1 ]; then
  pass "only the real address contributes an '@' to the trailer"
else
  bad "more than one '@' in the trailer: ${out}"
fi

# git must agree it is a trailer, not just a line that looks like one.
run 100
if printf 'subject\n\nbody\n\n%s\n' "$out" | git interpret-trailers --parse | grep -q '^Co-authored-by: Fiona Filer'; then
  pass "git interpret-trailers parses the output as a trailer"
else
  bad "git does not parse the emitted line as a trailer: ${out}"
fi

# --- the cases where NO trailer is right -------------------------------------

run 102
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
  pass "a bot-filed issue produces no trailer, and is not an error"
else
  bad "bot case wrong (rc=${rc}, out=${out})"
fi
case "$err" in *bot*) pass "the bot case says why on stderr" ;; *) bad "bot case gave no reason: ${err}" ;; esac

run 107
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
  pass "a '[bot]' login is skipped even when the API types it as User"
else
  bad "bot-suffix case wrong (rc=${rc}, out=${out})"
fi

STUB_SELF="selffiler"
run 104
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
  pass "your own issue produces no trailer (no self-co-authoring)"
else
  bad "self case wrong (rc=${rc}, out=${out})"
fi
# Login comparison, not email: the same person's commit email and GitHub
# identity routinely differ, which is why the noreply form exists at all.
STUB_SELF="SELFFILER"
run 104
if [ "$rc" -eq 0 ] && [ -z "$out" ]; then
  pass "the self check is case-insensitive on the login"
else
  bad "case-differing self login was not recognised (out=${out})"
fi
# Unauthenticated gh must not turn into a crash or a wrong skip.
STUB_SELF=""
run 100
if [ "$rc" -eq 0 ] && [ -n "$out" ]; then
  pass "an unauthenticated gh still emits the trailer rather than failing"
else
  bad "unauthenticated case wrong (rc=${rc}, out=${out}, err=${err})"
fi

# --- failures must be loud and empty -----------------------------------------

STUB_SELF="maintainer"
for spec in "404:999" "non-numeric id:105" "empty identity:106"; do
  label="${spec%%:*}"
  n="${spec##*:}"
  run "$n"
  if [ "$rc" -eq 1 ] && [ -z "$out" ]; then
    pass "${label} exits 1 with nothing on stdout"
  else
    bad "${label} wrong (rc=${rc}, out=${out})"
  fi
done

for badarg in "" "abc" "0" "-7" "--nope"; do
  run "$badarg"
  if [ "$rc" -eq 2 ] && [ -z "$out" ]; then
    pass "usage error for '${badarg}' exits 2 with nothing on stdout"
  else
    bad "bad argument '${badarg}' wrong (rc=${rc}, out=${out})"
  fi
done

# --- --append and --amend ----------------------------------------------------

msg="$TMP/msg.txt"
printf 'fix: something\n\nA body paragraph.\n\nSigned-off-by: Dev <dev@example.com>\n' > "$msg"
run --append "$msg" 100
if [ "$rc" -ne 0 ]; then
  bad "--append failed (rc=${rc}): ${err}"
elif ! grep -q '^Co-authored-by: Fiona Filer' "$msg"; then
  bad "--append did not add the trailer"
elif ! grep -q '^Signed-off-by: Dev' "$msg"; then
  bad "--append dropped the existing sign-off"
else
  pass "--append adds the trailer and keeps the sign-off"
fi
# The line has to land in the trailer block, not after the prose, or git and
# GitHub both stop treating it as a trailer.
if git interpret-trailers --parse "$msg" | grep -q '^Co-authored-by: Fiona Filer'; then
  pass "--append puts the line in the trailer block"
else
  bad "--append placed the line outside the trailer block"
fi
before=$(cat "$msg")
run --append "$msg" 100
if [ "$(cat "$msg")" = "$before" ]; then
  pass "--append is idempotent"
else
  bad "--append duplicated the trailer on a second run"
fi
run --append "$TMP/does-not-exist" 100
if [ "$rc" -eq 2 ]; then
  pass "--append on a missing file is a usage error"
else
  bad "--append on a missing file wrong (rc=${rc})"
fi

# --- regression: a pre-existing Co-authored-by must not suppress the credit ---
#
# This is the case every real commit in this repository is in: the contributor
# convention already puts `Co-authored-by: Copilot ...` on the commit. An
# --if-exists policy that keys on the trailer NAME (doNothing) silently skips
# the issue author here, so the mechanism reports success while crediting
# nobody — and the issue list looks correct while #6588's whole purpose is
# defeated. Guard the behaviour, not the flag.
msg2="$TMP/msg-existing-coauthor.txt"
printf 'feat: thing\n\nA body paragraph.\n\nCo-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>\nSigned-off-by: Dev <dev@example.com>\n' > "$msg2"
run --append "$msg2" 100
if [ "$rc" -ne 0 ]; then
  bad "--append failed alongside an existing co-author (rc=${rc}): ${err}"
elif ! grep -q '^Co-authored-by: Fiona Filer' "$msg2"; then
  bad "an existing Co-authored-by suppressed the issue author's credit"
elif ! grep -q '^Co-authored-by: Copilot' "$msg2"; then
  bad "--append dropped the pre-existing co-author"
else
  pass "an existing Co-authored-by does not suppress the issue author"
fi
# Both co-authors AND the sign-off have to remain in one trailer block. A blank
# line between them splits the block, git interpret-trailers stops reporting the
# sign-off, and the post-merge DCO monitor fails the protected branch (#6605).
parsed2=$(git interpret-trailers --parse "$msg2")
if printf '%s\n' "$parsed2" | grep -q '^Signed-off-by: Dev' \
  && printf '%s\n' "$parsed2" | grep -q '^Co-authored-by: Fiona Filer' \
  && printf '%s\n' "$parsed2" | grep -q '^Co-authored-by: Copilot'; then
  pass "sign-off and both co-authors stay in a single trailer block"
else
  bad "the trailer block was split; DCO would fail. parsed: ${parsed2}"
fi
before2=$(cat "$msg2")
run --append "$msg2" 100
if [ "$(cat "$msg2")" = "$before2" ]; then
  pass "--append stays idempotent alongside an existing co-author"
else
  bad "--append duplicated the trailer on a second run"
fi

repo="$TMP/repo"
mkdir -p "$repo"
(
  cd "$repo" || exit 1
  git init -q
  git config user.name 'Dev Person'
  git config user.email 'dev@example.com'
  echo x > f
  git add f
  git commit -q -s -m 'fix: a thing'
) || bad "could not build the amend fixture"

(
  cd "$repo" || exit 1
  set +e
  PATH="$STUB_DIR:$PATH" HIVE_GH_BIN="$STUB_DIR/gh" STUB_FIXTURES="$FIXTURES" \
    STUB_SELF="maintainer" bash "$SCRIPT" --amend 100 >/dev/null 2>&1
  rc=$?
  set -e
  exit $rc
)
if [ "$?" -ne 0 ]; then
  bad "--amend failed"
else
  body=$(cd "$repo" && git log -1 --format=%B)
  author=$(cd "$repo" && git log -1 --format='%an <%ae>')
  if ! printf '%s' "$body" | grep -q '^Co-authored-by: Fiona Filer'; then
    bad "--amend did not add the trailer"
  elif ! printf '%s' "$body" | grep -q '^Signed-off-by: Dev Person'; then
    bad "--amend dropped the sign-off"
  elif [ "$author" != "Dev Person <dev@example.com>" ]; then
    bad "--amend changed the commit author to ${author}"
  else
    pass "--amend adds the trailer, keeps the sign-off, and keeps the author"
  fi
fi

# --- the DCO invariant -------------------------------------------------------
#
# Co-authoring is attribution; signing off is certification. Conflating them
# would be a licensing problem, not a formatting one, so both directions are
# pinned here rather than assumed.

dco="$TMP/dco"
mkdir -p "$dco"
(
  cd "$dco" || exit 1
  git init -q
  git config user.name 'Dev Person'
  git config user.email 'dev@example.com'
  co='Co-authored-by: Fiona Filer <12345+filer@users.noreply.github.com>'
  so='Signed-off-by: Dev Person <dev@example.com>'

  echo a > f && git add f
  git commit -q -m "$(printf 'one\n\n%s\n%s\n' "$co" "$so")"
  echo b >> f && git add f
  git commit -q -m "$(printf 'two\n\n%s\n%s\n' "$so" "$co")"
) || bad "could not build the DCO fixture"

set +e
(cd "$dco" && bash "$DCO_CHECKER" 2 HEAD >/dev/null 2>&1)
dco_rc=$?
set -e
if [ "$dco_rc" -eq 0 ]; then
  pass "a co-authored commit still passes DCO, in either trailer order"
else
  bad "co-authoring broke the DCO checker (rc=${dco_rc})"
fi

# The other direction, and the one that would matter: a co-author's name must
# never stand in for the author's own sign-off.
(
  cd "$dco" || exit 1
  echo c >> f && git add f
  git commit -q -m "$(printf 'three\n\nCo-authored-by: Fiona Filer <12345+filer@users.noreply.github.com>\n')"
) || bad "could not build the unsigned fixture"
set +e
(cd "$dco" && bash "$DCO_CHECKER" 1 HEAD >/dev/null 2>&1)
unsigned_rc=$?
set -e
if [ "$unsigned_rc" -eq 1 ]; then
  pass "a commit with ONLY a co-author trailer still fails DCO"
else
  bad "a co-author trailer was accepted as a sign-off (rc=${unsigned_rc})"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-issue-coauthor FAILED"
  exit 1
fi

echo "test-issue-coauthor OK"
