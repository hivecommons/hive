package forgestate

import "time"

// QueueObservation and its value types describe forge-owned queues without
// granting admission or merge authority.
type QueueEntry struct {
	ID         string     `json:"id"`
	PR         int        `json:"pr"`
	Head       string     `json:"head"`
	Position   int        `json:"position"`
	State      string     `json:"state"`
	EnqueuedAt *time.Time `json:"enqueued_at,omitempty"`
}

type QueueCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	AppID      int64  `json:"app_id,omitempty"`
}

type QueueMember struct {
	PR         int    `json:"pr"`
	Head       string `json:"head"`
	Entry      string `json:"entry,omitempty"`
	Position   int    `json:"position,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
}

type QueueCandidate struct {
	Ref         string        `json:"ref"`
	SHA         string        `json:"sha"`
	BaseSHA     string        `json:"base_sha,omitempty"`
	Tree        string        `json:"tree,omitempty"`
	Membership  string        `json:"membership"`
	Members     []QueueMember `json:"members"`
	CI          string        `json:"ci"`
	Checks      []QueueCheck  `json:"checks"`
	ChecksKnown bool          `json:"checks_known"`
	Reason      string        `json:"reason,omitempty"`
}

type QueueOutcome struct {
	Kind         string    `json:"kind"`
	PR           int       `json:"pr,omitempty"`
	Head         string    `json:"head,omitempty"`
	CandidateSHA string    `json:"candidate_sha,omitempty"`
	MergeSHA     string    `json:"merge_sha,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	Source       string    `json:"source"`
}

type QueueObservation struct {
	Provider        string           `json:"provider"`
	Repo            string           `json:"repo"`
	Base            string           `json:"base"`
	State           string           `json:"state"`
	Complete        bool             `json:"complete"`
	EntriesComplete bool             `json:"entries_complete"`
	ObservedAt      time.Time        `json:"observed_at"`
	Stale           bool             `json:"stale"`
	FreshForS       int64            `json:"fresh_for_s"`
	Reason          string           `json:"reason,omitempty"`
	Entries         []QueueEntry     `json:"entries"`
	Candidates      []QueueCandidate `json:"candidates"`
	Outcomes        []QueueOutcome   `json:"outcomes"`
}
