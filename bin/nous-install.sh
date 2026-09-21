#!/usr/bin/env bash
# Install the external Nous strategy-evolution framework at a reviewed commit.
#
# Supply-chain pin (#8104): do not install whatever upstream branch HEAD points
# at. To bump, review the target upstream commit, update NOUS_PIN_SHA in this
# file (or test with HIVE_NOUS_PIN=<40-hex-sha>), and land that change by PR.
# The pinned upstream currently ships uv.lock but no pip-compatible
# requirements/constraints file, so pip cannot enforce that lock directly here;
# the script still refuses to run pip unless the Git checkout matches the pin.
set -euo pipefail

NOUS_DIR="${NOUS_DIR:-/opt/nous}"
NOUS_VENV="${NOUS_DIR}/venv"
NOUS_RUN_DIR="${NOUS_RUN_DIR:-/var/run/nous}"
NOUS_REPO="https://github.com/AI-native-Systems-Research/agentic-strategy-evolution"
# Pinned 2026-09-21 for #8104. See the header above before changing.
NOUS_PIN_SHA="${HIVE_NOUS_PIN:-34193512f9aecca8244c3a99c67873e578193319}"
NOUS_CHOWN_USER="${NOUS_CHOWN_USER-dev:dev}"

die() {
  echo "ERROR: $*" >&2
  exit 1
}

if [[ ! "$NOUS_PIN_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  die "NOUS pin must be a full 40-character lowercase commit SHA, got '$NOUS_PIN_SHA'"
fi

if [ -d "$NOUS_DIR/.git" ]; then
  echo "Nous already installed at $NOUS_DIR — checking out pinned commit $NOUS_PIN_SHA"
  cd "$NOUS_DIR"
  if git remote get-url origin >/dev/null 2>&1; then
    git remote set-url origin "$NOUS_REPO"
  else
    git remote add origin "$NOUS_REPO"
  fi
else
  echo "Cloning Nous framework to $NOUS_DIR for pinned commit $NOUS_PIN_SHA"
  git clone --no-checkout "$NOUS_REPO" "$NOUS_DIR"
  cd "$NOUS_DIR"
fi

git fetch --depth 1 origin "$NOUS_PIN_SHA"
git checkout --detach FETCH_HEAD
checked_out_sha="$(git rev-parse HEAD)"
if [ "$checked_out_sha" != "$NOUS_PIN_SHA" ]; then
  die "refusing to install Nous: expected $NOUS_PIN_SHA, checked out $checked_out_sha"
fi
echo "Verified Nous checkout at $checked_out_sha"

if [ ! -d "$NOUS_VENV" ]; then
  echo "Creating virtual environment at $NOUS_VENV"
  python3 -m venv "$NOUS_VENV"
fi

echo "Installing Nous into venv"
pip_args=(install -e .)
if [ -f requirements.lock ]; then
  echo "Using upstream pip constraints from requirements.lock"
  pip_args=(install -c requirements.lock -e .)
elif [ -f requirements.txt ]; then
  echo "Using upstream pip requirements from requirements.txt"
  pip_args=(install -r requirements.txt -e .)
elif [ -f uv.lock ]; then
  echo "Upstream ships uv.lock, but no pip-compatible lock/requirements file; installing from verified commit with pip dependency resolution"
fi
"$NOUS_VENV/bin/pip" "${pip_args[@]}" 2>&1

mkdir -p "$NOUS_RUN_DIR"/{governor,repo,snapshots}
if [ -n "$NOUS_CHOWN_USER" ]; then
  chown -R "$NOUS_CHOWN_USER" "$NOUS_RUN_DIR" "$NOUS_DIR"
fi

echo "Verifying installation…"
"$NOUS_VENV/bin/python3" -c "from run_campaign import run_campaign; print('Nous framework installed OK')"

echo ""
echo "Use $NOUS_VENV/bin/python3 to run Nous scripts"
