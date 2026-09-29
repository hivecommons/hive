- Hosted spokes can now be provisioned with the isolated GitHub credential sidecar ([#9586](https://github.com/hivecommons/hive/issues/9586), phase 2), behind a new hub switch `HIVE_HOSTED_CRED_SIDECAR` that is **off by default, including for new spokes**. With `HIVE_HOSTED_CRED_SIDECAR=true` on the hub, each newly provisioned spoke that can run it gets the following. A spoke that cannot run it is skipped with a logged reason: the OpenShift SCC lane, PAT spokes, spokes without an inline App key or without injection, and placeholder App or installation IDs.
  - A `cred-sidecar` container running `hive credsidecar` as UID 1001, with all capabilities dropped, a read-only root filesystem, and only the App key and HMAC key mounted.
  - Credential-sidecar mode on its `hive` container.
  - A per-hive HMAC key in `hive-secrets`.
  - A `hive-cred-sidecar` NetworkPolicy admitting traffic only on the terminal and dashboard ports.

  With the switch off, the rendered manifest is byte-identical to before. Existing spokes are never changed, and enabling the sidecar for any spoke is an operator decision.
