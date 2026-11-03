#!/usr/bin/env bash
# Public-safe broadcast gate for the hive-stream image (hivecommons/hive#15966).
#
# The hivecommonsfleet deployment (src/deploy/stream/fleet/) sets
# STREAM_PROFILE=public-safe. Twitch output is public regardless of dashboard
# authentication, so that profile refuses to start the encoder unless the
# owner has explicitly enabled it, acknowledged the public-safe content policy
# (src/docs/streaming-the-activity-monitor.md#public-safe-content-policy),
# pointed it at a real public-safe source, and installed a stream key. It also
# refuses any dashboard credential: a public-safe stream renders only what an
# anonymous visitor of the hub would see, so per-user fleet visibility can
# never reach the broadcast.
#
# With STREAM_PROFILE unset (the existing twitch.tv/hivecommons deployment)
# this exits 0 and changes nothing. entrypoint.sh runs it before starting any
# process; a non-zero exit stops the container before Xvfb, Chromium or ffmpeg.
# Nothing here prints a secret.
set -euo pipefail

# Bump the version when the policy text changes, so an old acknowledgement
# does not silently cover new content.
PUBLIC_SAFE_POLICY_VERSION="hivecommonsfleet-public-safe-v1"

STREAM_PROFILE="${STREAM_PROFILE:-}"
refuse() { printf 'stream: public-safe gate: refusing to start: %s\n' "$*" >&2; exit 3; }

case "$STREAM_PROFILE" in
  '') exit 0 ;;
  public-safe) ;;
  *) refuse "unknown STREAM_PROFILE '$STREAM_PROFILE' (only 'public-safe' or unset)" ;;
esac

[ "${STREAM_ENABLED:-false}" = true ] \
  || refuse "STREAM_ENABLED is not 'true' (broadcast is disabled by default; only the owner enables it)"
[ "${PUBLIC_SAFE_POLICY_ACK:-}" = "$PUBLIC_SAFE_POLICY_VERSION" ] \
  || refuse "PUBLIC_SAFE_POLICY_ACK must be '$PUBLIC_SAFE_POLICY_VERSION' (the owner's acknowledgement of the public-safe content policy)"
[ -z "${DASHBOARD_TOKEN:-}" ] \
  || refuse "DASHBOARD_TOKEN is set; a public-safe stream carries no dashboard credential and renders only the anonymous public view"

url="${DASHBOARD_URL:-}"
path="${DASHBOARD_PATH:-}"
case "$url" in
  https://*) ;;
  *) refuse "DASHBOARD_URL must be the hub's public https origin" ;;
esac
case "$url$path" in
  *CHANGE-ME*) refuse "DASHBOARD_URL/DASHBOARD_PATH still hold the CHANGE-ME placeholder; set the public-safe hub view first" ;;
esac
[ -n "$path" ] || refuse "DASHBOARD_PATH is empty; set it to the public-safe hub view"

# entrypoint.sh hands a *.flv value to ffmpeg verbatim, and ffmpeg reads
# rtmp://…/x.flv as a network sink — so only an absolute local path counts as
# a dry run.
case "${STREAM_OUTPUT:-}" in
  '') [ -n "${TWITCH_STREAM_KEY:-}" ] || refuse "TWITCH_STREAM_KEY is unset (the owner installs it out of band; never in source or config)" ;;
  *://*|*:*) refuse "STREAM_OUTPUT may only be 'null' or an absolute local *.flv path under the public-safe profile" ;;
  null|/*.flv) ;;  # local dry run: nothing leaves the pod
  *) refuse "STREAM_OUTPUT may only be 'null' or an absolute local *.flv path under the public-safe profile" ;;
esac

printf 'stream: public-safe gate: enabled, policy %s acknowledged, no dashboard credential\n' "$PUBLIC_SAFE_POLICY_VERSION"
