# Scanner image for https://github.com/hivecommons/hive (branch v5) — Anthropic OSS Scanner.
#
# The scanner builds this file with the repository checkout as the build
# context, runs it with network access, then audits the finished image with no
# Internet. Everything a reproducer or a regression test could need (Go
# toolchain, module cache, built binaries, tmux for the agent manager paths)
# is therefore fetched and left in the image here. threat_model.md, next to
# this file, says what to attack and how to run it.
#
# Production ships golang:1.27-alpine (src/Dockerfile). The scanner image is
# Debian-based instead so gdb/delve/strace install cleanly if an analyst wants
# them; the Go module and its go.mod directive (go 1.26.x) are the same.
FROM golang:1.27-trixie

ENV DEBIAN_FRONTEND=noninteractive \
    CGO_ENABLED=1 \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false

# git: pkg/pushbroker, pkg/sandbox and the gitidentity paths shell out to it.
# tmux: pkg/agent drives agents inside tmux servers and its tests spawn real
# ones. procps/jq/curl/python3: used by the bin/ shell pipeline and by anyone
# poking the dashboard on loopback.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates build-essential curl git jq procps tmux sqlite3 python3 \
    && rm -rf /var/lib/apt/lists/*

# CA trust bundle refresh: distrust two Mozilla roots the root programs
# withdrew on 2026-10-17..20. Trixie's ca-certificates 20250419+deb13u1 already
# drops the entries while older package builds still ship them; each entry is
# rewritten only when present so a package refresh cannot break the build
# either way, and the final tests prove the roots are absent from the
# regenerated bundle. Go, curl, git and python3 all read the regenerated system
# bundle. Same recipe as src/Dockerfile; the block can go once every base image
# in use ships a package that no longer carries the entries.
RUN for cert in mozilla/emSign_ECC_Root_CA_-_G3.crt mozilla/Comodo_AAA_Services_root.crt; do \
      if grep -q "^${cert}" /etc/ca-certificates.conf; then \
        sed -i -e "s|^${cert}|!&|" /etc/ca-certificates.conf \
        && grep -q "^!${cert}" /etc/ca-certificates.conf || exit 1; \
      fi; \
    done \
    && update-ca-certificates --fresh \
    && ! test -e /etc/ssl/certs/emSign_ECC_Root_CA_-_G3.pem \
    && ! test -e /etc/ssl/certs/Comodo_AAA_Services_root.pem

# scanner contract: the checkout lives inside the image, at /src.
# The Go module is rooted at src/ (module github.com/hivecommons/hive).
COPY . /src
WORKDIR /src/src

# Every module the tree depends on, then compile every package and every test
# binary once so nothing needs the network later. A failure here fails the
# build and primary_contact gets the log.
RUN go mod download \
    && go build ./... \
    && go test -run '^$' ./... >/dev/null

# The binaries an operator deploys (same targets as src/Dockerfile).
RUN go build -o /usr/local/bin/hive        ./cmd/hive \
    && go build -o /usr/local/bin/hivectl     ./cmd/hivectl \
    && go build -o /usr/local/bin/bd          ./cmd/bd \
    && go build -o /usr/local/bin/hive-backup ./cmd/hive-backup \
    && go build -o /usr/local/bin/apiproxy    ./cmd/apiproxy \
    && /usr/local/bin/hive --version

# Cheap baseline checks so the log shows the image works, without letting a
# failing test abort the build. The full suite (pkg/hub and pkg/agent in
# particular) is sharded across many CI runners, and pkg/agent exercises real
# tmux servers and signal delivery, so the baseline here is the set of
# security-relevant trust boundaries named in threat_model.md. Everything
# else is compiled above and can be run on demand inside the image.
# Known baseline noise: a handful of pkg/appkey and pkg/config tests chmod a
# directory read-only and expect the write to fail; the build runs as root,
# which ignores permission bits, so those report FAIL here and pass in CI.
RUN go vet ./... || echo "WARNING: go vet reported problems"
RUN timeout 1500 go test -short -p 4 -timeout 20m \
      ./pkg/proxy/... ./pkg/github/... ./pkg/sentinel/... ./pkg/ioscan/... \
      ./pkg/logscrub/... ./pkg/pushbroker/... ./pkg/sandbox/... ./pkg/appkey/... \
      ./pkg/keyderive/... ./pkg/delegation/... ./pkg/terminalassert/... \
      ./pkg/spoke/... ./pkg/mint/... ./pkg/apiproxy/... ./pkg/auth/... \
      ./pkg/config/... ./pkg/evidence/... ./pkg/knowledge/... ./pkg/scheduler/... \
      ./pkg/review/... ./pkg/promptsrc/... ./pkg/claims/... ./pkg/toolapprove/... \
    || echo "WARNING: baseline go test reported failures"

# Everything is fetched. From here on the go command must not try to reach a
# module proxy, the checksum database, or download a toolchain.
ENV GOPROXY=off \
    GOSUMDB=off

CMD ["bash"]
