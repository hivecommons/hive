package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

// taskLease is the server-authoritative record of a task the hub issued to a
// contributor identity (hivecommons/hive C4). It binds the assignment to the
// {profile, task, repo, generation} tuple and an expiry so a reconnecting relay's
// task_progress can only RE-ADOPT a task the hub actually assigned to it, under the
// exact generation it was assigned, and only until the lease expires. It is minted
// by recordLease at assignment and cleared by revokeLease on every release path; a
// resume that does not match an unexpired lease here is rejected outright.
//
// The registry holds one lease PER TASK, keyed by leaseKey(identity, taskID)
// (hivecommons/hive#7774). It was keyed by identity alone when it was written, on
// the premise that an identity holds one task at a time — but the concurrency
// gate in selectTask lets an identity hold max_concurrent tasks across its live
// connections (2 for contributor, 5 for trusted/merger), so a second assignment
// silently evicted the first task's lease. A flap on the connection working the
// first task then found only the second's lease, the resume was rejected, and a
// healthy agent was revoked mid-turn and its issue requeued as failed. Every
// task the hub has issued and not released is now independently re-adoptable.
type taskLease struct {
	identity string
	taskID   string
	repo     string
	number   int
	// key is the canonical, source-aware work-item identity (worksource.Ref.Key —
	// the same spelling WSTaskAssign.identityKey produces). It is carried so the
	// double-assignment guard in selectTask can tell which ITEM a lease holds
	// without re-deriving it from repo/number, which is wrong for external work:
	// Linear and Jira items deliberately carry Number == 0 and put their identity
	// in Key (#4245), so every zero-numbered item in a repo would collide as
	// "repo#0" (#5120).
	key  string
	tier string
	gen  uint64
	// restored marks a lease loadLeases read from disk at startup rather than one
	// recordLease minted in this process (#5681). It is deliberately NOT persisted:
	// it means "issued by the PREVIOUS process, whose holder has not reconnected
	// here yet", which is only ever true for the current boot. It used to gate the
	// double-assignment guard's hold on the item to a post-restart grace window;
	// since #7773 every unexpired lease is a hold (leasedIssueKeys), so this is
	// diagnostic — it says where a lease came from, not what it does.
	restored     bool
	expiresAt    time.Time
	mcpTokenID   string
	mcpTokenHash string
}

// leaseTTL is how long a hub-issued task lease remains re-adoptable after the last
// time the relay proved it was still working (hivecommons/hive C4). It is aligned
// with wsTaskTimeout (the wedged-task backstop) and, since #4260, is measured from
// the SAME event: recordLease stamps it at assignment and renewLease re-stamps it on
// every accepted task_progress, exactly where reclaimExpiredLeases re-stamps
// lastLeaseRenew. The two clocks therefore expire together, which is what makes the
// intended invariant true — a task past its re-adoption window is also one the
// wedged-task backstop has reclaimed, and a task the backstop considers alive is
// still re-adoptable.
//
// #4260: before that renewal existed, expiresAt was stamped once at assignment and
// never moved, so the alignment was only nominal. A perfectly healthy task that had
// been reporting progress for longer than leaseTTL was NEVER reclaimed (correctly —
// it was alive) yet its lease had silently expired, so the first socket drop after
// that point could not be resumed: lookupLease rejected the reconnecting relay,
// the hub sent task_revoke, and the same issue was re-assigned as a new task,
// typing a fresh prompt into a pane whose CLI was still mid-turn.
//
// A resume presented after this window is treated as a stale/forged claim and
// rejected; the relay simply asks for fresh work via "ready".
const leaseTTL = wsTaskTimeout

// leaseKey is the registry key for one task held by one identity (#7774). The
// separator is a control character neither half can contain: identities are
// ContributorID or ContributorID#session (sanitized labels), task ids are
// hub-minted. Both halves are also stored on the lease itself, so nothing ever
// has to parse a key back apart.
func leaseKey(identity, taskID string) string {
	return identity + "\x1f" + taskID
}

// leaseForLocked returns the lease this identity holds for taskID, or nil. It is
// the one place the composite key is looked up, so callers — and tests — never
// spell it themselves. The caller must hold leaseMu.
func (h *ContributeWSHub) leaseForLocked(identity, taskID string) *taskLease {
	if h.leases == nil {
		return nil
	}
	return h.leases[leaseKey(identity, taskID)]
}

