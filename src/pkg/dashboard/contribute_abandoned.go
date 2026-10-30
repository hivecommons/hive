package dashboard

// ContributorAbandonRecord is one PR of a contributor the hive adopted
// (hivecommons/hive#15782). AdoptedIntoPR is the hive's superseding PR, 0
// while the adoption is in place on the contributor's own branch.
type ContributorAbandonRecord struct {
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	AdoptedIntoPR int    `json:"adopted_into_pr"`
	Reason        string `json:"reason"`
	At            string `json:"at"`
}
