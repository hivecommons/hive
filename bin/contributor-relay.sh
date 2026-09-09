#!/bin/sh
# Compat wrapper (kubestellar/hive#6429): contributor-relay.sh is a Node.js
# program that used to carry the real source under this name — a misleading
# extension every shell-oriented tool (shellcheck, bash -n, editors, syntax
# highlighters) misfired on. The program moved to contributor-relay.js; this
# wrapper stays because the .sh path is baked into existing deployments
# (container images, install steps, operator muscle memory) that this PR does
# not control. It does nothing but exec the real program with the same argv.
exec node "$(dirname "$0")/contributor-relay.js" "$@"