// recordLease registers (or replaces) the server-authoritative lease for one
// task issued to an identity (hivecommons/hive C4). It stores the
// exact {task, repo, generation, tier} the hub issued plus an expiry, so a later
// reconnect can be validated against what the server actually handed out — never
// reconstructed from client-supplied fields. Called from selectTask under the new
// assignment's generation.
func (h *ContributeWSHub) recordLease(identity, taskID, repo string, number int, tier string, gen uint64, now time.Time) {
	h.recordLeaseForKey(identity, taskID, repo, number, "", tier, gen, now)
}

// recordLeaseForKey is recordLease plus the assignment's canonical work-item key.
// selectTask calls this form with chosen.ref.Key() so an EXTERNAL item's lease
// carries its real identity; an empty key falls back to the repo#number spelling,
// which is exact for GitHub work and is what the plain recordLease form records.
//
// A new lease never touches the identity's OTHER leases (#7774): an identity
// holding task X on one connection and being assigned task Y on another keeps
// both, and each stays re-adoptable on its own. Re-recording the SAME task
// replaces that task's lease, as before.
func (h *ContributeWSHub) recordLeaseForKey(identity, taskID, repo string, number int, key, tier string, gen uint64, now time.Time) {
	if identity == "" || taskID == "" {
		return
	}
	if key == "" {
		key = worksource.Ref{Repo: repo, Number: number}.Key()
	}
	h.leaseMu.Lock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	h.leases[leaseKey(identity, taskID)] = &taskLease{
		identity:  identity,
		taskID:    taskID,
		repo:      repo,
		number:    number,
		key:       key,
		tier:      tier,
		gen:       gen,
		expiresAt: now.Add(leaseTTL),
	}
	// #5681: a lease the hub issued must outlive the process that issued it.
	h.saveLeasesLocked()
	h.leaseMu.Unlock()
}

// renewLease extends an identity's server-issued lease window when the relay proves
// it is still working the task (kubestellar/hive#4260). It is the lease-registry half
// of the lastLeaseRenew stamp that reclaimExpiredLeases reads: both are driven by the
// same accepted task_progress, so "still alive" and "still re-adoptable" cannot drift
// apart and a long-running task does not lose the ability to survive a reconnect
// simply because it has been working for longer than leaseTTL.
//
// It grants NOTHING a caller did not already have. The lease is only touched when
// this identity holds one for this exact taskID, so a connection can neither renew
// another identity's lease nor extend a lease for a task it does not hold; a revoked
// lease is absent and stays absent. Only expiresAt moves — the {task, repo, number,
// tier, generation} tuple lookupLease matches on is never rewritten, so the C4
// exact-match contract and the #2568 generation fence are untouched.
func (h *ContributeWSHub) renewLease(identity, taskID string, now time.Time) {
	if identity == "" || taskID == "" {
		return
	}
	h.leaseMu.Lock()
	if l := h.leaseForLocked(identity, taskID); l != nil {
		l.expiresAt = now.Add(leaseTTL)
		// #5681: persist the EXTENDED window. Without this a restart would restore
		// the window as it stood at assignment, so a task that had been progressing
		// for longer than leaseTTL — the exact case #4260 fixed in memory — would
		// come back already expired and could not be resumed.
		h.saveLeasesLocked()
	}
	h.leaseMu.Unlock()
}

// revokeLease removes the server-authoritative lease for one task an identity holds,
// on any release path (hivecommons/hive C4): disconnect, ready-abandon,
// task_complete, task_failed, operator requeue, and lease-TTL expiry. Once revoked,
// a reconnecting relay's task_progress for that task no longer matches any lease and
// cannot re-adopt it — closing the window in which a released task could be
// resurrected from client fields. Only that task's entry goes; the identity's other
// leases are untouched (#7774). An empty taskID revokes every lease the identity
// holds — no production path passes one today, but the meaning is kept explicit.
func (h *ContributeWSHub) revokeLease(identity, taskID string) {
	if identity == "" {
		return
	}
	h.leaseMu.Lock()
	revoked := false
	if taskID != "" {
		if _, ok := h.leases[leaseKey(identity, taskID)]; ok {
			delete(h.leases, leaseKey(identity, taskID))
			revoked = true
		}
	} else {
		for k, l := range h.leases {
			if l != nil && l.identity == identity {
				delete(h.leases, k)
				revoked = true
			}
		}
	}
	if revoked {
		// #5681: a revoke that did not reach disk would be undone by the next
		// restart, resurrecting a released task. Persist it with the same urgency
		// as the in-memory delete.
		h.saveLeasesLocked()
	}
	h.leaseMu.Unlock()
}

