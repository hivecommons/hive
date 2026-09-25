#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${HIVE_BASE_URL:-}"
PROJECT="${HIVE_OF_WEEK_PROJECT:-}"
WEEK="${HIVE_OF_WEEK_WEEK:-}"
OUT_DIR="${HIVE_OF_WEEK_OUT_DIR:-hive-of-week-renders}"
TITLE="${HIVE_OF_WEEK_TITLE:-Hive of the Week}"
FPS="${HIVE_GOURCE_FPS:-30}"
VIEWPORT="${HIVE_GOURCE_VIEWPORT:-1920x1080}"
SECONDS_PER_DAY="${HIVE_GOURCE_SECONDS_PER_DAY:-1.15}"
VIDEO_NAME="${HIVE_OF_WEEK_VIDEO_NAME:-latest.mp4}"
POSTER_NAME="${HIVE_OF_WEEK_POSTER_NAME:-latest.jpg}"
LOG_NAME="${HIVE_OF_WEEK_LOG_NAME:-source.gource.log}"

if [[ -z "$BASE_URL" ]]; then
  echo "HIVE_BASE_URL is required" >&2
  exit 2
fi

mkdir -p "$OUT_DIR"

query="week=${WEEK}"
if [[ -n "$PROJECT" ]]; then
  query="project=${PROJECT}&${query}"
fi

curl -fsSL "${BASE_URL%/}/api/leaderboard/gource-log?${query}" -o "${OUT_DIR}/${LOG_NAME}"

if [[ ! -s "${OUT_DIR}/${LOG_NAME}" ]]; then
  echo "gource source log is empty; nothing to render" >&2
  exit 3
fi

gource \
  --log-format custom \
  --viewport "$VIEWPORT" \
  --seconds-per-day "$SECONDS_PER_DAY" \
  --file-idle-time 0 \
  --camera-mode overview \
  --highlight-users \
  --highlight-colour FFC857 \
  --background-colour 060301 \
  --font-colour FFE8A3 \
  --dir-colour FFC857 \
  --filename-colour F5D76E \
  --bloom-multiplier 1.35 \
  --bloom-intensity 0.55 \
  --elasticity 0.42 \
  --user-scale 1.25 \
  --hide filenames,dirnames,mouse,progress \
  --title "$TITLE" \
  --output-framerate "$FPS" \
  --output-ppm-stream - \
  "${OUT_DIR}/${LOG_NAME}" \
| ffmpeg -y -r "$FPS" -f image2pipe -vcodec ppm -i - \
  -vcodec libx264 -preset medium -pix_fmt yuv420p -crf "${HIVE_GOURCE_CRF:-20}" \
  -movflags +faststart "${OUT_DIR}/${VIDEO_NAME}"

ffmpeg -y -i "${OUT_DIR}/${VIDEO_NAME}" -frames:v 1 -q:v 2 "${OUT_DIR}/${POSTER_NAME}" >/dev/null 2>&1

echo "${OUT_DIR}/${VIDEO_NAME}"
