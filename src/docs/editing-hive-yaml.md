# Editing `hive.yaml`

Use **Settings** if it has an editable control for your key. If it does not,
change the deployment's YAML. On a hosted hive with no shell or deployment
access, ask the hosting operator to make that change; viewing or exporting the
config does not let you write it back.

## What Settings can change

Search Settings using the full dotted key (for example,
`governor.labels.stage.engine.mode`). The [Settings guide](settings.md) lists
controls by tab, and the [operator reference](operator-reference.md#configuration-blocks)
explains the YAML keys. Common examples:

| Settings tab | Example editable YAML keys |
| --- | --- |
| General | `acmm_level` |
| Labels | `governor.labels.stage.engine.mode` |
| Repos | `project.repos`, `repos.<repo>.parent_autoclose` |
| Budget | `governor.budget.usd` |
| Dashboard | `dashboard.port`, `dashboard.subscription_gauge_header` |

The [Settings parity allowlist](../pkg/dashboard/settings_parity_allowlist.txt)
records per-key exceptions to the dialog coverage check, with comments on why
an exception exists. It is not a permissions list or an exact list of all
file-only keys: the check looks for YAML key references in the dialog, and some
keys have read-only displays or are set through another control. Check for an
editable control on your running version.

**`review.analysis_factcheck.*` is YAML-only.** The review controls in
Settings do not enable it. If you only have the dashboard, send the hosting
operator the [TechDocs YAML example](workflows/techdocs-assessment.md#independent-fact-check-before-a-person-reviews-optional)
with your drafter, reviewer and qualifying label; do not send your whole YAML.

## Find the file that survives restart

| Deployment | First-boot input | Where to edit an existing hive |
| --- | --- | --- |
| [Kubernetes](kubernetes-deployment.md) | `hive-config` ConfigMap, key `hive.yaml`, staged at `/etc/hive/hive.yaml` | `/data/hive.yaml.runtime` on the persistent volume. On newer manifests `/etc/hive/hive.yaml` links there; on older ones it is a copy. |
| [Podman / Quadlet](quickstart-podman.md) | `~/.config/hive/hive.yaml` (rootless) or `/etc/hive/hive.yaml` (rootful), mounted at `/etc/hive/hive.yaml` in the container | `/data/hive.yaml.runtime` in the `hive-data` volume. The seed mount is read-only. |
| [Docker Compose](quickstart-docker-compose.md) | `src/hive.yaml` in the checkout (`./hive.yaml` relative to `src/docker-compose.yaml`), mounted at `/etc/hive/hive.yaml` in the container | `/data/hive.yaml.runtime` in the Compose `hive-data` volume. The entrypoint can copy it over the bind-mounted file at boot. Docker Desktop on macOS follows this same rule. |
| Native `hive` binary on macOS or Linux | `/etc/hive/hive.yaml` by default, or the file specified at launch | The same file. For example, `hive --config ./hive.yaml` uses the file relative to the process's working directory. |

The repository's [macOS LaunchAgent example](../../launchd/com.hive.plist.example)
runs the agent supervisor, not the native `hive` server. It does not set the
server's config path.

`HIVE_CONFIG` sets the binary's default path; an explicit `--config` takes
precedence. The container entrypoint also honors `HIVE_CONFIG` and can redirect
the binary to the persisted file when the seed mount is read-only. For a
running deployment, check `metadata.persisted_path` in the JSON export or the
`boot: config source of truth` log, which also shows `loaded_from` and the
symlink-resolved `boot_source`. An older volume can still boot from
`/data/hive.yaml.bak` if the runtime file is absent. Do not delete either file
to force Hive to pick up a seed edit.

## View YAML or export effective config

On the **hive's** dashboard, sign in as an owner and open the avatar menu:

- **View hive.yaml** (`GET /api/config/download`) shows the raw YAML read
  from `HIVE_CONFIG` or `/etc/hive/hive.yaml`. The viewer lets you copy it;
  the endpoint also serves it as an attachment. It is not the merged config
  and can contain secrets. Do not paste it into an issue or share it publicly.
- **Export effective config (JSON)** (`GET /api/config/export`) shows the
  in-memory `effective` config. `layers` shows the seed, dashboard overlay,
  per-agent overrides and `config.env`, with paths and presence markers.
  `side_files` shows adjacent inputs such as prompt templates, restrictions
  and sidebar layout, not YAML merge layers. `provenance` reports field origins
  from the layer merge; check `effective` to see what the process is actually
  using. The export redacts secret-looking values, but review it before
  sharing. It is neither an import file nor a [disaster recovery backup](backup-restore.md#config-export-is-not-a-backup).

On a new Kubernetes volume the dashboard overlay is merged over the ConfigMap
seed to create the runtime file. After that, the runtime file is the entrypoint's
boot input. The Go loader can still apply selected dashboard-overlay fields and
per-agent overrides, and `config.env` can pin certain values. See
[configuration layering](config-layering.md) for the precedence and exceptions.

## Edit, validate, and apply

1. Plan a maintenance window for manual edits. Back up the files you will
   change, keep the copies private, and coordinate with anyone saving
   Settings: a save from the running hive can overwrite a manual edit.
2. Change only the intended key in the persisted YAML, preserving permissions
   and any environment references. Update the existing YAML block rather than
   adding a duplicate. On Kubernetes, if `/data/hive.yaml.dashboard` already
   supplies an overriding block, update the field in that block too. For
   example, the Go loader can take `review` as a whole from the overlay, so a
   runtime-only `review.analysis_factcheck` edit can be masked even after a
   restart. Do not replace the secret-free overlay with the runtime file.
3. Validate with the deployed version of `hive`, in the same environment
   and with the same config layers available: `hive validate -config /path/to/hive.yaml`
   (substitute the edited YAML path). This loads and validates the config
   without starting another hive. Do not restart on a validation error.
4. Check when the setting takes effect. Settings saves may apply immediately,
   on a later sweep, or only at startup, depending on the field. For example,
   the stage engine reads `governor.labels.stage.engine.mode` on its next
   pass, while the dashboard listens on a new `dashboard.port` only after a
   restart. For a manual edit, Hive watches the **loaded file**, not every
   input in the export. Check for `config reloaded from file` or
   `config reload failed` in the logs. A reload can update live readers but
   cannot reinitialize every boot-time service. If the edited file is not
   the watched file, the setting is startup-only, or live reload is not
   documented for the setting, restart through your deployment manager
   without replacing the data volume. For changes to process environment or
   mounts, reapply the deployment so the process receives them.
5. Verify the effective value in a fresh JSON export (or the setting's
   runtime behavior) and check startup or reload logs. If the value did
   not change, check for an overriding layer before editing again.
