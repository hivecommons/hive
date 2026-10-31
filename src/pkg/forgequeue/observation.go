// Package forgequeue describes observed forge-owned integration queues.
// Observations confer no admission or merge authority.
package forgequeue

import "time"

type Entry struct {
	ID         string     `json:"id"`
	PR         int        `json:"pr"`
	Head       string     `json:"head"`
	Position   int        `json:"position"`
	State      string     `json:"state"`
	EnqueuedAt *time.Time `json:"enqueued_at,omitempty"`
}

type Member struct {
	PR   int    `json:"pr"`
	Head string `json:"head"`
}

type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	AppID      int64  `json:"app_id,omitempty"`
}

type Candidate struct {
	Ref         string   `json:"ref"`
	SHA         string   `json:"sha"`
	BaseSHA     string   `json:"base_sha,omitempty"`
	Tree        string   `json:"tree,omitempty"`
	Membership  string   `json:"membership"`
	Members     []Member `json:"members"`
	CI          string   `json:"ci"`
	Checks      []Check  `json:"checks"`
	ChecksKnown bool     `json:"checks_known"`
	Reason      string   `json:"reason,omitempty"`
}

type Outcome struct {
	Kind         string    `json:"kind"`
	PR           int       `json:"pr,omitempty"`
	Head         string    `json:"head,omitempty"`
	CandidateSHA string    `json:"candidate_sha,omitempty"`
	MergeSHA     string    `json:"merge_sha,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	Source       string    `json:"source"`
}

type Observation struct {
	Provider        string      `json:"provider"`
	Repo            string      `json:"repo"`
	Base            string      `json:"base"`
	State           string      `json:"state"`
	Complete        bool        `json:"complete"`
	EntriesComplete bool        `json:"entries_complete"`
	ObservedAt      time.Time   `json:"observed_at"`
	Stale           bool        `json:"stale"`
	FreshForS       int64       `json:"fresh_for_s"`
	Reason          string      `json:"reason,omitempty"`
	Entries         []Entry     `json:"entries"`
	Candidates      []Candidate `json:"candidates"`
	Outcomes        []Outcome   `json:"outcomes"`
}
