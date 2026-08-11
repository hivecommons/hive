# Troubleshooting

> **Legacy v1/systemd documentation.** This page troubleshoots the original
> supervisor/tmux/systemd deployment (`bin/supervisor.sh`, `systemctl`, and
> `/etc/hive/agent.env`). For current v2 Docker/Go operations, use
> [v2 troubleshooting](../v2/docs/troubleshooting.md) and the
> [`v2/docs/README.md`](../v2/docs/README.md) index.


## The `/loop` prompt never fires — the agent just sits at the prompt

Cause: the supervisor's `AGENT_READY_MARKER` doesn't appear in the agent's TUI, so the supervisor gives up before sending `AGENT_LOOP_PROMPT`.

Fix: attach to the session, look at the footer the agent shows when it's accepting input (for Claude Code v2.x it's "bypass permissions on"), and put that string in `AGENT_READY_MARKER` in `/etc/hive/agent.env`. Then:

```sh
sudo systemctl stop hive
sudo -u "$AGENT_USER" tmux kill-session -t "$AGENT_SESSION_NAME"
sudo systemctl start hive
```

## A permission prompt blocks every `/loop` iteration

Cause: the agent is hitting an interactive prompt that needs a human to dismiss. For Claude Code writing to `memory/` triggers a "sensitive file" prompt even under `--dangerously-skip-permissions`.

Fix: find the unique text of the "accept" option (e.g. `Yes, and always allow access to`) and put it in `AGENT_AUTO_APPROVE_PHRASE`. The supervisor will auto-send `Down Enter` the next time that phrase appears in the pane.

If your agent needs multiple different approvals, extend `approve_prompt_if_present()` in `bin/supervisor.sh` to loop over a list of phrases.

## `systemctl restart hive` didn't pick up my new `AGENT_LOOP_PROMPT`

This is a footgun and it's worth repeating: **plain `systemctl restart` doesn't cause a session respawn.** tmux sessions survive supervisor exit, so when the new supervisor starts and sees a "healthy" old session with an "alive" agent, it does nothing. The old session is still running the old prompt.

For a full reset you need:

```sh
sudo systemctl stop hive
sudo -u "$AGENT_USER" tmux kill-session -t "$AGENT_SESSION_NAME"
sudo systemctl start hive
```

Now the new supervisor finds no session, creates one, and sends the current `AGENT_LOOP_PROMPT`.

## ntfy push never arrives

Run in order:

```sh
# 1. Manual test — does ntfy work at all?
curl -d "test" "$NTFY_SERVER/$NTFY_TOPIC"

# 2. Is the healthcheck env correct?
systemctl cat hive-healthcheck.service
# Look for EnvironmentFile=/etc/hive/agent.env

# 3. Run the healthcheck by hand to see any errors:
sudo -u "$AGENT_USER" env $(cat /etc/hive/agent.env | xargs) /usr/local/bin/agent-healthcheck.sh

# 4. Is the log actually stale?
stat "$AGENT_LOG_FILE"
```

If step 1 works but the healthcheck doesn't push, the most common cause is `NTFY_TOPIC` being blank or misspelled in the env file.

## Supervisor keeps respawning even though the agent looks healthy

Check the `agent_alive()` heuristic in `bin/supervisor.sh`. It considers the agent dead if no non-shell process is running under the pane PID. If your launch command has an extra shell wrapper, the supervisor may mistake the wrapper for "dead." Remove the wrapper, or edit the heuristic.

## Agent writes the heartbeat but scans are obviously broken

The healthcheck only knows about file mtime. If the agent's iteration code is silently noop-ing but still remembers to append to the log, the healthcheck is happy. Two defensive patterns:

1. **Put counts in the log.** If `Issues triaged: 0` for 8 firings in a row, something's wrong. You'll see it in `tail -f $AGENT_LOG_FILE`.
2. **Add your own smoke-check** as a second systemd timer that hits an external endpoint (GitHub API, your app) and cross-checks the agent's reported state.

## Where do I see what the agent is doing right now?

```sh
sudo -u "$AGENT_USER" tmux attach -t "$AGENT_SESSION_NAME"
# Ctrl+B, D to detach — session keeps running.

# Or just peek without attaching:
sudo -u "$AGENT_USER" tmux capture-pane -t "$AGENT_SESSION_NAME" -p -S -50
```

## Switching Claude Code models via tmux

The `/model` slash command inside Claude Code has a picker with only three choices: **Default** (Opus 4.7), **Sonnet** (Sonnet 4.6), and **Haiku** (Haiku 4.5). There is no picker entry for Opus 4.6.

To select a model not shown in the picker, pass the **full model slug** to `/model`:

```
/model claude-opus-4-6
```

### Known model slugs

| Alias in picker | Full slug | Notes |
|----------------|-----------|-------|
| Default (Opus 4.7) | `claude-opus-4-7` | Latest, highest capability |
| — | `claude-opus-4-6` | Not in picker — must use full slug |
| Sonnet | `claude-sonnet-4-6` | Best for everyday tasks |
| Haiku | `claude-haiku-4-5` | Fastest |

### What does NOT work

| Command | Result |
|---------|--------|
| `/model opus 4` | `Model 'opus 4' not found` |
| `/model claude-opus-4` | `Model 'claude-opus-4' not found` |
| `/model --model claude-opus-4-6` | `Model '--model claude-opus-4-6' not found` (treats flag as model name) |
| `/fast` | Toggles "Fast mode" billing tier for Opus 4.6 — does NOT switch the model itself |

### Sending via tmux (supervisor)

When switching an agent's model via `tmux send-keys`, the full sequence is:

```sh
tmux send-keys -t <session> '/model claude-opus-4-6'
tmux send-keys -t <session> Enter
```

Confirm by reading the pane — the footer should show `Opus 4.6`:

```sh
tmux capture-pane -t <session> -p | grep -i opus
```

### Confirming with `claude -p`

To verify a slug works before sending it to an agent session:

```sh
claude -p --model claude-opus-4-6 "say hello"
```

If the slug is valid, you'll get a response. If not, you'll get an error.

## CI check rules for merging PRs

All agents that merge PRs (scanner, reviewer, supervisor) MUST follow these rules:

### Non-negotiable

1. **ALL required CI checks must show SUCCESS before merging.** Run `gh pr checks <number> --required` and verify every line says `pass`. If any check is `pending` or `fail`, WAIT.
2. **The ONLY exception is `tide`.** Prow's merge queue (`tide`) stays pending forever without `lgtm`/`approved` labels. If `tide` is the only non-passing check, treat CI as green and merge.
3. **Never batch-merge.** After merging one PR, wait for the next PR's CI to re-run against the updated base. PRs green before a merge may conflict after.
4. **Merge sequence:** merge one → wait for next PR's CI to re-trigger and pass → merge next.

### Why `tide` is safe to ignore

`tide` is Prow's automatic merge controller. It requires `lgtm` + `approved` labels to queue a PR. Since AI-authored PRs use a normal `--squash` merge (Prow is not involved, and `--admin` is never used), `tide` will never transition to `success`. It is not a CI quality gate — it is a merge queue status indicator.

### Checking CI status

```sh
# Check all checks (filter out tide)
gh pr checks <number> --required 2>&1 | grep -v tide

# Quick pass/fail summary
gh pr checks <number> --required 2>&1 | grep -v tide | grep -E "fail|pending"
# If no output → safe to merge
```

## Clean reinstall

```sh
sudo ./uninstall.sh
# (optionally remove /etc/hive/agent.env and the heartbeat log dir)
sudo ./install.sh
```
