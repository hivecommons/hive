#!/usr/bin/env bash
set -euo pipefail

# A versioned go install resolves a separate module graph, so the preceding
# go mod download does not protect this step from transient proxy failures.
attempts=5
delay=2
for ((attempt = 1; attempt <= attempts; attempt++)); do
  if go install gotest.tools/gotestsum@v1.13.0; then
    break
  else
    rc=$?
  fi
  if ((attempt == attempts)); then
    echo "::error::gotestsum install failed after $attempts attempts (last rc=$rc)" >&2
    exit "$rc"
  fi
  echo "::warning::gotestsum install failed (attempt $attempt/$attempts, rc=$rc); retrying in ${delay}s" >&2
  sleep "$delay"
  delay=$((delay * 2))
done

bin_dir="$(go env GOPATH)/bin"
"$bin_dir/gotestsum" --version
echo "$bin_dir" >> "$GITHUB_PATH"