// lookupLease returns the active, unexpired server-issued lease for an identity that
// EXACTLY matches the resume claim (hivecommons/hive C4): same task_id, same
// canonical repo, same number, and same assignment generation. Any mismatch — no
// lease, wrong task, wrong repo/number, wrong (or zero) generation, or an expired
// lease — returns nil, so a reconnecting relay may only re-adopt the precise task the
// hub assigned it, under the generation it was assigned, and only within the lease
// window. It never reconstructs ownership from the client's own fields.
//
// clientGen == 0 (an unversioned relay) is deliberately NOT honored here: re-adoption
// requires proving possession of the server-issued generation token, which an
// unversioned relay cannot present. Such a relay is asked to re-`ready` for fresh
// work instead of resurrecting a lease it cannot authenticate.
func (h *ContributeWSHub) lookupLease(identity, taskID, repo string, number int, clientGen uint64, now time.Time) *taskLease {
	if identity == "" || taskID == "" || clientGen == 0 {
		return nil
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	// Keyed by {identity, task} (#7774): a lease for a DIFFERENT task the same
	// identity holds is simply not this one, rather than a mismatch that rejects
	// the resume — which is what revoked healthy work whenever an identity held
	// more than one task.
	l := h.leaseForLocked(identity, taskID)
	if l == nil {
		return nil
	}
	if now.After(l.expiresAt) {
		// Expired: drop it so it can never be re-adopted, and treat as no lease.
		delete(h.leases, leaseKey(identity, taskID))
		h.saveLeasesLocked()
		return nil
	}
	if l.gen != clientGen {
		return nil
	}
	if repo != "" && l.repo != repo {
		return nil
	}
	if number != 0 && l.number != number {
		return nil
	}
	return l
}

// persistedLease is the on-disk form of a taskLease (#5681).
//
// It carries the lease and nothing else. There is no credential in it: the scoped
// GitHub token is minted per assignment and delivered separately (#2537), never
// stored here. Restoring a lease therefore grants exactly one thing — the ability
// to RE-ADOPT a task the hub already issued to that identity — and never the
// ability to obtain a fresh credential without passing selectTask's gates.
type persistedLease struct {
	Identity     string    `json:"identity"`
	TaskID       string    `json:"task_id"`
	Repo         string    `json:"repo"`
	Number       int       `json:"number"`
	Key          string    `json:"key,omitempty"`
	Tier         string    `json:"tier"`
	Gen          uint64    `json:"gen"`
	ExpiresAt    time.Time `json:"expires_at"`
	MCPTokenID   string    `json:"mcp_token_id,omitempty"`
	MCPTokenHash string    `json:"mcp_token_hash,omitempty"`
}

func (h *ContributeWSHub) taskLeasesPath() string {
	if h != nil && h.taskLeasesFile != "" {
		return h.taskLeasesFile
	}
	return taskLeasesFile
}

// saveLeasesLocked writes the server-issued lease registry to disk (#5681).
//
// THE CALLER MUST HOLD leaseMu. The snapshot and the write happen under the same
// lock deliberately: if the snapshot were taken under the lock and the rename done
// outside it, two concurrent mutations could land their renames in the opposite
// order and leave the file describing an OLDER registry than the one in memory —
// and the whole point of the file is that it is what the next process boots from.
// The cost is negligible: the file holds one record per held task (bounded by
// maxWSConnections times the tier's max_concurrent) and every mutation site is
// low-frequency —
// assignment, release, and one task_progress per relay per PROGRESS_REPORT_INTERVAL_MS.
//
// Leases already past their expiry are skipped rather than written: a lease that
// can no longer be re-adopted must not be able to come back from disk.
func (h *ContributeWSHub) saveLeasesLocked() {
	if h == nil || !h.persistTaskLedgers {
		return
	}
	now := time.Now()
	records := make([]persistedLease, 0, len(h.leases))
	for _, l := range h.leases {
		if l == nil || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		records = append(records, persistedLease{
			Identity:     l.identity,
			TaskID:       l.taskID,
			Repo:         l.repo,
			Number:       l.number,
			Key:          l.key,
			Tier:         l.tier,
			Gen:          l.gen,
			ExpiresAt:    l.expiresAt,
			MCPTokenID:   l.mcpTokenID,
			MCPTokenHash: l.mcpTokenHash,
		})
	}
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases marshal failed", "error", err)
		return
	}
	path := h.taskLeasesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] task leases directory creation failed", "error", err)
		return
	}
	// Crash-safe persist per the #5625 idiom: a UNIQUE temp name (a fixed name
	// lets a non-cooperating process clobber a commit in flight), fsync of the
	// bytes before the rename (the whole point of this file is that the next
	// process boots from it, so the record must be durable, not just renamed),
	// and an fsync of the directory so the rename itself survives a crash.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases temp creation failed", "error", err)
		return
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	// 0600, unlike the sibling ledgers: this file is the C4 authorization record
	// that lookupLease matches a resume against, so it is owner-only on both sides
	// — nothing else on the host has any business reading which contributor holds
	// which work item, and nothing else has any business writing it. CreateTemp
	// already makes 0600; the explicit chmod pins the invariant rather than
	// inheriting it.
	if err := tmp.Chmod(0o600); err != nil {
		h.logger.Warn("[contribute-ws] task leases chmod failed", "error", err)
		return
	}
	if _, err := tmp.Write(data); err != nil {
		h.logger.Warn("[contribute-ws] task leases write failed", "error", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		h.logger.Warn("[contribute-ws] task leases sync failed", "error", err)
		return
	}
	if err := tmp.Close(); err != nil {
		h.logger.Warn("[contribute-ws] task leases close failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] task leases rename failed", "error", err)
		return
	}
	keep = true
	directory, err := os.Open(dir)
	if err != nil {
		h.logger.Warn("[contribute-ws] task leases directory open failed", "error", err)
		return
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		h.logger.Warn("[contribute-ws] task leases directory sync failed", "error", err)
	}
}

