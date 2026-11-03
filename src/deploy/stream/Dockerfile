# hive-stream: headless Twitch streamer for the Hive activity monitor
# (hivecommons/hive#15594). See README.md here and
# src/docs/streaming-the-activity-monitor.md.
#
# Build context is the REPO ROOT (like src/Dockerfile):
#   docker build -f src/deploy/stream/Dockerfile -t hive-stream .
#
# Base images are pinned by digest for the same supply-chain reason as
# src/Dockerfile.contributor (#3746); the golang digest is the one
# src/Dockerfile uses, the bookworm-slim digest the one Dockerfile.contributor
# uses, so a refresh of either is a one-line bump in lockstep with them.
ARG BASE_REGISTRY=docker.io/library

FROM --platform=$BUILDPLATFORM ${BASE_REGISTRY}/golang:1.27-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS authproxy
ARG TARGETOS TARGETARCH
WORKDIR /build
COPY src/deploy/stream/authproxy/*.go ./
# Stdlib-only: a throwaway module keeps this stage independent of the root
# go.mod/go.sum (no download, no replace directives to satisfy).
RUN go mod init hive-stream/authproxy >/dev/null 2>&1 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/authproxy .

FROM ${BASE_REGISTRY}/debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
ENV DEBIAN_FRONTEND=noninteractive
# PulseAudio provides a null sink ("hive") that Chromium plays the tracker's
# merge-zap / new-item-buzz sounds into; ffmpeg captures its monitor so the
# stream carries them. STREAM_AUDIO=silent (entrypoint.sh) restores the old
# anullsrc + --mute-audio behaviour without PulseAudio running.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl tini procps \
      xvfb x11-utils xdotool \
      pulseaudio pulseaudio-utils \
      chromium \
      ffmpeg \
      fonts-dejavu-core fonts-noto-core fonts-noto-color-emoji fonts-liberation \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 1000 --create-home --shell /usr/sbin/nologin stream \
 && install -d -o stream -g stream /run/stream

COPY --from=authproxy /out/authproxy /usr/local/bin/authproxy
COPY src/deploy/stream/entrypoint.sh src/deploy/stream/healthcheck.sh /usr/local/bin/
RUN chmod 0755 /usr/local/bin/entrypoint.sh /usr/local/bin/healthcheck.sh \
 && chromium --version && ffmpeg -version | head -1 && Xvfb -help >/dev/null 2>&1 \
 && pulseaudio --version && ffmpeg -hide_banner -devices 2>/dev/null | grep -q pulse

USER stream
ENV HOME=/home/stream RUN_DIR=/run/stream \
    DASHBOARD_PATH="/?kiosk=1#section-activity-monitor-section" \
    TWITCH_INGEST=rtmp://live.twitch.tv/app \
    VIDEO_SIZE=1920x1080 FPS=30 BITRATE=4500k RELOAD_EVERY=30m STREAM_AUDIO=pulse
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 CMD ["/usr/local/bin/healthcheck.sh"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
