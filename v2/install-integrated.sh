#!/usr/bin/env sh
set -eu

version="${HIVE_VERSION:-latest}"
repository="${HIVE_REPOSITORY:-DavidDiaz0317/hive}"
install_dir="${HIVE_INSTALL_DIR:-$HOME/.local/share/hive}"
release_dir="${HIVE_RELEASE_DIR:-}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --repo) repository="$2"; shift 2 ;;
    --install-dir) install_dir="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

validate_install_dir_path() {
  candidate="$1"
  case "$candidate" in
    /*) ;;
    *)
      echo "Hive --install-dir must be an absolute path to a dedicated Hive directory." >&2
      return 1
      ;;
  esac
  while [ "$candidate" != "/" ] && [ "${candidate%/}" != "$candidate" ]; do
    candidate=${candidate%/}
  done
  if [ "$candidate" != "/" ]; then
    case "$candidate/" in
      *'/../'*|*'/./'*|*'//'*)
        echo "Hive --install-dir must not contain '.', '..', or repeated path segments: $candidate" >&2
        return 1
        ;;
    esac
  fi
  case "$candidate" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/usr/local|/usr/local/bin|/usr/local/lib|/usr/local/share|/var/lib|/var/tmp)
      echo "Refusing unsafe Hive install directory $candidate. Choose a new dedicated leaf such as $HOME/.local/share/hive." >&2
      return 1
      ;;
  esac
  for common_parent in "$HOME" "$HOME/.local" "$HOME/.local/share" "$HOME/.config" "$HOME/.cache" "$HOME/.codex" "$HOME/bin" "${TMPDIR:-/tmp}"; do
    common_parent=${common_parent%/}
    if [ "$candidate" = "$common_parent" ]; then
      echo "Refusing unsafe Hive install directory $candidate. It is a home or common parent directory; choose a new dedicated Hive leaf." >&2
      return 1
    fi
  done
  leaf=$(basename -- "$candidate")
  leaf_lower=$(printf '%s' "$leaf" | tr '[:upper:]' '[:lower:]')
  case "$leaf_lower" in
    hive|.hive|hive[-_.]*|*[-_.]hive|*[-_.]hive[-_.]*|installed|install[-_.]*) ;;
    *)
      echo "Refusing non-dedicated Hive install leaf '$leaf'. Use a directory named hive, hive-*, *-hive, installed, or install-*." >&2
      return 1
      ;;
  esac
  install_dir="$candidate"
}

validate_install_dir_path "$install_dir"

[ "$(uname -s)" = "Linux" ] || { echo "This installer supports Linux; use install-integrated.ps1 on Windows." >&2; exit 1; }
[ "$(uname -m)" = "x86_64" ] || { echo "The integrated Hive installer currently supports Linux x64 only." >&2; exit 1; }
[ -n "$release_dir" ] || command -v gh >/dev/null 2>&1 || { echo "GitHub CLI is required for signed release verification and authorization: https://cli.github.com/" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required." >&2; exit 1; }
case "$repository" in
  */*)
    owner=${repository%%/*}
    name=${repository#*/}
    case "$owner" in '') echo "Repository must be one owner/name pair." >&2; exit 1 ;; esac
    case "$name" in */*|'') echo "Repository must be one owner/name pair." >&2; exit 1 ;; esac
    ;;
  *) echo "Repository must be one owner/name pair." >&2; exit 1 ;;
esac
case "$owner$name" in *[!A-Za-z0-9_.-]*) echo "Repository contains unsafe characters." >&2; exit 1 ;; esac
if [ -z "$release_dir" ]; then
  gh auth status >/dev/null 2>&1 || {
    echo "GitHub CLI is not authenticated. Run 'gh auth login', then retry the installer." >&2
    exit 1
  }
fi

if [ "$version" = "latest" ]; then
  [ -z "$release_dir" ] || { echo "Set a concrete HIVE_VERSION with HIVE_RELEASE_DIR." >&2; exit 1; }
  version="$(gh api "repos/$repository/releases/latest" --jq .tag_name)"
fi
[ -n "$version" ] || { echo "No published integrated Hive release was found." >&2; exit 1; }
case "$version" in
  ''|[!A-Za-z0-9]*|*[!A-Za-z0-9._-]*) echo "Release version contains unsafe characters." >&2; exit 1 ;;
esac
[ "${#version}" -le 128 ] || { echo "Release version is too long." >&2; exit 1; }
if [ -z "$release_dir" ]; then
  printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$' || {
    echo "Published integrated release versions must match vMAJOR.MINOR.PATCH-integrated.NUMBER." >&2
    exit 1
  }
fi

resolve_release_commit() {
  resolved="$(gh api "repos/$repository/commits/$version" --jq .sha)"
  printf '%s\n' "$resolved" | grep -Eq '^[a-f0-9]{40}$' || {
    echo "Release tag $version did not resolve to an exact 40-character commit." >&2
    exit 1
  }
  printf '%s\n' "$resolved"
}

release_commit=""
if [ -z "$release_dir" ]; then
  release_commit="$(resolve_release_commit)"
fi

asset="hive-integrated-$version-linux-amd64.tar.gz"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/hive-install.XXXXXX")"
activated=0
committed=0
had_previous=0
link_backup_complete=0
link_candidate_installed=0
visual_link_backup_complete=0
visual_link_candidate_installed=0
skill_backup_complete=0
skill_copy_started=0
parent_dir="$(dirname "$install_dir")"
backup_dir="$install_dir.previous"
link_path="$HOME/.local/bin/hive"
link_backup="$link_path.previous.$$"
visual_link_path="$HOME/.local/bin/visual-hive"
visual_link_backup="$visual_link_path.previous.$$"
codex_home="${CODEX_HOME:-$HOME/.codex}"
skill_target="$codex_home/skills/hive"
skill_backup="$skill_target.previous.$$"
distribution_validator_node=""
transition_journal="$install_dir.installer-transition.json"
transition_helper="$tmp/hive-installer-transition"
transition_prepared=0
transition_completed=0
cleanup() {
  case "$tmp" in "${TMPDIR:-/tmp}"/hive-install.*) rm -rf -- "$tmp" ;; esac
}
rollback() {
  rollback_failed=0
  if [ "$skill_backup_complete" = "1" ] || [ "$skill_copy_started" = "1" ]; then
    case "$skill_target" in "$codex_home"/skills/*)
      rm -rf -- "$skill_target" || rollback_failed=1
      if [ "$skill_backup_complete" = "1" ] && [ -e "$skill_backup" ]; then mv -- "$skill_backup" "$skill_target" || rollback_failed=1; fi
      ;;
      *) rollback_failed=1 ;;
    esac
  fi
  if [ "$link_backup_complete" = "1" ] || [ "$link_candidate_installed" = "1" ]; then
    rm -f -- "$link_path" || rollback_failed=1
    if [ "$link_backup_complete" = "1" ] && { [ -e "$link_backup" ] || [ -L "$link_backup" ]; }; then mv -- "$link_backup" "$link_path" || rollback_failed=1; fi
  fi
	if [ "$visual_link_backup_complete" = "1" ] || [ "$visual_link_candidate_installed" = "1" ]; then
	  rm -f -- "$visual_link_path" || rollback_failed=1
	  if [ "$visual_link_backup_complete" = "1" ] && { [ -e "$visual_link_backup" ] || [ -L "$visual_link_backup" ]; }; then mv -- "$visual_link_backup" "$visual_link_path" || rollback_failed=1; fi
	fi
	if [ "$activated" = "1" ]; then
	  case "$install_dir" in "$parent_dir"/*)
		candidate_removed=1
		if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
		  if require_recognized_hive_distribution "$install_dir" "newly activated Hive candidate"; then
			rm -rf -- "$install_dir" || { rollback_failed=1; candidate_removed=0; }
		  else
			rollback_failed=1
			candidate_removed=0
		  fi
		fi
		if [ "$candidate_removed" = "1" ] && [ "$had_previous" = "1" ] && { [ -e "$backup_dir" ] || [ -L "$backup_dir" ]; }; then
		  if require_recognized_hive_distribution "$backup_dir" "Hive rollback backup"; then
			mv -- "$backup_dir" "$install_dir" || rollback_failed=1
		  else
			rollback_failed=1
		  fi
		fi
		;;
		*) rollback_failed=1 ;;
	  esac
	fi
	if [ "$transition_prepared" = "1" ] && [ "$transition_completed" = "0" ] && [ -f "$transition_journal" ]; then
	  "$transition_helper" installer-transition --phase rollback --journal "$transition_journal" --runtime-executable "$install_dir/hive" --json >/dev/null || rollback_failed=1
	fi
  [ "$rollback_failed" = "0" ]
}
finish() {
  status=$?
  trap - EXIT INT TERM
	if [ "$status" -ne 0 ] && { [ "$activated" = "1" ] || [ "$transition_prepared" = "1" ]; } && [ "$committed" = "0" ]; then
    if rollback; then
	  echo "Hive installation failed; the previous installation, launcher, and Codex skill were restored; persistent schedulers were also restored." >&2
    else
      echo "Hive installation failed and rollback was incomplete; inspect $backup_dir, $link_backup, $visual_link_backup, and $skill_backup." >&2
    fi
  fi
  cleanup
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT TERM

if [ -n "$release_dir" ]; then
  cp -- "$release_dir/$asset" "$release_dir/$asset.sha256" "$tmp/"
else
  gh release download "$version" --repo "$repository" --pattern "$asset" --pattern "$asset.sha256" --dir "$tmp"
fi
if [ "${HIVE_SKIP_ATTESTATION:-0}" = "1" ]; then
  [ -n "$release_dir" ] || { echo "HIVE_SKIP_ATTESTATION is allowed only with HIVE_RELEASE_DIR." >&2; exit 1; }
else
  gh attestation verify "$tmp/$asset" \
    --repo "$repository" \
    --signer-workflow "$repository/.github/workflows/integrated-release.yml" \
    --source-ref "refs/tags/$version" \
    --source-digest "$release_commit" \
    --signer-digest "$release_commit" \
    --deny-self-hosted-runners >/dev/null
  current_release_commit="$(resolve_release_commit)"
  [ "$current_release_commit" = "$release_commit" ] || {
    echo "Release tag $version moved from $release_commit to $current_release_commit during verification." >&2
    exit 1
  }
fi
(cd "$tmp" && sha256sum --check "$asset.sha256")
mkdir "$tmp/extract"
entries_file="$tmp/archive.entries"
tar -tzf "$tmp/$asset" > "$entries_file"
entry_count="$(wc -l < "$entries_file" | tr -d ' ')"
case "$entry_count" in ''|*[!0-9]*) echo "Release archive entry count is invalid." >&2; exit 1 ;; esac
[ "$entry_count" -gt 0 ] && [ "$entry_count" -le 50000 ] || { echo "Release archive has an empty or excessive entry inventory." >&2; exit 1; }
archive_root=""
while IFS= read -r raw_entry; do
  entry=${raw_entry%/}
  [ -n "$entry" ] || continue
  case "$entry" in
    /*|*\\*|*:*|./*|../*|*/./*|*/../*|*/.|*/..|*//* )
      echo "Unsafe release archive path: $entry" >&2
      exit 1
      ;;
  esac
  entry_root=${entry%%/*}
  [ -n "$archive_root" ] || archive_root=$entry_root
  [ "$entry_root" = "$archive_root" ] || { echo "Release archive must contain exactly one root directory." >&2; exit 1; }
done < "$entries_file"
[ -n "$archive_root" ] || { echo "Release archive has no root directory." >&2; exit 1; }
types_file="$tmp/archive.types"
tar -tvzf "$tmp/$asset" > "$types_file"
awk '{ type=substr($0,1,1); if (type != "-" && type != "d") exit 1 }' "$types_file" || {
  echo "Release archive contains a link or unsupported entry type." >&2
  exit 1
}
tar -xzf "$tmp/$asset" -C "$tmp/extract"
top_level_count="$(find "$tmp/extract" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
[ "$top_level_count" = "1" ] || { echo "Release archive must extract exactly one distribution root." >&2; exit 1; }
source_dir="$tmp/extract/$archive_root"
[ -d "$source_dir" ] && [ ! -L "$source_dir" ] || { echo "Release archive has no ordinary distribution directory." >&2; exit 1; }
if find "$source_dir" -type l -print -quit | grep -q . || find "$source_dir" -mindepth 1 ! -type d ! -type f -print -quit | grep -q .; then
  echo "Release archive contains a link or unsupported extracted entry." >&2
  exit 1
fi
extracted_files="$(find "$source_dir" -type f | wc -l | tr -d ' ')"
extracted_bytes="$(du -sb "$source_dir" | awk '{print $1}')"
[ "$extracted_files" -le 50000 ] && [ "$extracted_bytes" -le 1073741824 ] || { echo "Release archive exceeds extracted file or byte bounds." >&2; exit 1; }

distribution_validator_node="$source_dir/runtime/node"
[ -f "$source_dir/distribution-manifest.json" ] && [ ! -L "$source_dir/distribution-manifest.json" ] || { echo "Release manifest is not an ordinary file." >&2; exit 1; }
[ -f "$distribution_validator_node" ] && [ ! -L "$distribution_validator_node" ] || { echo "Bundled Node validator is not an ordinary file." >&2; exit 1; }
[ "$(grep -Fxc '      "path": "runtime/node",' "$source_dir/distribution-manifest.json")" = "1" ] || { echo "Release manifest lacks one exact runtime/node inventory entry." >&2; exit 1; }
expected_node_sha="$(grep -A1 -F '      "path": "runtime/node",' "$source_dir/distribution-manifest.json" | tail -n 1 | sed -n 's/^[[:space:]]*"sha256": "\([a-f0-9]*\)",$/\1/p')"
[ "${#expected_node_sha}" = "64" ] || { echo "Release manifest Node digest is invalid." >&2; exit 1; }
actual_node_sha="$(sha256sum "$distribution_validator_node" | awk '{print $1}')"
[ "$actual_node_sha" = "$expected_node_sha" ] || { echo "Bundled Node validator digest does not match the release manifest." >&2; exit 1; }
validate_hive_distribution() {
  validation_root="$1"
  expected_version="${2:-}"
  env -u NODE_OPTIONS -u NODE_PATH "$distribution_validator_node" - "$validation_root" "$expected_version" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const suppliedRoot = path.resolve(process.argv[2]);
const expectedVersion = process.argv[3] || '';
if (fs.lstatSync(suppliedRoot).isSymbolicLink()) throw new Error(`distribution root is a symbolic link: ${suppliedRoot}`);
const root = fs.realpathSync(suppliedRoot);
const manifest = JSON.parse(fs.readFileSync(path.join(root, 'distribution-manifest.json'), 'utf8'));
if (manifest.schema_version !== 'hive.integrated-distribution.v1' || manifest.os !== 'linux' || manifest.architecture !== 'amd64') throw new Error('distribution platform mismatch');
if (!/^[a-f0-9]{40}$/.test(manifest.hive_commit || '') || !/^[a-f0-9]{40}$/.test(manifest.visual_hive_commit || '') || manifest.hosted_controller_protocol !== 1 || !/^v22\./.test(manifest.node_version || '') || !Array.isArray(manifest.files) || manifest.files.length === 0) throw new Error('distribution identity is incomplete');
if (expectedVersion && manifest.hive_version !== expectedVersion) throw new Error(`distribution Hive version ${manifest.hive_version || '<missing>'} does not match requested release ${expectedVersion}`);
if (manifest.hive_version && !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(manifest.hive_version)) throw new Error('distribution Hive version is unsafe');
const inventoried = new Set();
for (const file of manifest.files) {
  if (!file.path || path.posix.isAbsolute(file.path) || file.path.includes('\\') || file.path.split('/').includes('..')) throw new Error(`unsafe inventory path: ${file.path}`);
  if (!Number.isSafeInteger(file.size) || file.size < 0 || !/^[a-f0-9]{64}$/.test(file.sha256 || '')) throw new Error(`invalid inventory identity: ${file.path}`);
  const target = fs.realpathSync(path.join(root, ...file.path.split('/')));
  if (!target.startsWith(root + path.sep)) throw new Error(`inventory path escaped root: ${file.path}`);
  const data = fs.readFileSync(target);
  if (data.length !== file.size || crypto.createHash('sha256').update(data).digest('hex') !== file.sha256) throw new Error(`inventory mismatch: ${file.path}`);
  if (inventoried.has(file.path)) throw new Error(`duplicate inventory path: ${file.path}`);
  inventoried.add(file.path);
}
const actual = [];
const walk = (dir) => {
  for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
    const target = path.join(dir, entry.name);
    if (entry.isSymbolicLink()) throw new Error(`distribution contains symlink: ${path.relative(root, target)}`);
    if (entry.isDirectory()) walk(target);
    else if (entry.isFile()) actual.push(path.relative(root, target).split(path.sep).join('/'));
    else throw new Error(`distribution contains unsupported entry: ${path.relative(root, target)}`);
  }
};
walk(root);
const expectedActual = new Set([...inventoried, 'distribution-manifest.json']);
for (const file of actual) if (!expectedActual.has(file)) throw new Error(`uninventoried distribution file: ${file}`);
for (const file of expectedActual) if (!actual.includes(file)) throw new Error(`missing distribution file: ${file}`);
const requiredFiles = ['hive', 'runtime/node', 'visual-hive/visual-hive.mjs', 'visual-hive/release-manifest.json', 'skills/hive/SKILL.md', 'skills/hive/agents/openai.yaml'];
if (expectedVersion || inventoried.has('bin/visual-hive')) requiredFiles.push('bin/visual-hive');
for (const required of requiredFiles) {
  if (!fs.statSync(path.join(root, ...required.split('/'))).isFile()) throw new Error(`missing ${required}`);
}
NODE
}

require_recognized_hive_distribution() {
  existing_path="$1"
  existing_label="$2"
  if [ -L "$existing_path" ] || [ ! -d "$existing_path" ] || [ -z "$distribution_validator_node" ]; then
    echo "Refusing to replace $existing_label at $existing_path because it is not an exact recognized Hive distribution. Move it aside manually or choose a new dedicated --install-dir; no existing bytes were changed." >&2
    return 1
  fi
  if ! validate_hive_distribution "$existing_path"; then
    echo "Refusing to replace $existing_label at $existing_path because it is not an exact recognized Hive distribution (manifest or inventory validation failed). Move it aside manually or choose a new dedicated --install-dir; no existing bytes were changed." >&2
    return 1
  fi
}

require_recognized_hive_launcher() {
  existing_path="$1"
  expected_target="$2"
  if ! env -u NODE_OPTIONS -u NODE_PATH "$distribution_validator_node" - "$existing_path" "$expected_target" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const launcher = path.resolve(process.argv[2]);
const expected = path.resolve(process.argv[3]);
const item = fs.lstatSync(launcher);
if (!item.isSymbolicLink()) throw new Error('launcher is not a symbolic link');
const target = path.resolve(path.dirname(launcher), fs.readlinkSync(launcher));
if (target !== expected) throw new Error(`launcher points to ${target}, expected ${expected}`);
NODE
  then
    echo "Refusing to replace $existing_path because it is not the launcher owned by the recognized Hive installation at $install_dir. Move it aside manually; no existing bytes were changed." >&2
    return 1
  fi
}

require_recognized_hive_skill() {
  existing_path="$1"
  current_skill="$2"
  incoming_skill="$3"
  if ! env -u NODE_OPTIONS -u NODE_PATH "$distribution_validator_node" - "$existing_path" "$current_skill" "$incoming_skill" <<'NODE'
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const MAX_FILES = 256;
const MAX_BYTES = 4 * 1024 * 1024;
function inventory(root) {
  const rootItem = fs.lstatSync(root);
  if (!rootItem.isDirectory() || rootItem.isSymbolicLink()) throw new Error('skill root is not a real directory');
  const rows = [];
  let files = 0;
  let bytes = 0;
  function walk(dir) {
    for (const entry of fs.readdirSync(dir, {withFileTypes: true}).sort((a, b) => a.name.localeCompare(b.name, 'en'))) {
      const target = path.join(dir, entry.name);
      const relative = path.relative(root, target).split(path.sep).join('/');
      const item = fs.lstatSync(target);
      if (rows.length >= 512) throw new Error('skill entry-count limit exceeded');
      if (item.isSymbolicLink()) throw new Error(`skill contains a symbolic link: ${relative}`);
      if (item.isDirectory()) {
        rows.push(`D\0${relative}`);
        walk(target);
      }
      else if (item.isFile()) {
        if (++files > MAX_FILES) throw new Error('skill file-count limit exceeded');
        bytes += item.size;
        if (bytes > MAX_BYTES) throw new Error('skill byte limit exceeded');
        rows.push(`${relative}\0${item.size}\0${crypto.createHash('sha256').update(fs.readFileSync(target)).digest('hex')}`);
      } else throw new Error(`skill contains an unsupported entry: ${relative}`);
    }
  }
  walk(root);
  return rows.join('\n');
}
const actual = inventory(path.resolve(process.argv[2]));
const candidates = process.argv.slice(3).filter(Boolean).map(candidate => inventory(path.resolve(candidate)));
if (!candidates.includes(actual)) throw new Error('skill inventory does not match a recognized packaged Hive skill');
NODE
  then
    echo "Refusing to replace $existing_path because it is not an exact packaged Hive skill owned by this installer. Move it aside manually or set CODEX_HOME to a clean location; no existing bytes were changed." >&2
    return 1
  fi
}

validate_hive_distribution "$source_dir" "$version"

if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
  require_recognized_hive_distribution "$install_dir" "existing install target"
	had_previous=1
fi
if [ -e "$backup_dir" ] || [ -L "$backup_dir" ]; then
  require_recognized_hive_distribution "$backup_dir" "existing .previous backup"
fi
if [ -e "$link_path" ] || [ -L "$link_path" ]; then
  if [ ! -e "$install_dir" ]; then
    echo "Refusing to replace launcher $link_path because no recognized current Hive installation owns it. Move it aside manually; no existing bytes were changed." >&2
    exit 1
  fi
  require_recognized_hive_launcher "$link_path" "$install_dir/hive"
fi
if [ -e "$link_backup" ] || [ -L "$link_backup" ]; then
  echo "Refusing to overwrite pre-existing launcher backup $link_backup; move it aside manually after inspection." >&2
  exit 1
fi
if [ -e "$visual_link_path" ] || [ -L "$visual_link_path" ]; then
  if [ ! -e "$install_dir" ]; then
    echo "Refusing to replace launcher $visual_link_path because no recognized current Hive installation owns it. Move it aside manually; no existing bytes were changed." >&2
    exit 1
  fi
  require_recognized_hive_launcher "$visual_link_path" "$install_dir/bin/visual-hive"
fi
if [ -e "$visual_link_backup" ] || [ -L "$visual_link_backup" ]; then
  echo "Refusing to overwrite pre-existing launcher backup $visual_link_backup; move it aside manually after inspection." >&2
  exit 1
fi
if [ -e "$skill_target" ] || [ -L "$skill_target" ]; then
  current_skill=""
  [ -e "$install_dir" ] && current_skill="$install_dir/skills/hive"
  require_recognized_hive_skill "$skill_target" "$current_skill" "$source_dir/skills/hive"
fi
if [ -e "$skill_backup" ] || [ -L "$skill_backup" ]; then
  echo "Refusing to overwrite pre-existing Codex skill backup $skill_backup; move it aside manually after inspection." >&2
  exit 1
fi

mkdir -p "$parent_dir" "$HOME/.local/bin"
staging_dir="$install_dir.new.$$"
case "$staging_dir" in "$parent_dir"/*) ;; *) echo "Unsafe staging path." >&2; exit 1 ;; esac
[ ! -e "$staging_dir" ] && [ ! -L "$staging_dir" ] || { echo "Refusing to overwrite pre-existing staging path $staging_dir; remove it manually after inspection." >&2; exit 1; }
cp -R "$source_dir" "$staging_dir"
validate_hive_distribution "$staging_dir" "$version"
cp -- "$staging_dir/hive" "$transition_helper"
chmod 0700 "$transition_helper"
if [ "$had_previous" = "1" ]; then
	transition_prepared=1
	"$transition_helper" installer-transition --phase prepare --journal "$transition_journal" --owned-install-dir "$install_dir" --json >/dev/null
elif [ -e "$transition_journal" ] || [ -L "$transition_journal" ]; then
	echo "A prior Hive scheduler transition journal remains at $transition_journal but no exact previous installation is available. Restore the prior recognized installation or recover with its original signed installer." >&2
	exit 1
fi
if [ -e "$install_dir" ]; then
  if [ -e "$backup_dir" ] || [ -L "$backup_dir" ]; then
    require_recognized_hive_distribution "$backup_dir" "existing .previous backup"
    rm -rf -- "$backup_dir"
  fi
  require_recognized_hive_distribution "$install_dir" "existing install target"
  mv -- "$install_dir" "$backup_dir"
fi
if ! mv -- "$staging_dir" "$install_dir"; then
  if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    if require_recognized_hive_distribution "$install_dir" "partially activated Hive candidate"; then
      rm -rf -- "$install_dir"
    else
      echo "Hive activation failed and an unrecognized target remains at $install_dir; the recognized backup remains at $backup_dir for manual recovery." >&2
      exit 1
    fi
  fi
  if [ "$had_previous" = "1" ] && [ -e "$backup_dir" ]; then
    require_recognized_hive_distribution "$backup_dir" "Hive rollback backup"
    if mv -- "$backup_dir" "$install_dir"; then
      echo "Hive activation failed; the previous installation was restored." >&2
    else
      echo "Hive activation failed and the previous installation could not be restored; backup remains at $backup_dir." >&2
    fi
  fi
  exit 1
fi
activated=1
if [ -e "$link_path" ] || [ -L "$link_path" ]; then
  mv -- "$link_path" "$link_backup"
  link_backup_complete=1
fi
ln -s "$install_dir/hive" "$link_path"
link_candidate_installed=1
if [ -e "$visual_link_path" ] || [ -L "$visual_link_path" ]; then
  mv -- "$visual_link_path" "$visual_link_backup"
  visual_link_backup_complete=1
fi
ln -s "$install_dir/bin/visual-hive" "$visual_link_path"
visual_link_candidate_installed=1

mkdir -p "$codex_home/skills"
case "$skill_target" in "$codex_home"/skills/*) ;; *) echo "Unsafe Codex skill path." >&2; exit 1 ;; esac
if [ -e "$skill_target" ] || [ -L "$skill_target" ]; then
  mv -- "$skill_target" "$skill_backup"
  skill_backup_complete=1
fi
skill_copy_started=1
cp -R "$install_dir/skills/hive" "$skill_target"
if [ "$transition_prepared" = "1" ]; then
	"$install_dir/hive" installer-transition --phase commit --journal "$transition_journal" --runtime-executable "$install_dir/hive" --json >/dev/null
fi
# The commit-complete journal remains rollback-capable until this install
# commit point and recovery-capable afterward if final cleanup is interrupted.
committed=1
transition_completed=$transition_prepared
if [ "$transition_prepared" = "1" ]; then
	if ! "$install_dir/hive" installer-transition --phase finalize --journal "$transition_journal" --json >/dev/null; then
		echo "Warning: Hive and its persistent schedulers are running on the activated release, but the durable transition journal remains for automatic recovery." >&2
	fi
fi
if [ -e "$backup_dir" ] || [ -L "$backup_dir" ]; then
  if require_recognized_hive_distribution "$backup_dir" "committed Hive backup"; then
    rm -rf -- "$backup_dir" || echo "Warning: prior Hive backup remains at $backup_dir" >&2
  else
    echo "Warning: Hive installed, but the unrecognized backup was preserved at $backup_dir for manual inspection." >&2
  fi
fi
[ ! -e "$link_backup" ] && [ ! -L "$link_backup" ] || rm -rf -- "$link_backup" || echo "Warning: prior launcher backup remains at $link_backup" >&2
[ ! -e "$visual_link_backup" ] && [ ! -L "$visual_link_backup" ] || rm -rf -- "$visual_link_backup" || echo "Warning: prior Visual Hive launcher backup remains at $visual_link_backup" >&2
[ ! -e "$skill_backup" ] || rm -rf -- "$skill_backup" || echo "Warning: prior skill backup remains at $skill_backup" >&2

echo "Hive $version installed at $install_dir"
echo "Authenticate with 'gh auth login' only if needed, then run this exact-path command in the current shell:"
echo "$link_path setup --repo owner/repository --coverage comprehensive --automation auto-merge --provider codex --visual-hive --start --json"