// loadLeases restores the server-issued lease registry at hub startup (#5681).
//
// Leases lived only in process memory. A hub restart — which self-upgrade rolls
// (#5391) make routine rather than rare — erased every record of what the hub had
// assigned, while the relays carried on working: they hold one task at a time and
// re-assert it on reconnect (#4260). With the registry empty, EVERY in-flight
// resume failed lookupLease, was answered "no active lease for this task", and had
// its agent interrupted mid-turn — then was handed the identical issue back seconds
// later. Ownership was never in question; only the record of it.
//
// This does not weaken C4. The restored record is still one the SERVER issued and
// wrote itself; a resume still has to match it exactly on
// {identity, task_id, repo, number, generation} and still has to be inside the
// window. Nothing is reconstructed from client-supplied fields, and a lease whose
// expiry has passed is dropped rather than loaded — so a stale file cannot
// resurrect a task that is no longer re-adoptable.
func (h *ContributeWSHub) loadLeases() {
	if h == nil || !h.persistTaskLedgers {
		return
	}
	data, err := os.ReadFile(h.taskLeasesPath())
	if err != nil {
		return
	}
	var records []persistedLease
	if json.Unmarshal(data, &records) != nil {
		h.logger.Warn("[contribute-ws] task leases file unreadable; starting with an empty registry")
		return
	}
	now := time.Now()
	var maxGen uint64
	restored := 0

	h.leaseMu.Lock()
	if h.leases == nil {
		h.leases = make(map[string]*taskLease)
	}
	for _, rec := range records {
		// gen == 0 could never be matched by lookupLease (it refuses clientGen 0),
		// so such a record is unusable; drop it rather than hold an issue hostage.
		if rec.Identity == "" || rec.TaskID == "" || rec.Gen == 0 {
			continue
		}
		if rec.ExpiresAt.IsZero() || now.After(rec.ExpiresAt) {
			continue
		}
		key := rec.Key
		if key == "" {
			key = worksource.Ref{Repo: rec.Repo, Number: rec.Number}.Key()
		}
		// One record per task (#7774). A file written before that held at most
		// one record per identity and loads unchanged; a file written after may
		// hold several for one identity, each of which must come back.
		h.leases[leaseKey(rec.Identity, rec.TaskID)] = &taskLease{
			identity:     rec.Identity,
			taskID:       rec.TaskID,
			repo:         rec.Repo,
			number:       rec.Number,
			key:          key,
			tier:         rec.Tier,
			gen:          rec.Gen,
			restored:     true,
			expiresAt:    rec.ExpiresAt,
			mcpTokenID:   rec.MCPTokenID,
			mcpTokenHash: rec.MCPTokenHash,
		}
		if rec.Gen > maxGen {
			maxGen = rec.Gen
		}
		restored++
	}
	h.leaseMu.Unlock()

	// #2568: taskGen is an in-memory counter that restarts at zero, so without this
	// a post-restart assignment would mint generations that ALIAS the ones just
	// restored — and the Gate (generationAccepted) would then accept a pre-restart
	// straggler against a brand-new task that happened to draw the same number.
	// Advancing the counter past every restored generation keeps what the hub
	// issues strictly ahead of what it has already issued.
	for {
		cur := h.taskGen.Load()
		if cur >= maxGen || h.taskGen.CompareAndSwap(cur, maxGen) {
			break
		}
	}

	if restored > 0 {
		h.logger.Info("[contribute-ws] restored task leases across restart",
			"count", restored, "max_gen", maxGen)
	}
}

