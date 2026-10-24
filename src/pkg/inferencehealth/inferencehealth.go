// Package inferencehealth is the Hive-side name for the inference-gateway
// health store. The store lives in the Crux ring as pkg/crux/inferencehealth
// (stdlib only; the proxy records gateway faults into it and snapshots them
// for the runtime's status surface), and this package re-exports every symbol
// so pkg/hub, pkg/hub/spoke and pkg/dashboard keep their import path: Crux
// owns, Hive aliases (hivecommons/hive#13536).
package inferencehealth

import cruxinferencehealth "github.com/hivecommons/hive/pkg/crux/inferencehealth"

const (
	ClassDNS     = cruxinferencehealth.ClassDNS
	ClassConnect = cruxinferencehealth.ClassConnect
	Class5xx     = cruxinferencehealth.Class5xx
	ClassAuth    = cruxinferencehealth.ClassAuth
	ClassBudget  = cruxinferencehealth.ClassBudget
	ClassOther   = cruxinferencehealth.ClassOther
)

type (
	GatewayStatus = cruxinferencehealth.GatewayStatus
	Store         = cruxinferencehealth.Store
)

var (
	NewStore           = cruxinferencehealth.NewStore
	ClassifyHTTPStatus = cruxinferencehealth.ClassifyHTTPStatus
	ClassifyError      = cruxinferencehealth.ClassifyError
	Reason             = cruxinferencehealth.Reason
	Sort               = cruxinferencehealth.Sort
	MostRecent         = cruxinferencehealth.MostRecent
)
