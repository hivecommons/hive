# Contributor registration token expiry and rotation

Registration tokens now have persisted `token_issued_at`, `token_expires_at`,
and `token_last_used_at` fields. Existing credentials receive migration-time
issuance dates on dashboard startup; restarting does not extend their lifetime.
Invite tokens are unchanged.

Configure the dashboard in `hive.yaml` with Go durations (hours, not `d`):

```yaml
contribute:
  token_max_age: 2160h # 90 days; mandatory maximum lifetime
  token_idle_max: 0s  # optional inactivity cutoff; default off
```

`HIVE_CONTRIBUTE_TOKEN_MAX_AGE` and `HIVE_CONTRIBUTE_TOKEN_IDLE_MAX` override these
values. Invalid/negative settings fall back to 90 days / off; zero maximum age
also falls back to 90 days, rather than disabling expiry. Reducing maximum age
applies immediately; increasing it never extends an already-issued token's
stored expiry. The exact expiry instant is rejected, not accepted. Successful
authentication and pong-responsive live sessions count as activity (live-session
activity is persisted about once per minute, or more often for short idle limits). Idle limits shorter than a
heartbeat interval are not recommended.

## Contributors

The Management panel shows token issuance/expiry and warns when an active
contributor's token reaches 80% of maximum age. Contributors can check their own
credential with `GET /api/contribute/token`, using `Authorization: Bearer <token>`.
Expired credentials receive HTTP 401 with recovery instructions. WebSocket
authentication sends the same instructions in `auth_failed`; existing relays
already display that reason. Connected sessions are rechecked on heartbeat,
including expiry, revocation and replacement by a newly issued token.

Recover using GitHub proof of identity (an expired registration token cannot
rotate itself):

```sh
hivectl contribute login           # active saved hive profile
hivectl contribute login my-hive   # named profile
# Equivalent existing command:
hivectl hives reissue my-hive
```

The command saves the new credential and regenerates `contributor.env`. Restart
the relay as instructed. The underlying recovery endpoint remains
`POST /api/contribute/reissue-token` with a GitHub bearer token.

## Operators / incident response

Contribute → Management → Registration tokens lists token age/expiry and offers
confirmation-gated revoke-one and revoke-all actions. These invalidate credentials
and disconnect live relays without changing contributor trust tiers. Recovery
requires each contributor's GitHub-authenticated login. Account-level revocation
(the existing Revoke control) is separate and still prevents self-recovery.

```sh
hivectl --server https://my-hive.example contribute tokens list --output json
hivectl --server https://my-hive.example contribute tokens revoke alice
hivectl --server https://my-hive.example contribute tokens revoke --all --yes
```

API equivalents (hive owner/read-write authentication required):

- `GET /api/contributors` — sanitized profiles including token timestamps and
  `token_rotation_due`.
- `POST /api/contributors/{id}/token/revoke` — revoke one registration token.
- `POST /api/contributors/tokens/revoke-all` — revoke every registration token.

Bulk revocation is deliberately not transactional across files. A persistence or
concurrency error returns 409 with the count already revoked; refresh and retry
to finish. Failed writes are never reported as successful rotations/revocations.
