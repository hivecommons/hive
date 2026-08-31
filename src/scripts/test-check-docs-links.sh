#!/usr/bin/env bash
# test-check-docs-links.sh — exercises src/scripts/check-docs-links.py against
# small fixture Markdown trees so the checker's pass/fail behavior is proven,
# not just its output against today's src/docs/ snapshot.
#
# Usage: src/scripts/test-check-docs-links.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-docs-links.py"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/docs-links.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

# --- Case 1: a clean tree passes ---------------------------------------
clean="$tmp/clean/docs"
mkdir -p "$clean"
cat > "$clean/a.md" <<'EOF'
# A

See [B](b.md#section-one).

## Self anchor

[back to self](#self-anchor).
EOF
cat > "$clean/b.md" <<'EOF'
# B

## Section one

Text.
EOF
if (cd "$tmp/clean" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_ok "clean tree exits 0"
else
  note_fail "clean tree should exit 0:"; cat /tmp/out.$$
fi
rm -f /tmp/out.$$

# --- Case 2: a missing file is caught -----------------------------------
missing="$tmp/missing/docs"
mkdir -p "$missing"
cat > "$missing/a.md" <<'EOF'
# A

See [gone](nowhere.md).
EOF
if (cd "$tmp/missing" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_fail "missing-file link should exit non-zero"
else
  grep -q "nowhere.md does not exist" /tmp/out.$$ \
    && note_ok "missing file target is reported" \
    || { note_fail "expected 'does not exist' in output:"; cat /tmp/out.$$; }
fi
rm -f /tmp/out.$$

# --- Case 3: a stale anchor (heading renamed) is caught -----------------
stale="$tmp/stale/docs"
mkdir -p "$stale"
cat > "$stale/a.md" <<'EOF'
# A

See [B](b.md#old-heading).
EOF
cat > "$stale/b.md" <<'EOF'
# B

## New heading

Text.
EOF
if (cd "$tmp/stale" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_fail "stale anchor should exit non-zero"
else
  grep -q "no heading matching '#old-heading'" /tmp/out.$$ \
    && note_ok "renamed-heading anchor break is reported (the #5206 class of bug)" \
    || { note_fail "expected anchor mismatch in output:"; cat /tmp/out.$$; }
fi
rm -f /tmp/out.$$

# --- Case 4: GitHub's double-hyphen slug (punctuation between spaces) is
# NOT a false positive -----------------------------------------------------
punct="$tmp/punct/docs"
mkdir -p "$punct"
cat > "$punct/a.md" <<'EOF'
# A

See [B](b.md#8-hub--spoke).
EOF
cat > "$punct/b.md" <<'EOF'
# B

## 8. Hub & spoke

Text.
EOF
if (cd "$tmp/punct" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_ok "double-hyphen GitHub slug from '&' is not a false positive"
else
  note_fail "double-hyphen slug should resolve, got:"; cat /tmp/out.$$
fi
rm -f /tmp/out.$$

# --- Case 5: explicit <a id="..."> anchors are honored ------------------
htmlanchor="$tmp/htmlanchor/docs"
mkdir -p "$htmlanchor"
cat > "$htmlanchor/a.md" <<'EOF'
# A

See [B](b.md#custom-anchor).
EOF
cat > "$htmlanchor/b.md" <<'EOF'
# B

<a id="custom-anchor"></a>

Some content the heading slug would never produce.
EOF
if (cd "$tmp/htmlanchor" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_ok "explicit <a id> anchor is honored"
else
  note_fail "explicit <a id> anchor should resolve, got:"; cat /tmp/out.$$
fi
rm -f /tmp/out.$$

# --- Case 6: links inside fenced code blocks are ignored -----------------
fenced="$tmp/fenced/docs"
mkdir -p "$fenced"
cat > "$fenced/a.md" <<'EOF'
# A

```markdown
[example](does-not-exist.md)
```
EOF
if (cd "$tmp/fenced" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_ok "link inside a fenced code block is not checked"
else
  note_fail "fenced example link should not be checked, got:"; cat /tmp/out.$$
fi
rm -f /tmp/out.$$

# --- Case 7: external links are never checked ----------------------------
external="$tmp/external/docs"
mkdir -p "$external"
cat > "$external/a.md" <<'EOF'
# A

[external](https://example.invalid/does/not/exist) and [mail](mailto:x@example.com).
EOF
if (cd "$tmp/external" && python3 "$checker" docs) >/tmp/out.$$ 2>&1; then
  note_ok "external/mailto links are skipped"
else
  note_fail "external links should not be checked, got:"; cat /tmp/out.$$
fi
rm -f /tmp/out.$$

if [ "$fail" -ne 0 ]; then
  echo "test-check-docs-links FAILED"
  exit 1
fi
echo "test-check-docs-links OK"
