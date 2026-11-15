#!/usr/bin/env bash
# v6-topup-autoresolve.sh — finish a conflicted v5 -> v6 forward-merge when
# every conflict is one of the known, mechanically resolvable patterns
# (.github/workflows/v6-topup.yml). Run it from the repo root with the merge
# still in progress. Exit 0: every conflict was resolved and staged (the caller
# commits). Exit 1: at least one conflict is not a known pattern, or a known
# file conflicts in a way this script does not recognize; nothing is staged
# that the caller needs to keep (it aborts the merge and reports).
#
# Known patterns:
#   - DU discord/** and DU/UD .github/workflows/discord-syntax.yml: v6 removed
#     discord/ (#9277) while v5 still develops it. Keep v6's deletion.
#   - UU CHANGELOG.md: both lines prepend their own release sections under
#     "## Unreleased" on every release, so the top of the file conflicts on
#     almost every top-up. Keep both sides' lines (v6's block first, then
#     v5's), which is the layout v6's CHANGELOG already has.
#   - UU .github/scripts/integration-governance.cjs, when every conflict hunk
#     is the TRUSTED_SCRIPTS_TREE literal on both sides: the literal pins
#     `git rev-parse HEAD:src/scripts`, so whenever both lines touched
#     src/scripts the two pins differ. Re-pin it to the merged src/scripts
#     tree (the merge of two already-reviewed trees; the top-up PR shows it).
#   - UU src/pkg/dashboard/static/index.html: a generated artifact. Regenerate
#     it as the byte-exact concatenation of static-src/manifest.txt (what
#     webstatic.ConcatIndex and TestIndexHTMLIsGenerated do), provided no
#     static-src file is itself conflicted.
#
# Deliberately NOT a known pattern:
#   - UU src/pkg/config/config.go. v6 split the monolith into topical files
#     (#9386, #9956), and an earlier version of this script kept v6's side of
#     any config.go conflict. A conflict there means v5 changed config.go since
#     the last top-up, and keeping v6's side drops that change without a trace:
#     #18222's keystore sealing survived only because it was hand-ported into
#     v6's load.go/save.go. A config.go conflict always goes to a human, who
#     ports v5's change into the matching split file. Whether v6 already has an
#     equivalent change cannot be decided mechanically (the code moved), so
#     there is no "safe subset" to auto-resolve.
set -euo pipefail

PIN_FILE=.github/scripts/integration-governance.cjs
INDEX_FILE=src/pkg/dashboard/static/index.html
STATIC_SRC=src/pkg/dashboard/static-src
PIN_RE="^const TRUSTED_SCRIPTS_TREE = '[0-9a-f]{40}';\$"

unmerged="$(git status --porcelain | grep -E '^(DD|AU|UD|UA|DU|AA|UU) ' || true)"
[ -n "$unmerged" ] || { echo "v6-topup-autoresolve: no conflicts to resolve"; exit 1; }

known='^DU discord/|^(DU|UD) \.github/workflows/discord-syntax\.yml$|^UU CHANGELOG\.md$|^UU \.github/scripts/integration-governance\.cjs$|^UU src/pkg/dashboard/static/index\.html$'
if unknown="$(printf '%s\n' "$unmerged" | grep -vE "$known")"; then
  echo "v6-topup-autoresolve: conflicts outside the known patterns:"
  printf '%s\n' "$unknown"
  if printf '%s\n' "$unknown" | grep -q ' src/pkg/config/config\.go$'; then
    echo "v6-topup-autoresolve: v5 changed src/pkg/config/config.go, which v6 split into topical files;"
    echo "  port v5's change by hand: git diff \$(git merge-base HEAD MERGE_HEAD) MERGE_HEAD -- src/pkg/config/config.go"
  fi
  exit 1
fi

