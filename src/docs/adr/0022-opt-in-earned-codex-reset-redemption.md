# ADR-0022: Opt-in redemption of earned Codex resets

Status: Accepted

## Context

[#6833](https://github.com/hivecommons/hive/issues/6833) and
[#6964](https://github.com/hivecommons/hive/issues/6964) established that no
code path may spend credits or mutate billing. `TestNoCodexSpendOrBillingMutation`
enforced that by banning `rateLimitResetCredit`, `/consume` and `purchaseCredits`
in all production Go. Codex accounts can now bank *earned* rate-limit resets
(read-only count published as `reset_credits_available`, #10596). Redeeming one
is the owner using their own resource, not spending money. The maintainer ruled
on #10595 (option A, narrow exception, local consent only), revising the
no-consume rule for that case only.

## Decision

Hive may call `account/rateLimitResetCredit/consume` for already-earned resets
only, under all of these conditions:

- **Local opt-in.** `HIVE_CODEX_AUTO_USE_BANKED_RESET=1` or the per-pool config
  key is set by the contributor on their own machine. Hub prompts, remote
  assignments and any hub-delivered configuration can never set it. Default off.
- **Exhaustion only.** Redemption happens only when a fresh reading shows the
  applicable window exhausted and at least one reset is available.
- **Idempotent.** Each logical attempt uses a persisted idempotency key reused
  across retries and restarts.
- **Re-read before resume.** Quota is re-read after redemption; work resumes
  only when the fresh reading clears the guard.
- **Audit-logged.** Every redemption records the pool, the reset consumed and
  the remaining count. Credentials are never logged.

`purchaseCredits`, plan upgrades and paid overage remain banned everywhere.

The guard test becomes an allow-list: `rateLimitResetCredit` and `/consume` may
appear in production Go only in `src/pkg/rotation/codex_reset_redeem.go`, the
single consent-gated controller delivered by #10598. `purchaseCredits` stays
banned in every production file, including that one.

## Consequences

Contributors who opt in can recover automatically from weekly exhaustion using
resets they already earned, while the "hive never spends your money" promise
holds: nothing is redeemed without local consent and nothing is ever purchased.
Any new `consume` call site requires a new ADR.
