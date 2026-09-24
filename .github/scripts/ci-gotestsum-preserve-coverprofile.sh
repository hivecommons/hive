#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <gotestsum command...>" >&2
  exit 2
fi

real_go="$(command -v go)"
wrapper_dir="${HIVE_GOTESTSUM_GO_WRAPPER_DIR:-.ci/gotestsum-go-wrapper}"
marker_dir="${HIVE_GOTESTSUM_COVER_MARKER_DIR:-.ci/gotestsum-coverprofile-seen}"
mkdir -p "$wrapper_dir" "$marker_dir"
wrapper_dir_abs="$(cd "$wrapper_dir" && pwd)"
marker_dir_abs="$(cd "$marker_dir" && pwd)"

cat > "$wrapper_dir_abs/go" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail

args=("$@")
if [ "${args[0]:-}" = "test" ]; then
  for i in "${!args[@]}"; do
    cover=""
    case "${args[$i]}" in
      -coverprofile=*)
        cover="${args[$i]#-coverprofile=}"
        ;;
      -coverprofile)
        next=$((i + 1))
        if [ "$next" -lt "${#args[@]}" ]; then
          cover="${args[$next]}"
        fi
        ;;
    esac

    [ -n "$cover" ] || continue
    safe="$(printf '%s' "$cover" | cksum | awk '{print $1}')"
    marker="${HIVE_GOTESTSUM_COVER_MARKER_DIR_ABS}/${safe}.seen"
    if [ -e "$marker" ]; then
      rerun_cover="${cover}.rerun-${$}-${i}"
      case "${args[$i]}" in
        -coverprofile=*) args[$i]="-coverprofile=${rerun_cover}" ;;
        -coverprofile) args[$next]="$rerun_cover" ;;
      esac
    else
      : > "$marker"
    fi
  done
fi

exec "$REAL_GO" "${args[@]}"
WRAPPER
chmod +x "$wrapper_dir_abs/go"

REAL_GO="$real_go" \
HIVE_GOTESTSUM_COVER_MARKER_DIR_ABS="$marker_dir_abs" \
PATH="$wrapper_dir_abs:$PATH" \
"$@"
