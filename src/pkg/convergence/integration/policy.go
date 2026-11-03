package integration

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func groupChangeKey(repo, authority, head string) string {
	return strings.ToLower(repo) + "#" + authority + "#" + head
}

func appendCandidate(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

func (s *Store) policyActive(subject Subject) bool {
	for _, binding := range s.policies.Active {
		if strings.EqualFold(binding.Repo, subject.Repo) && binding.Base == subject.Base && binding.Fingerprint == subject.Policy {
			return true
		}
	}
	return false
}

func (s *Store) SetPolicies(policies []config.IntegrationPolicy, now time.Time) error {
	set := PolicySet{Active: make([]PolicyBinding, 0, len(policies))}
	for _, policy := range policies {
		set.Active = append(set.Active, PolicyBinding{Repo: strings.ToLower(policy.Repo), Base: policy.Base, Fingerprint: policy.Fingerprint()})
	}
	sort.Slice(set.Active, func(i, j int) bool { return set.Active[i].Fingerprint < set.Active[j].Fingerprint })
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.writerLock()
	if err != nil {
		return err
	}
	defer unlock()
	if s.policies != nil && eventDigest(Event{PolicySet: s.policies}) == eventDigest(Event{PolicySet: &set}) {
		return nil
	}
	return s.appendLocked(Event{Version: SchemaVersion, ID: fmt.Sprintf("policies/%d", now.UnixNano()), At: now, PolicySet: &set})
}
