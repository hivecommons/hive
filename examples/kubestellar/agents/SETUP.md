# KubeStellar Multi-Agent Setup Guide

## Prerequisites

- Home server (Proxmox container at 192.168.4.28) accessible on LAN
- `hive` alias available on the server
- `tmux`, `sqlite3`, `gh`, `bd` (beads CLI) installed on the server

---

## Step 0: Tailscale (VPN split tunnel)

Run these steps **while NOT on Cisco VPN** (need LAN access to 192.168.4.28).

### On this laptop (macOS):

Option A — Mac App Store (easiest):
1. Open App Store → search "Tailscale" → Install
2. Open Tailscale from menu bar → Sign in

Option B — Homebrew CLI:
```bash
brew install tailscale
sudo brew services start tailscale
sudo tailscale up
# Opens browser to authenticate — sign in with your account
```

### On the home server (192.168.4.28):

```bash
ssh root@192.168.4.28

# Install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# Start and authenticate
tailscale up

# Note the Tailscale IP
tailscale ip -4
# → 100.x.x.x (use this IP from now on — works through Cisco VPN)
```

### Verify (while ON Cisco VPN):

```bash
# This should work through the VPN now
ssh root@100.x.x.x hostname
```

---

## Step 1: Create agent directories on server

```bash
ssh root@100.x.x.x  # or 192.168.4.28 if not on VPN

# Agent home
mkdir -p ~/.hive-agents/{supervisor,scanner,architect,ci-maintainer,outreach}

# Clone repos for each mutating agent
# Replace ${PROJECT_ORG} and repo names with your project's values from hive-project.yaml
for agent in scanner architect; do
  cd ~/.hive-agents/$agent
  for repo in ${PROJECT_REPOS_LIST}; do
    git clone "https://github.com/${repo}.git"
  done
done

# Reviewer and outreach get read-only clones (primary repo only)
for agent in ci-maintainer outreach; do
  cd ~/.hive-agents/$agent
  git clone "https://github.com/${PROJECT_PRIMARY_REPO}.git"
done
```

## Step 2: Initialize supervisor

```bash
cd ~/.hive-agents/supervisor
git init
# Copy supervisor policy as CLAUDE.md (the filename the CLI reads)
cp /path/to/supervisor.md CLAUDE.md
git add CLAUDE.md && git commit -m "init"

# Symlink state.db
ln -sf ~/.hive-fix-loop/state.db state.db
```

## Step 3: Initialize beads ledger

```bash
mkdir -p ~/agent-ledger && cd ~/agent-ledger
bd init
```

## Step 4: Install scanner (launchd/cron)

```bash
# Copy worker.sh
mkdir -p ~/.hive-fix-loop
cp /path/to/worker.sh ~/.hive-fix-loop/worker.sh
chmod +x ~/.hive-fix-loop/worker.sh

# NOTE: No cron installation needed. EXECUTOR MODE — agents are kicked by
# the governor (systemd timer) and supervisor, not by self-scheduled crons.
# See src/docs/architecture.md for details.
```

## Step 5: Copy policy files + env files

```bash
# Copy policy for each executor into their console dir as CLAUDE.md
cp scanner.md ~/.hive-agents/scanner/console/CLAUDE.md
cp architect.md ~/.hive-agents/architect/console/CLAUDE.md
cp ci-maintainer.md ~/.hive-agents/ci-maintainer/console/CLAUDE.md
cp outreach.md ~/.hive-agents/outreach/console/CLAUDE.md

# Copy env files (adjust paths for Linux — /root/ instead of /Users/andan02/)
# If using systemd:
sudo mkdir -p /etc/hive
for agent in supervisor scanner architect ci-maintainer outreach; do
  sudo cp ${agent}.env /etc/hive/ks-${agent}.env
done
```

## Step 6: Install hive instances

```bash
cd /path/to/hive

for agent in supervisor scanner architect ci-maintainer outreach; do
  sudo ./install.sh --instance ks-${agent}
done
```

## Step 7: Set NTFY_TOPIC