// pruneExpiredLeases drops leases that have aged out of their re-adoption window and
// rewrites the file when anything changed (#5681). lookupLease already drops an
// expired lease it happens to read, but a lease whose relay never comes back is
// never looked up: without this it would sit in the registry — and in the
// double-assignment guard below — until the process ended. Called from cleanupLoop
// alongside the other stale-state reaping. Returns how many were dropped.
func (h *ContributeWSHub) pruneExpiredLeases(now time.Time) int {
	dropped := 0
	h.leaseMu.Lock()
	for k, l := range h.leases {
		if l == nil || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			delete(h.leases, k)
			dropped++
		}
	}
	if dropped > 0 {
		h.saveLeasesLocked()
	}
	h.leaseMu.Unlock()
	return dropped
}

// leasedIssueKeys returns the canonical work-item keys that an unexpired lease is
// holding for some identity OTHER than exceptIdentity. selectTask adds them to its
// in-flight exclusions, so an item that is still RE-ADOPTABLE is never OFFERABLE
// (hivecommons/hive#7773).
//
// Those two windows used to be allowed to overlap. A dropped socket keeps its lease
// so the relay can resume (#4260), and the item was left merely cooling down under
// #2356's release hedge — ten minutes — while the lease stayed re-adoptable for
// leaseTTL, thirty. Nothing covered the gap: between ten and thirty minutes after a
// disconnected relay's last progress report the item was out of the live-connection
// scan, out of cooldown, and still resumable. A second contributor asking for work
// in that window was offered it; when the first relay came back — a laptop waking,
// a VPN reconnecting — lookupLease matched, resumeTaskToken minted it a fresh
// credential, and two contributors held the same issue with valid tokens. The
// design note for #5322 was right that the hedge "comfortably outlasts the
// reconnect backoff"; it did not outlast a medium-length outage.
//
// A lease that can still be resumed IS a hold, and is treated as one for exactly as
// long as it can be resumed: the moment it is released (task_complete, task_failed,
// ready-abandon, operator requeue, the wedged-task backstop) revokeLease removes
// it, and the moment it expires pruneExpiredLeases drops it. The cost #5681
// weighed — a park for the length of the lease on every disconnect — is real for a
// relay that never comes back, and it is the price of the invariant: the same
// item cannot be offerable to one contributor and resumable by another. #5681's
// two-minute post-restart grace was this rule applied to restored leases only; it
// is now the rule for every lease, so the restored flag no longer gates anything.
//
// A lease belonging to the REQUESTER is deliberately never an exclusion: asking for
// work is itself the statement that it is not holding that task any more. An
// identity that runs several connections and loses one mid-task can therefore be
// re-offered that task on its other connection while the first could still resume
// it — a duplicate within one account, in a configuration the docs discourage —
// which is narrower than the cross-contributor duplicate this closes.
func (h *ContributeWSHub) leasedIssueKeys(exceptIdentity string, now time.Time) map[string]bool {
	keys := make(map[string]bool)
	if h == nil {
		return keys
	}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, l := range h.leases {
		if l == nil || l.identity == exceptIdentity {
			continue
		}
		if l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		if l.key != "" {
			keys[l.key] = true
		}
	}
	return keys
}

