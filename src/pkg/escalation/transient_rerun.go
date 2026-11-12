package escalation

// Transient-triage re-run (projectbluefin/common#1647).
//
// When an agent diagnoses a red check as infrastructure, not the change (a
// runner's apt mirror dropping the connection, a lost runner), the repair is
// a re-run of the failed jobs, not a commit. Before this the fix loop kept
// kicking the owning agent: each kick was a delivered re-dispatch, the agent
// answered each one with the same "infrastructure, not this change" comment,
// and the sixth kick escalated the PR to a person. A lane then pushed an
// empty "retrigger CI" commit, which re-arms review fences and can dismiss
// approvals.
//
// The agent's triage reaches the hive as the `hive-ci-transient` marker
// (github.PullRequest.TransientCITriaged). ObserveRed records the head it
// covers; while a re-run of that head is owed, the fix-kick paths stand down
// (TransientRerunOwed) and the eval cycle re-runs the failed jobs once
// (RecordTransientRerun). The re-run gives the failing checks new run ids, so
// the markers go stale by themselves: a head that is red again afterwards is
// back in the ordinary fix loop, with its usual budget and escalation.

// TransientRerunOwed reports that the PR's red head is transient-triaged and
// the hive has not yet re-run its failed jobs. headSHA "" means the head the
// ledger last saw red (the merge watcher's hook knows only the PR number).
// An escalated PR is a person's, so nothing is owed on it.
func (s *Store) TransientRerunOwed(repo string, number int, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.Escalated || e.ReporterTrustReason != "" {
		return false
	}
	if headSHA == "" {
		headSHA = e.CurRedSHA
	}
	return headSHA != "" && e.TransientTriagedSHA == headSHA && e.TransientRerunSHA != headSHA
}

// RecordTransientRerun marks the head's one transient re-run as spent. The
// caller records it once the attempt is settled: re-run, refused by the
// write-back gate or by GitHub, or nothing left to re-run. An attempt that
// failed for a reason a later pass can clear (an API error) is not recorded.
func (s *Store) RecordTransientRerun(repo string, number int, headSHA string) {
	s.RecordTransientRerunOutcome(repo, number, headSHA, "")
}

// RecordTransientRerunOutcome is RecordTransientRerun that also keeps why the
// spent re-run re-ran nothing (skipped; "" when the failed jobs were re-run),
// so the dependency-bot hand-off can tell a person whether the re-run
// happened (TransientRerunOutcome).
func (s *Store) RecordTransientRerunOutcome(repo string, number int, headSHA, skipped string) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	e := s.entries[key]
	if e == nil {
		e = &Entry{Machinery: MachineryVersion}
		s.entries[key] = e
	}
	e.TransientRerunSHA = headSHA
	e.TransientRerunSkipped = skipped
	e.UpdatedAt = s.now()
	s.saveLocked()
}

// TransientRerunOutcome reports whether the head's one transient re-run is
// spent and, if it is, why it re-ran nothing ("" when the failed jobs were
// re-run).
func (s *Store) TransientRerunOutcome(repo string, number int, headSHA string) (spent bool, skipped string) {
	if headSHA == "" {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.TransientRerunSHA != headSHA {
		return false, ""
	}
	return true, e.TransientRerunSkipped
}
