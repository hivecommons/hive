package github

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// CIFailureSignal is one failed check a verified webhook delivery reported
// on an open PR's head (hivecommons/hive#15291): which PR, which head, which
// check, where its log is and when the delivery arrived. The governor reacts
// to it within seconds instead of waiting for the next sweep to find the PR
// red.
type CIFailureSignal struct {
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	HeadSHA    string    `json:"head_sha"`
	Check      string    `json:"check"`
	DetailsURL string    `json:"details_url,omitempty"`
	At         time.Time `json:"at"`
}

// CIFailureStatus is the `webhooks.ci_failures` block of /api/status: how
// many distinct (PR, head, check) failure signals are held and the newest.
type CIFailureStatus struct {
	Count int              `json:"count"`
	Last  *CIFailureSignal `json:"last,omitempty"`
}

const (
	// ciFailureRetention is how long a signal is held. A head that is
	// still red a day later is the sweep's (ci-failing.json) business.
	ciFailureRetention = 24 * time.Hour
	// ciFailureMaxSignals bounds the store against a check fan-out storm.
	ciFailureMaxSignals = 512
)

// ciFailureStore is the small in-memory CI failure signal store, keyed by
// repo, PR, head SHA and check so a redelivery or a second failing
// check_suite/check_run for the same check on the same head records once.
// It lives on the webhook tracker and shares its lock and clock.
type ciFailureStore struct {
	signals map[string]CIFailureSignal
	last    *CIFailureSignal
}

func ciFailureKey(repo string, number int, headSHA, check string) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number) + "@" + headSHA + "|" + strings.ToLower(check)
}

func (s *ciFailureStore) pruneLocked(now time.Time) {
	for k, sig := range s.signals {
		if now.Sub(sig.At) >= ciFailureRetention {
			delete(s.signals, k)
		}
	}
	if s.last != nil && now.Sub(s.last.At) >= ciFailureRetention {
		s.last = nil
	}
	if len(s.signals) <= ciFailureMaxSignals {
		return
	}
	keys := make([]string, 0, len(s.signals))
	for k := range s.signals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return s.signals[keys[i]].At.Before(s.signals[keys[j]].At) })
	for _, k := range keys[:len(keys)-ciFailureMaxSignals] {
		delete(s.signals, k)
	}
}

func (s *ciFailureStore) statusLocked(now time.Time) CIFailureStatus {
	s.pruneLocked(now)
	out := CIFailureStatus{Count: len(s.signals)}
	if s.last != nil {
		last := *s.last
		out.Last = &last
	}
	return out
}

// recordCIFailures records one signal per PR for a failed check on headSHA
// and returns the ones not already held.
func (t *webhookTracker) recordCIFailures(repo string, numbers []int, headSHA, check, detailsURL string) []CIFailureSignal {
	if len(numbers) == 0 || headSHA == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := &t.ciFailures
	if s.signals == nil {
		s.signals = map[string]CIFailureSignal{}
	}
	s.pruneLocked(now)
	var out []CIFailureSignal
	for _, n := range numbers {
		key := ciFailureKey(repo, n, headSHA, check)
		if _, seen := s.signals[key]; seen {
			continue
		}
		sig := CIFailureSignal{Repo: repo, Number: n, HeadSHA: headSHA, Check: check, DetailsURL: detailsURL, At: now.UTC()}
		s.signals[key] = sig
		last := sig
		s.last = &last
		out = append(out, sig)
	}
	s.pruneLocked(now)
	return out
}

// CIFailureSignalsSnapshot returns the CI failure signal store's status
// block (also served as /api/status webhooks.ci_failures).
func CIFailureSignalsSnapshot() CIFailureStatus {
	t := sharedWebhookTracker
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ciFailures.statusLocked(t.now())
}

// webhookCIFailure reports whether a delivery is a failed check and names
// the check and its log link: a completed check_run concluding failure or
// timed_out, a check_suite concluding failure or timed_out, or a status in
// state failure or error. Anything else (success, neutral, cancelled,
// pending, in-progress) is not a failure signal.
func webhookCIFailure(event string, p webhookEventPayload) (check, detailsURL string, ok bool) {
	switch event {
	case "check_run":
		if p.CheckRun == nil || p.Action != "completed" || !ciFailedConclusion(p.CheckRun.Conclusion) {
			return "", "", false
		}
		url := strings.TrimSpace(p.CheckRun.DetailsURL)
		if url == "" {
			url = strings.TrimSpace(p.CheckRun.HTMLURL)
		}
		return ciFailureCheckName(p.CheckRun.Name, event), url, true
	case "check_suite":
		if p.CheckSuite == nil || !ciFailedConclusion(p.CheckSuite.Conclusion) {
			return "", "", false
		}
		name := ""
		if p.CheckSuite.App != nil {
			name = p.CheckSuite.App.Name
		}
		return ciFailureCheckName(name, event), "", true
	case "status":
		switch strings.ToLower(strings.TrimSpace(p.State)) {
		case "failure", "error":
			return ciFailureCheckName(p.Context, event), strings.TrimSpace(p.TargetURL), true
		}
	}
	return "", "", false
}

func ciFailedConclusion(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "failure", "timed_out":
		return true
	}
	return false
}

func ciFailureCheckName(name, event string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return event
}

// webhookCIFailurePRs resolves a failed head to open PRs from what the
// delivery and the PR detail cache already hold: the payload's own
// pull_requests list and every cached PR whose head is headSHA. No GitHub
// call is made; a head neither names is left to the sweep, which the
// invalidation's dirty mark already schedules.
func webhookCIFailurePRs(event string, p webhookEventPayload, aliases []string, headSHA string) []int {
	set := map[int]bool{}
	var refs []webhookPRRef
	switch event {
	case "check_run":
		if p.CheckRun != nil {
			refs = p.CheckRun.PullRequests
		}
	case "check_suite":
		if p.CheckSuite != nil {
			refs = p.CheckSuite.PullRequests
		}
	}
	for _, r := range refs {
		if r.Number > 0 {
			set[r.Number] = true
		}
	}
	for _, n := range sharedPRDetailCache.numbersMatching(aliases, func(sha, _, _ string) bool { return sha == headSHA }) {
		if n > 0 {
			set[n] = true
		}
	}
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