// taskLeasesFile is the durable home of the server-issued task-lease registry
// (#5681). It sits beside the other contributor ledgers, but is written 0600: it is
// the C4 authorization record a resume is matched against, not a report.
var taskLeasesFile = "/data/contributors/task-leases.json"

// reclaimExpiredLeases is the hub-owned LEASE-TTL backstop (kubestellar/hive#2568,
// option 4). A connection that is still HELD by its socket (heartbeat alive) but has
// not renewed its task lease within wsTaskTimeout is presumed wedged — connected but
// no longer progressing — and its task is auto-released. It reuses EXACTLY the manual
// requeue machinery: it books the same short failure cooldown (so the released issue
// is not instantly re-admissible and can't recreate the #2492 dup-assign race), bumps
// the assignment generation (the Gate — so the wedged worker, if it later wakes, is
// fenced), and pushes task_revoke with an auto-expiry reason so a still-listening
// relay stops cleanly. It is deliberately CONSERVATIVE: a task that keeps reporting
// task_progress renews lastLeaseRenew every report and is therefore NEVER reclaimed,
// so "working slowly but alive" is not confused with "wedged". `now` is injected so
// tests can drive expiry deterministically.
func (h *ContributeWSHub) reclaimExpiredLeases(now time.Time) int {
	type expiredTarget struct {
		conn *ContributorConnection
		task WSTaskAssign
	}
	var targets []expiredTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		// Only a connection actively holding a task with a started lease clock can
		// expire; a zero lastLeaseRenew means no active lease (idle or just released).
		expired := c.currentTask != nil && !c.lastLeaseRenew.IsZero() &&
			now.Sub(c.lastLeaseRenew) > wsTaskTimeout
		if expired {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			// #2675: clear credential state so a stale pendingToken cannot leak to the
			// now-idle connection (mirrors RequeueContributorTask cleanup).
			c.pendingToken = ""
			c.credentialDelivered = false
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, expiredTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()

	for _, tgt := range targets {
		// C4: the lease-TTL backstop released this task — revoke its server-issued
		// lease so the wedged worker cannot re-adopt it via a later task_progress.
		h.revokeLease(identityOf(tgt.conn), tgt.task.TaskID)
		if tgt.task.Number > 0 {
			h.recordTaskFailureForTask(&tgt.task, false)
		}
		username := ""
		if tgt.conn.profile != nil {
			username = tgt.conn.profile.GitHubUsername
		}
		h.logger.Warn("[contribute-ws] task lease expired, auto-released",
			"username", username,
			"task", tgt.task.TaskID,
			"repo", tgt.task.Repo,
			"number", tgt.task.Number,
			"lease_ttl", wsTaskTimeout.String(),
		)
		h.recordTaskDecision(username, decisionLeaseExpired, &tgt.task,
			"lease went unrenewed for "+wsTaskTimeout.String()+"; task auto-released and revoked")
		h.addActivity(username, "lease expired: auto-released", tgt.conn.role, tgt.conn.cliBackend, tgt.conn.model, tgt.conn.reasoningEffort, tgt.task.TaskID)
		if tgt.conn.ws != nil {
			_ = tgt.conn.send(WSMessage{
				Type:   "task_revoke",
				Seq:    h.nextSeq(),
				TaskID: tgt.task.TaskID,
				Reason: leaseExpiredReason,
			})
		}
	}
	return len(targets)
}
