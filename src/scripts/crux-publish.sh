#!/usr/bin/env bash
# crux-publish.sh — build the hivecommons/crux publish tree from src/crux.
#
# Hive is the source of truth for the Crux agent runtime: the nested Go module
# at src/crux (module github.com/hivecommons/hive/crux, #13536). The public
# repository hivecommons/crux is a *published copy* of that module with the
# module path rewritten to github.com/hivecommons/crux, so that
# `go install github.com/hivecommons/crux/cmd/crux@latest` and
# `brew install hivecommons/hive/crux` work without pulling hive. Nothing is
# edited in hivecommons/crux by hand except its own build scaffolding
# (.github/, .goreleaser.yaml, Dockerfile, README.md, LICENSE, runbooks/).
#
# This script is the whole mechanism (.github/workflows/crux-sync.yml calls
# it; it also runs by hand for a first/emergency sync):
#
#   1. Start from a checkout of hivecommons/crux `main` (DEST).
#   2. Remove everything in DEST except the hand-maintained scaffolding
#      listed in KEEP below.
#   3. Copy src/crux into DEST. Its README.md (module-level, hive-facing)
#      lands as docs/MODULE.md; DEST/README.md stays hand-maintained.
#   4. Rewrite the module path in go.mod and every *.go import.
#   5. Record the hive source sha + date in .hive-sync.
#   6. With GOWORK=off, `go build ./...` and `go vet ./...` the result. The
#      nested module requires nothing from the root module and the root
#      go.work is not copied, so the publish tree must build standalone; if
#      it does not, the sync stops here and nothing is committed.
#
# A plain copy (not `git subtree split`) is deliberate: the split walks hive's
# full history on every run (minutes, and growing), produces a history whose
# imports do not compile (the rewrite happens after the split), and gains us
# nothing because the commit that lands in hivecommons/crux names the exact
# hive sha it came from. Each sync is one commit on top of hivecommons/crux
# `main`, so that repository's history is linear and fast-forward only.
#
# Usage: src/scripts/crux-publish.sh <hive-checkout> <crux-checkout>
# Env:   CRUX_SKIP_BUILD=1 skips step 6 (for dry runs without a toolchain).
set -euo pipefail

HIVE="${1:?usage: crux-publish.sh <hive-checkout> <crux-checkout>}"
DEST="${2:?usage: crux-publish.sh <hive-checkout> <crux-checkout>}"
SRC="$HIVE/src/crux"
OLD_MODULE="github.com/hivecommons/hive/crux"
NEW_MODULE="github.com/hivecommons/crux"

# Hand-maintained in hivecommons/crux; never touched by a sync.
KEEP=(.git .github .goreleaser.yaml Dockerfile Dockerfile.goreleaser LICENSE README.md .gitignore .golangci.yml .dockerignore runbooks)

[ -f "$SRC/go.mod" ] || { echo "::error::$SRC/go.mod not found" >&2; exit 1; }
[ -d "$DEST" ] || { echo "::error::$DEST is not a directory" >&2; exit 1; }
grep -qx "module $OLD_MODULE" "$SRC/go.mod" || {
  echo "::error::$SRC/go.mod does not declare module $OLD_MODULE" >&2; exit 1; }

hive_sha="$(git -C "$HIVE" rev-parse HEAD)"
hive_ref="$(git -C "$HIVE" rev-parse --abbrev-ref HEAD 2>/dev/null || echo detached)"

