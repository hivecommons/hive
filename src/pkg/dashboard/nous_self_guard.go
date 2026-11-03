package dashboard

import "strings"

// Hive-self experiments (nous.Experiment, SelfFlagAssignment) are background
// records about Hive itself. The Experiment Lab is repo-only, so these guards
// drop anything carrying a Hive-self marker or shape before it reaches a Lab
// response, even if a producer mistakenly writes one into the Lab state.
var nousSelfStatusKeys = []string{
	"self", "selfExperiments", "self_experiments", "selfFlag", "self_flag",
	"selfFlags", "self_flags", "selfFlagAssignment", "self_flag_assignment",
}

func nousIsSelfMarker(v interface{}) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "self", "hive-self", "hive_self", "hiveself", "background":
		return true
	}
	return false
}

// nousIsHiveSelfEntry reports whether a ledger/experiment row is a Hive-self
// record, by explicit source/scope marker or by the shape of nous.Experiment
// (subject + cohorts) and SelfFlagAssignment (experiment_id + flag + enabled).
func nousIsHiveSelfEntry(entry map[string]interface{}) bool {
	for _, k := range []string{"source", "scope", "origin", "kind", "type"} {
		if nousIsSelfMarker(entry[k]) {
			return true
		}
	}
	for _, k := range nousSelfStatusKeys {
		if v, ok := entry[k]; ok && v != nil && v != false {
			return true
		}
	}
	_, hasSubject := entry["subject"]
	_, hasCohorts := entry["cohorts"]
	if hasSubject && hasCohorts {
		return true
	}
	_, hasID := entry["experiment_id"]
	_, hasFlag := entry["flag"]
	_, hasEnabled := entry["enabled"]
	return hasID && hasFlag && hasEnabled
}

// nousRepoOnlyLedger returns a copy of ledger without Hive-self entries.
func nousRepoOnlyLedger(ledger []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(ledger))
	for _, entry := range ledger {
		if !nousIsHiveSelfEntry(entry) {
			out = append(out, entry)
		}
	}
	return out
}

func nousDropSelfStatus(status map[string]interface{}) {
	for _, k := range nousSelfStatusKeys {
		delete(status, k)
	}
}
