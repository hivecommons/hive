# feedback: recovery is ledger-based, not credential-based

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).
> In a live deployment these `feedback_*.md` files sit in the agent's memory
> directory; they are kept beside the policy here so the example is complete.

## The rule

When the scanner's CLI session hits a usage limit or loses its login, the
scanner must **never** attempt to re-authenticate itself — no scripted
`/login`, no token minting, no copying credentials from another session, no
prompting for or storing OAuth material. The **operator logs in manually**,
once, and that is the only credential path.

Recovery of in-flight *work* is a separate concern and is handled entirely
through the beads ledger:

1. Every dispatched fix has a bead. The bead — not the session — is the record
   that the work exists.
2. When a session dies mid-work (usage limit, crash, login loss), the bead
   goes stale: `in_progress`, `updated_at` old, no linked PR.
3. The first iteration **after** the operator logs back in runs the stale
   sweep (see "On every pre-flight" in scanner.md): stuck beads are reset to
   `open` with `sweep_reason=stale_no_pr`, and the normal dispatch loop
   re-dispatches them with fresh agents.

## Why

- **Credentials are the operator's.** An agent that can restore its own login
  can also exfiltrate or misuse it. Keeping the human in that loop is the
  security boundary, and it is deliberate — do not "improve" it away.
- **Sessions are disposable; the ledger is not.** Any recovery scheme keyed on
  keeping a session alive fails exactly when it is needed (the session is what
  died). Recovery keyed on beads survives any number of session losses.
- **No double work.** The sweep verifies against GitHub before resetting: if a
  PR already references the tracked issue, the bead is left alone. A
  credential-based "resume where I was" has no such idempotence check.

## What this looks like in practice

Usage limit hits at 14:02. Operator sees the login prompt at 15:30 and logs
in. The 15:45 iteration's pre-flight finds three beads untouched since 14:02
with no linked PRs, resets them, and the same iteration re-dispatches all
three. Nothing was lost; nothing needed the old session back.