# Every hunk of the pin file must be exactly one TRUSTED_SCRIPTS_TREE line per
# side; anything else is a real edit and goes to a human.
pin_hunks_only() {
  awk -v re="$PIN_RE" '
    /^<<<<<<< / { side = 1; n1 = 0; n2 = 0; hunks++; next }
    /^\|\|\|\|\|\|\| / && side == 1 { side = 3; next }
    /^=======$/ && (side == 1 || side == 3) { side = 2; next }
    /^>>>>>>> / && side == 2 {
      if (n1 != 1 || n2 != 1) bad = 1
      side = 0; next
    }
    side == 1 { n1++; if ($0 !~ re) bad = 1 }
    side == 2 { n2++; if ($0 !~ re) bad = 1 }
    END { exit (bad || hunks == 0 || side != 0) ? 1 : 0 }
  ' "$1"
}

regen_index=0
repin=0
resolved=0
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  status="${entry%% *}"
  file="${entry#* }"
  case "$status $file" in
    "DU "*|"UD "*)
      git rm -q -- "$file"
      ;;
    "UU CHANGELOG.md")
      tmp="$(mktemp -d)"
      git show ":1:$file" > "$tmp/base"
      git show ":2:$file" > "$tmp/ours"
      git show ":3:$file" > "$tmp/theirs"
      git merge-file --union "$tmp/ours" "$tmp/base" "$tmp/theirs"
      cp "$tmp/ours" "$file"
      rm -rf "$tmp"
      git add -- "$file"
      ;;
    "UU $PIN_FILE")
      if ! pin_hunks_only "$file"; then
        echo "v6-topup-autoresolve: $file conflicts outside the TRUSTED_SCRIPTS_TREE literal"
        exit 1
      fi
      repin=1
      ;;
    "UU $INDEX_FILE")
      regen_index=1
      ;;
    *)
      echo "v6-topup-autoresolve: unhandled entry: $entry"
      exit 1
      ;;
  esac
  resolved=$((resolved + 1))
done <<<"$unmerged"

if [ "$regen_index" = 1 ]; then
  : > "$INDEX_FILE"
  while IFS= read -r line || [ -n "$line" ]; do
    line="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
    case "$line" in ''|'#'*) continue ;; esac
    # The same clean-relative-path rule as webstatic.ParseIndexManifest.
    case "$line" in
      /*|..*|./*|*/|*//*|*/./*|*/../*|*/.|*/..)
        echo "v6-topup-autoresolve: unsafe manifest path $line"; exit 1 ;;
    esac
    cat "$STATIC_SRC/$line" >> "$INDEX_FILE"
  done < "$STATIC_SRC/manifest.txt"
  git add -- "$INDEX_FILE"
fi

if [ "$repin" = 1 ]; then
  # Stage a placeholder so the index has no unmerged entries, then read the
  # merged src/scripts tree from it (the pin file is outside src/scripts).
  # Keep v6's side of each (pin-only) hunk; the literal is rewritten below.
  awk '
    /^<<<<<<< / { side = 1; next }
    /^\|\|\|\|\|\|\| / && side == 1 { side = 3; next }
    /^=======$/ && (side == 1 || side == 3) { side = 2; next }
    /^>>>>>>> / && side == 2 { side = 0; next }
    side == 0 || side == 1 { print }
  ' "$PIN_FILE" > "$PIN_FILE.tmp"
  mv "$PIN_FILE.tmp" "$PIN_FILE"
  git add -- "$PIN_FILE"
  tree="$(git write-tree)"
  scripts_tree="$(git rev-parse "${tree}:src/scripts")"
  sed -E -i.bak "s/^const TRUSTED_SCRIPTS_TREE = '[0-9a-f]{40}';\$/const TRUSTED_SCRIPTS_TREE = '${scripts_tree}';/" "$PIN_FILE"
  rm -f "$PIN_FILE.bak"
  git add -- "$PIN_FILE"
  echo "v6-topup-autoresolve: re-pinned TRUSTED_SCRIPTS_TREE to ${scripts_tree}"
fi

if git status --porcelain | grep -qE '^(DD|AU|UD|UA|DU|AA|UU) '; then
  echo "v6-topup-autoresolve: conflicts remain after resolution"
  exit 1
fi
echo "v6-topup-autoresolve: resolved ${resolved} known conflict(s)"
