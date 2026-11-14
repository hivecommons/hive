# Register a self-hosted hive on a hub

This page is for people who run their own hive and want it listed on a hub such as hive.hivecommons.dev. It mirrors the **Self-host** card on the hub's `/get-started` page. Operators who run a hub and want to enable or audit this flow should read [hub-deployment.md](hub-deployment.md#self-service-registration) instead.

A self-hosted hive is complete on its own. Listing it on a hub is optional and gives you a public entry in Active Hives, an online badge, and a place in the fleet views. The hub lists your hive as directory metadata only. It never sends your hive credentials, upgrades or management actions, and you never receive the hub's master secret.

## Before you start

- A running or ready-to-run hive. See [Docker Compose](quickstart-docker-compose.md), [Podman](quickstart-podman.md) or [Kubernetes](kubernetes-deployment.md).
- A GitHub account with **admin** permission on every repo the hive will work on. The hub checks this with your own GitHub token at registration time. `maintain` or `write` is not enough.
- A hub whose operator has enabled self-service registration. If the **Register your own hive** button sends you to the self-hosting guide instead of opening a form, the hub has it off; ask that hub's operator.

## Steps

1. Sign in to the hub with GitHub and open **My Hives**.
2. Click **Register your own hive**.
3. Fill in a short name, the GitHub org, the repos (as `owner/name`), and the `https` URL of your hive's dashboard. Your hive ID becomes `byo-<your login>--<name>`, with two hyphens between login and name.
4. Submit. The hub checks admin permission on each repo. One failing repo refuses the whole request and stores nothing.
5. **Copy the key now.** The dialog shows `HIVE_HEARTBEAT_KEY` exactly once and the hub keeps only a hash. Closing the dialog is final. If you miss it, rotate the key (below) rather than registering again.
6. Start the spoke with one of the snippets below, substituting the values from the dialog.
7. After the first accepted heartbeat the hive shows as online in **Self-hosted hives** and appears in Active Hives with the **self-hosted** type.

## Snippets

The dialog gives you these three, already filled in. Each sets the same three variables: `HIVE_HUB_URL`, `HIVE_ID` and `HIVE_HEARTBEAT_KEY`. Do not set `HIVE_HUB_SECRET`; a self-service hive never has one.

Docker Compose:

```yaml
services:
  hive:
    image: ghcr.io/hivecommons/hive:stable
    environment:
      HIVE_HUB_URL: "https://hive.example.com"
      HIVE_ID: "byo-yourlogin--yourname"
      HIVE_HEARTBEAT_KEY: "<the key shown once>"
```

Podman:

```sh
podman run -d --name hive \
  -e HIVE_HUB_URL="https://hive.example.com" \
  -e HIVE_ID="byo-yourlogin--yourname" \
  -e HIVE_HEARTBEAT_KEY="<the key shown once>" \
  ghcr.io/hivecommons/hive:stable
```

Kubernetes (mount or `envFrom` this Secret into the spoke Deployment):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: hive-hub-credentials
type: Opaque
stringData:
  HIVE_HUB_URL: "https://hive.example.com"
  HIVE_ID: "byo-yourlogin--yourname"
  HIVE_HEARTBEAT_KEY: "<the key shown once>"
```

The snippets show only the hub-facing variables. Your hive still needs its usual configuration (GitHub token or App, model backend, storage) from the quickstart you followed.

## Rotate the key

Open **Self-hosted hives** and click **Rotate key** on the entry. The hub mints a new key and shows it once, the same way as at registration. The previous key keeps working for ten minutes so a running spoke can be updated without a gap; after that only the new key is accepted. Rotate if the key was lost, pasted somewhere it should not have been, or if you suspect it leaked.

## Remove the hive

Click **Remove** on the entry. The key stops working at once and the listing disappears. Your hive keeps running; only the hub entry is gone. A hub admin can also remove any self-service hive.

## Expiry

A registered hive that sends no accepted heartbeat for 24 hours (the hub's default; the dialog shows the actual window) is removed automatically, key and listing together. This also applies to a registration that never heartbeated. If your hive was down for longer than that, register it again and start it with the new key.

## Limits and what the key does

- One person may hold a limited number of self-service hives at once; the hub's default is 5. Remove one to register another.
- The key authenticates heartbeats for your one hive ID and nothing else. It cannot act as another hive, as a hosted hive, or as the hub.
- The hub never gives a self-service hive GitHub App credentials, a gateway, a cluster ID, authorized-user lists, upgrade or branch-switch instructions, restarts or fleet-breaker actions. Display-only data such as hub banners still arrives.
- Every registration, refusal, rotation, removal and expiry is written to the hub's audit log without the key.

## Troubleshooting

- **The button goes to the self-hosting guide instead of a form.** The hub has self-service registration off. Ask the operator to set `HIVE_HUB_SELFSERVICE_REGISTRATION`.
- **403 naming a repo.** You do not have admin permission on that repo under the GitHub account you signed in with. Fix the permission or remove the repo from the list.
- **429.** You are at the per-user cap. Remove an entry you no longer use.
- **The hive never shows as online.** Check that all three variables are set on the spoke and that `HIVE_ID` matches the registered ID exactly. The spoke logs `hub heartbeat rejected status=401` when the key does not match; rotate and paste the new key.
- **The entry disappeared.** It expired after the hub's heartbeat window. Register again.
