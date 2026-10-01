# Spoke down with DiskPressure — recovery runbook

A spoke whose `/data` volume (or the node disk behind it) fills up is SIGKILLed
by kubelet ~2s after every start (exit 137, `CrashLoopBackOff`) and the
ReplicaSet recreates it forever, with no warning beforehand
([#9868](https://github.com/hivecommons/hive/issues/9868),
[#9869](https://github.com/hivecommons/hive/issues/9869)). This is the single
runbook for that failure mode: where the data lives, what is safe to delete,
how to expand, and what already degrades gracefully before it gets this far.

## 1. Confirm the diagnosis

```bash
kubectl describe node <node> | grep -A2 Conditions   # look for DiskPressure
kubectl get events -n <ns> --field-selector reason=Evicted
kubectl get pvc -n <ns>                              # hive-data near its cap?
```

If the hive pod is up, check the health surface first — it answers "which
disk, how bad, how much room" without a shell on the node:

```bash
curl -s http://<dashboard>/api/health/deep | jq '.checks[] | select(.name=="data_disk")'
```

`data_disk` (added in #9870, `src/pkg/dashboard/health_disk.go`) reports
`warn` at 75%/85% used and `fail` at 95% — a `fail` here means you are at this
runbook *before* the crash loop, which is the point of the check.

## 2. Immediate relief — what is safe to delete

In priority order, all regenerable:

1. **Stale agent-CLI session-state dirs.** `src/pkg/sessionprune` already
   prunes these automatically (every 6h, 7-day retention by default — see
   `cmd/hive/main.go`'s `runSessionPrune`), but a spoke that has been
   crash-looping may not have had a clean tick to run it. Safe to clear by
   hand if still over budget:
   ```bash
   find /data/home/.copilot -maxdepth 1 -type d -mtime +7 -exec rm -rf {} +
   ```
2. **CLI caches** (`~/.cache`, `~/.npm`, language-server caches under agent
   homes) — fully regenerable, never required for correctness.
3. **Old run/session artifacts** outside the pruned session-state tree (dated
   `*-pre-shared-*` staging directories, old log rotations) — anything with a
   dated/UUID name under an agent home that is not the live session.

Do **not** delete anything under a path that does not match the above (hub
state, repo clones in active use, GitHub App credentials).

## 3. If the pod won't stay up long enough to clean from inside it

Run a one-off pod mounting the same PVC:

```bash
kubectl run disk-cleanup --rm -it --restart=Never \
  --image=busybox -n <ns> \
  --overrides='{"spec":{"containers":[{"name":"disk-cleanup","image":"busybox","command":["sh"],"stdin":true,"tty":true,"volumeMounts":[{"name":"data","mountPath":"/data"}]}],"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"hive-data"}}]}}'
```
then delete from `/data` as in step 2, `exit`, and let the Deployment
restart normally.

## 4. Longer-term fix — move off node-local storage

If the underlying class is node-local (`local-path`, `openebs-hostpath`),
deleting files only buys time — the PVC's cap cannot be expanded by editing
it, and a single node's disk is shared with every other local-path PV on it.
See [Manual provisioning → Multi-node clusters: do not put /data on
node-local storage](manual-provisioning.md#multi-node-clusters-do-not-put-data-on-node-local-storage)
for the storage-class table and the scale-down/copy/repoint migration steps
to a network-attached class.

## 5. What already degrades gracefully before this point

- **Health check** (#9870): `data_disk` warns at 75%/85% and fails at 95% via
  `/api/health/deep` and the hub's `failingHealthChecks`, long before kubelet
  eviction.
- **Governor pause** (#9869): `bin/kick-governor.sh` samples `/data` on every
  tick and, once usage reaches the same 95% critical threshold, stops kicking
  every agent (`disk_paused_<agent>` state, logged as `DISK CRITICAL`) so no
  new work schedules more writes onto a volume that is already full. It
  resumes automatically (`DISK RECOVERED`) once usage drops back below the
  threshold — no manual unpause needed after cleanup. This does not touch the
  dashboard/API server, so read paths stay up throughout.
- **Self-janitor** (`src/pkg/sessionprune`): bounded, automatic cleanup of
  aged CLI session-state directories, logged (`session prune complete`) only
  when it actually removes something.

Related: [#2349](https://github.com/hivecommons/hive/issues/2349) (small
disks / OKE DiskPressure, same symptom on a different cloud).