```bash
TOPIC=$(uuidgen)
echo "Your ntfy topic: $TOPIC"
echo "Subscribe to it in the ntfy app on your phone"

# Update all env files
for f in /etc/hive/ks-*.env; do
  sed -i "s/^NTFY_TOPIC=$/NTFY_TOPIC=$TOPIC/" "$f"
done

# Restart all
for agent in supervisor scanner architect ci-maintainer outreach; do
  sudo systemctl restart hive@ks-${agent}
done
```

## Step 8: Verify

```bash
# Check all agents
for s in ks-supervisor ks-scanner ks-architect ks-ci-maintainer ks-outreach; do
  echo "=== $s ==="
  tmux has-session -t "$s" 2>/dev/null && echo "✅ running" || echo "❌ not running"
done

# Attach to supervisor
tmux attach -t ks-supervisor
# (Ctrl+B, D to detach)

# View all side-by-side
tmux new-session -d -s overview \; \
  split-window -h \; \
  split-window -v -t 0 \; \
  split-window -v -t 2 \; \
  send-keys -t 0 'tmux attach -t ks-supervisor' Enter \; \
  send-keys -t 1 'tmux attach -t ks-scanner' Enter \; \
  send-keys -t 2 'tmux attach -t ks-ci-maintainer' Enter \; \
  send-keys -t 3 'tmux attach -t ks-outreach' Enter
tmux attach -t overview
```

---

## Architecture Summary

```
┌─────────────────────────────────────────────────────────────────┐
│  Home Server (Proxmox LXC 192.168.4.28 / Tailscale 100.x.x.x) │
│                                                                 │
│  ┌─── cron (15 min) ───┐     ┌──────────────────────────────┐  │
│  │ worker.sh (scanner)  │────▶│ state.db (SQLite)            │  │
│  └──────────────────────┘     └──────────┬───────────────────┘  │
│                                          │                      │
│  ┌──── systemd ─────────────────────────┐│                      │
│  │                                      ││                      │
│  │  ks-supervisor (Opus, /loop 1m)      ││                      │
│  │  ├── reads state.db + beads ◀────────┘│                      │
│  │  ├── does ALL triage + planning       │                      │
│  │  ├── writes precise work orders       │                      │
│  │  ├── sends ntfy digests               │                      │
│  │  │                                    │                      │
│  │  ├──► ks-scanner (Sonnet, EXECUTOR)    │                      │
│  │  │    bugs, PRs, reviews, hygiene     │                      │
│  │  │                                    │                      │
│  │  ├──► ks-architect (Sonnet, EXECUTOR) │                      │
│  │  │    features, refactoring           │                      │
│  │  │                                    │                      │
│  │  ├──► ks-ci-maintainer (Sonnet, EXECUTOR)  │                      │
│  │  │    post-merge, CI health           │                      │
│  │  │                                    │                      │
│  │  └──► ks-outreach (Sonnet, EXECUTOR)│                      │
│  │       CNCF ecosystem, ADOPTERS        │                      │
│  │                                       │                      │
│  │  ~/agent-ledger/ (beads coordination) │                      │
│  └───────────────────────────────────────┘                      │
│                                                                 │
│  ntfy.sh ──► phone notifications                                │
└─────────────────────────────────────────────────────────────────┘
         ▲
         │ SSH via Tailscale (100.x.x.x)
         │ Works through Cisco AnyConnect
         │
    ┌────┴─────┐
    │  Laptop  │
    │ (VPN on) │
    └──────────┘
```

## Token Economics

| Agent | Model | Usage | Cost |
|-------|-------|-------|------|
| Supervisor | Opus | Full reasoning, 1/min loop | High but justified — single brain |
| Scanner | Sonnet | Mechanical execution | Low — no triage/planning |
| Architect | Sonnet | Mechanical execution | Low — occasional |
| Reviewer | Sonnet | Mechanical execution | Low — read-only work |
| Outreach | Sonnet | Mechanical execution | Low — occasional |

All planning/triage/analysis happens once in Opus. Executors never repeat that work.
