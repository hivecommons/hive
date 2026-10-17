package spoke

import "os"

// Consent is local, explicit, and default off. The hub cannot opt a hive in.
// This background-only opt-in is not a user's Experiment Lab/governor setting.
func selfExperimentsOptedIn() bool {
	return os.Getenv("HIVE_SELF_EXPERIMENTS_OPT_IN") == "true"
}
