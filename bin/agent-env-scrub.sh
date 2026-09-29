# agent-env-scrub.sh — shell-boundary credential scrub for agent tool shells.
#
# SECURITY (#4045): agent CLI backends re-export their own live GitHub
# credentials into the shells they spawn for tool calls. Observed live on a
# Copilot-backed fleet: the CLI authenticates from its persistent credential
# store (/data/copilot-user-token) and sets GITHUB_TOKEN in the environment of
# every tool shell — AFTER all of the hive's launch-path scrubbing (#3931) has
# run. A wrapper-denied agent then fell back to
#   curl -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/...
# and succeeded at a repo write, bypassing every gh-wrapper control (allowlist
# #3854, mode/ACMM gates, merge eligibility, authorship routing, provenance).
#
# This file is SOURCED — never executed — at the start of every shell in the
# agent's process tree, via BASH_ENV/ENV exported by agent-launch.sh (tool
# shells, which are non-interactive) and via the /etc/bash.bashrc guard the
# Dockerfile installs (interactive shells). Because the unset runs at CHILD
# shell startup, it neutralizes tokens however they arrived: inherited from the
# session env, or set explicitly in the spawn env by the backend CLI itself —
# the #4045 mechanism, which no amount of parent-side scrubbing can reach.
#
# What this deliberately does NOT break:
#   - The backend CLI process keeps its own auth. The CLI (node/binary) never
#     sources this file; COPILOT_GITHUB_TOKEN / GITHUB_TOKEN stay in ITS env
#     for Copilot API auth and the built-in GitHub MCP server (the sanctioned,
#     hive-mediated write path under github.app_authored_prs).
#   - gh-wrapper.sh still authenticates: it sources this at startup (losing
#     only inherited token env, which it must never trust anyway — audit H3)
#     and then exports GH_TOKEN itself from HIVE_AGENT_TOKEN_CACHE, which is a
#     PATH, not a credential, and is deliberately not scrubbed.
#   - git-credential-hive.sh reads the per-agent cache file directly; hive-merge
#     and hive-open-pr likewise never rely on inherited token env.
#
# Residual (documented): a same-uid agent can still read a backend CLI's
# /proc/<pid>/environ deliberately. Whether that extraction lane is closed
# depends on the spoke's HIVE_PROXY_INJECT_GH_AUTH setting (#1861, shipped on
# v5 as PR #5876):
#   - "true" (opt-in only; injection is never on by default, #9586): the
#     App token a backend CLI holds (GITHUB_TOKEN, read from the agent's token
#     cache) is the inert hive-proxy-injected-<agent> placeholder, and the MITM
#     proxy strips ANY agent-supplied Authorization on GitHub hosts - including
#     a CLI's own login token - and injects the real scoped token itself. A
#     smuggled credential is therefore not spendable against GitHub through
#     the proxy. Closed wherever forced egress keeps agents on the proxy.
#   - unset or "false" (the default on every hive, hosted or not): the backend
#     CLI's env can carry the agent's REAL scoped token and the proxy forwards
#     agent-supplied Authorization as-is. The lane is OPEN there until the
#     spoke opts in.
#
# POSIX sh compatible (dash-safe): no bashisms, and `unset` of an absent
# variable is not an error. Keep the variable list in sync with
# bin/test_agent_env_scrub.sh, which source-asserts every name below.
unset GITHUB_TOKEN
unset GH_TOKEN
unset GH_ENTERPRISE_TOKEN
unset GITHUB_ENTERPRISE_TOKEN
unset COPILOT_GITHUB_TOKEN
unset GITHUB_COPILOT_TOKEN
unset HIVE_GITHUB_TOKEN
