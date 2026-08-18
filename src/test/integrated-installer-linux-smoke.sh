#!/usr/bin/env bash
set -euo pipefail

hive_root="${1:?Hive v2 source root is required}"
visual_bundle="${2:?unpacked Visual Hive release bundle is required}"
work_root="${3:-$(mktemp -d /tmp/hive-integrated-linux.XXXXXX)}"
hive_commit="${HIVE_COMMIT:-$(git -C "$hive_root" rev-parse HEAD)}"
visual_commit="${VISUAL_HIVE_COMMIT:-$(sed -nE 's/^[[:space:]]*"gitCommit":[[:space:]]*"([a-f0-9]{40})",?$/\1/p' "$visual_bundle/release-manifest.json")}"
node_version="22.23.1"
release_version="vlocal-linux"

[[ "$hive_commit" =~ ^[a-f0-9]{40}$ ]] || { echo "immutable Hive commit is required" >&2; exit 1; }
[[ "$visual_commit" =~ ^[a-f0-9]{40}$ ]] || { echo "immutable Visual Hive commit is required" >&2; exit 1; }

mkdir -p "$work_root/node" "$work_root/release"
archive="node-v${node_version}-linux-x64.tar.gz"
base="https://nodejs.org/dist/v${node_version}"
curl --fail --silent --show-error --location "$base/SHASUMS256.txt" -o "$work_root/node/SHASUMS256.txt"
curl --fail --silent --show-error --location "$base/$archive" -o "$work_root/node/$archive"
(cd "$work_root/node" && grep "  $archive$" SHASUMS256.txt | sha256sum --check --strict -)
tar -xzf "$work_root/node/$archive" -C "$work_root/node"
node_root="$work_root/node/node-v${node_version}-linux-x64"
node_runtime="$work_root/node-runtime"
mkdir -p "$node_runtime/node_modules"
cp "$node_root/bin/node" "$node_runtime/node"
cp "$node_root/LICENSE" "$node_runtime/LICENSE.node.txt"
cp -RL "$node_root/lib/node_modules/npm" "$node_runtime/node_modules/npm"
cp -RL "$node_root/lib/node_modules/corepack" "$node_runtime/node_modules/corepack"
cp "$hive_root/runtime-launchers/linux/npm" "$hive_root/runtime-launchers/linux/npx" \
  "$hive_root/runtime-launchers/linux/corepack" "$hive_root/runtime-launchers/linux/pnpm" \
  "$hive_root/runtime-launchers/linux/pnpx" "$hive_root/runtime-launchers/linux/yarn" \
  "$hive_root/runtime-launchers/linux/yarnpkg" "$node_runtime/"
chmod 0755 "$node_runtime/node" "$node_runtime/npm" "$node_runtime/npx" "$node_runtime/corepack" \
  "$node_runtime/pnpm" "$node_runtime/pnpx" "$node_runtime/yarn" "$node_runtime/yarnpkg"

(cd "$hive_root" && go build -trimpath -ldflags "-s -w -X main.gitHash=$hive_commit -X main.gitShort=${hive_commit:0:12} -X main.integratedVersion=$release_version" -o "$work_root/hive" ./cmd/hive)
distribution="$work_root/release/hive-integrated-${release_version}-linux-amd64"
(cd "$hive_root" && go run ./cmd/hive-dist \
  --hive "$work_root/hive" --hive-commit "$hive_commit" --hive-version "$release_version" \
  --visual-hive "$visual_bundle" --visual-hive-commit "$visual_commit" \
  --node "$node_root/bin/node" --node-runtime "$node_runtime" --node-license "$node_root/LICENSE" --node-version "v${node_version}" \
  --skill "$hive_root/skills/hive" --target-os linux --target-arch amd64 --output "$distribution" >/dev/null)

(cd "$work_root/release" && tar -czf "hive-integrated-${release_version}-linux-amd64.tar.gz" "hive-integrated-${release_version}-linux-amd64")
(cd "$work_root/release" && sha256sum "hive-integrated-${release_version}-linux-amd64.tar.gz" > "hive-integrated-${release_version}-linux-amd64.tar.gz.sha256")

export HOME="$work_root/home"
export HIVE_VERSION="$release_version"
export HIVE_RELEASE_DIR="$work_root/release"
export HIVE_SKIP_ATTESTATION=1
export HIVE_INSTALL_DIR="$work_root/installed"
mkdir -p "$HOME"
sh "$hive_root/install-integrated.sh"
test "$(readlink "$HOME/.local/bin/visual-hive")" = "$HIVE_INSTALL_DIR/bin/visual-hive"
"$HIVE_INSTALL_DIR/runtime/node" -e \
  'const manifest=require(process.argv[1]); if (manifest.hosted_controller_protocol !== 1) process.exit(1)' \
  "$HIVE_INSTALL_DIR/distribution-manifest.json"
launcher_path="$work_root/launcher-path"
mkdir -p "$launcher_path"
ln -s "$(command -v dirname)" "$launcher_path/dirname"
ln -s "$(command -v readlink)" "$launcher_path/readlink"
(
  PATH="$HOME/.local/bin:$launcher_path"
  export PATH
  ! command -v node >/dev/null 2>&1
  visual-hive --version
)
"$HIVE_INSTALL_DIR/hive" --version | grep -Fx "Hive $release_version"
"$HIVE_INSTALL_DIR/hive" --version | grep -Fx "commit: $hive_commit"
"$HIVE_INSTALL_DIR/hive" setup --repo DavidDiaz0317/hive-visual-hive-install-proof-20260713-234243 --coverage comprehensive --automation advisory --provider codex --visual-hive --runtime local --plan --json > "$work_root/setup-plan.json"
grep -q '"schema_version": "hive.setup-plan.v2"' "$work_root/setup-plan.json"
grep -q '"execution_mode": "local"' "$work_root/setup-plan.json"
grep -q '"read_only": true' "$work_root/setup-plan.json"
grep -q '"state_dir": ' "$work_root/setup-plan.json"
echo "Linux integrated installer smoke passed: $work_root"