# 2. Clear DEST except KEEP.
shopt -s dotglob nullglob
for entry in "$DEST"/*; do
  name="$(basename "$entry")"
  keep=0
  for k in "${KEEP[@]}"; do [ "$name" = "$k" ] && keep=1 && break; done
  [ "$keep" = 1 ] || rm -rf "$entry"
done
shopt -u dotglob nullglob

# 3. Copy the module. rsync keeps modes; exclude anything a developer may
# have left in the tree that is not part of the module.
rsync -a --exclude '.git' --exclude 'README.md' "$SRC/" "$DEST/"
if [ -f "$SRC/README.md" ]; then
  mkdir -p "$DEST/docs"
  {
    echo "<!-- Synced from hivecommons/hive src/crux/README.md at ${hive_sha}; edit it there. -->"
    echo
    cat "$SRC/README.md"
  } > "$DEST/docs/MODULE.md"
fi

# 4. Rewrite the module path. `go.mod` first, then every import. The pattern
# is anchored on the quote so a hypothetical github.com/hivecommons/hive/cruxfoo
# cannot be mangled.
sed -i.bak "s#^module ${OLD_MODULE}\$#module ${NEW_MODULE}#" "$DEST/go.mod" && rm -f "$DEST/go.mod.bak"
while IFS= read -r -d '' f; do
  if grep -q "\"${OLD_MODULE}" "$f"; then
    # Two plain substitutions rather than one alternation: BSD sed has no \|.
    sed -i.bak -e "s#\"${OLD_MODULE}/#\"${NEW_MODULE}/#g" -e "s#\"${OLD_MODULE}\"#\"${NEW_MODULE}\"#g" "$f" && rm -f "$f.bak"
  fi
done < <(find "$DEST" -name '*.go' -not -path "$DEST/.git/*" -print0)
# Non-import mentions (Makefile, docs, comments) are left alone except the
# Makefile/doc install lines that name the module path verbatim.
for f in "$DEST/Makefile" "$DEST/docs/MODULE.md"; do
  [ -f "$f" ] && sed -i.bak "s#${OLD_MODULE}#${NEW_MODULE}#g" "$f" && rm -f "$f.bak"
done
# Rewrite relative ../docs/ links in MODULE.md to absolute hive URLs pinned to the synced commit.
if [ -f "$DEST/docs/MODULE.md" ]; then
  sed -i.bak "s#](\.\./docs/#](https://github.com/hivecommons/hive/blob/${hive_sha}/src/docs/#g" "$DEST/docs/MODULE.md" && rm -f "$DEST/docs/MODULE.md.bak"
fi
if grep -rn --include='*.go' "\"${OLD_MODULE}" "$DEST" | grep -q .; then
  echo "::error::module path rewrite incomplete:" >&2
  grep -rn --include='*.go' "\"${OLD_MODULE}" "$DEST" >&2
  exit 1
fi

# 5. Provenance.
cat > "$DEST/.hive-sync" <<EOF
# Generated by hivecommons/hive src/scripts/crux-publish.sh — do not edit.
# This tree is a module-path-rewritten copy of hivecommons/hive src/crux.
# File issues and pull requests in hivecommons/hive.
source=https://github.com/hivecommons/hive/tree/${hive_sha}/src/crux
hive_sha=${hive_sha}
hive_ref=${hive_ref}
synced_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
module_rewrite=${OLD_MODULE} -> ${NEW_MODULE}
EOF

# 6. Guard: the rewritten tree must build on its own.
if [ "${CRUX_SKIP_BUILD:-}" != 1 ]; then
  (
    cd "$DEST"
    export GOWORK=off GOFLAGS=-mod=mod
    echo "crux-publish: go build ./... (GOWORK=off)"
    go build ./...
    echo "crux-publish: go vet ./..."
    go vet ./...
    # The rewrite must not change what go.mod needs; a tidy delta means hive's
    # src/crux/go.mod is itself untidy, which hive's own CI should catch.
    cp go.mod go.mod.pre && cp go.sum go.sum.pre
    go mod tidy
    if ! cmp -s go.mod go.mod.pre || ! cmp -s go.sum go.sum.pre; then
      echo "::warning::go mod tidy changed go.mod/go.sum in the publish tree; committing the tidied files"
      diff -u go.mod.pre go.mod || true
    fi
    rm -f go.mod.pre go.sum.pre
  )
fi

echo "crux-publish: publish tree ready in $DEST (hive ${hive_sha:0:12})"
