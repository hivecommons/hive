package github

// NotificationWriteSite is the classification of one producer of
// notification-capable GitHub writes.
type NotificationWriteSite struct {
	Class NotificationClass
	// Passthrough marks a wrapper that forwards its caller's write; the
	// shadow attribution looks past it to the caller and falls back to it
	// only when the caller is not inventoried.
	Passthrough bool
}

// notificationWriteSites is the inventory of every function under src/pkg
// and src/cmd that calls a notification-capable write primitive
// (CreateIssueComment, CreateIssueCommentBudgeted, PostNoteOrFold,
// PostClaimNote, PullRequests.CreateReview, RequestReviewers) or records a
// suppressed note. Keys are "<package>.<function>" or
// "<package>.(*Recv).<method>", the form runtime reports with closures
// stripped. TestNotificationWriteSitesInventory fails when a new call site
// is added without an entry here, so every write carries a class tag. The
// same table is documented in src/docs/notifications.md.
var notificationWriteSites = map[string]NotificationWriteSite{
	// Passthrough wrappers.
	"github.(*Client).CreateIssueCommentBudgeted": {Class: NotificationClassStatus, Passthrough: true},
	"github.(*Client).PostNoteOrFold":             {Class: NotificationClassStatus, Passthrough: true},
	"github.(*Client).RequestReviewers":           {Class: NotificationClassRequiredReview, Passthrough: true},
	"main.(*repoForgeRouter).CreateIssueComment":  {Class: NotificationClassAttention, Passthrough: true},
	"main.liveTriageCommenter.CreateIssueComment": {Class: NotificationClassAttention, Passthrough: true},
	"github.(*Client).UpsertIssueMarkerComment":   {Class: NotificationClassStatus, Passthrough: true},

	// status: bookkeeping readable on demand.
	"dashboard.(*ContributeWSHub).markAlreadyDoneIssue":       {Class: NotificationClassStatus},
	"dashboard.(*ContributeWSHub).releaseGateParkLabel":       {Class: NotificationClassStatus},
	"dashboard.(*ContributeWSHub).releaseToolingBlockedLabel": {Class: NotificationClassStatus},
	"dashboard.(*Server).armRebaseThenMerge":                  {Class: NotificationClassStatus},
	"dashboard.(*Server).executeDelegatedDecision":            {Class: NotificationClassStatus},
	"dashboard.(*Server).handlePullRequestMergeDecision":      {Class: NotificationClassStatus},
	"dashboard.(*Server).handleRepoItemAdoptNow":              {Class: NotificationClassStatus},
	"dashboard.(*Server).handleRepoItemBlockerClosePartial":   {Class: NotificationClassStatus},
	"dashboard.(*Server).handleRepoItemDecision":              {Class: NotificationClassStatus},
	"dashboard.(*Server).handleReviewQueueSupersededClose":    {Class: NotificationClassStatus},
	"dashboard.(*Server).mirrorPlanWith":                      {Class: NotificationClassStatus},
	"github.(*Client).CloseIssueForConfiguredLinePR":          {Class: NotificationClassStatus},
	"github.(*Client).PostFleetReportCleared":                 {Class: NotificationClassStatus},
	"github.(*Client).PostFleetReportRecovery":                {Class: NotificationClassStatus},
	"github.(*Client).PostFleetReportRollup":                  {Class: NotificationClassStatus},
	"github.(*Client).SweepLegacyFleetReportIssues":           {Class: NotificationClassStatus},
	"github.(*Client).closeDecomposedParent":                  {Class: NotificationClassStatus},
	"github.(*Client).closeIssueForMergedPRClaimWithIssue":    {Class: NotificationClassStatus},
	"github.(*Client).closeSupersededPRDecision":              {Class: NotificationClassStatus},
	"github.(*Client).fileReviewBacklog":                      {Class: NotificationClassStatus},
	"github.(*Client).joinFleetReportIssue":                   {Class: NotificationClassStatus},
	"github.(*Client).markInProgressStarted":                  {Class: NotificationClassStatus},
	"github.(*Client).postUnblockedNote":                      {Class: NotificationClassStatus},
	"github.(*Client).priorArtGateClose":                      {Class: NotificationClassStatus},
	"github.(*Client).reconcileBlockedTargets":                {Class: NotificationClassStatus},
	"github.(*Client).recordReviewEvidenceMerge":              {Class: NotificationClassStatus},
	"github.SyncIssueStageLabelsWith":                         {Class: NotificationClassStatus},
	"github.ensureIssueStagePRLink":                           {Class: NotificationClassStatus},
	"github.githubIssueBacklogSink.AppendToItem":              {Class: NotificationClassStatus},
	"github.githubIssueBacklogSink.CloseItem":                 {Class: NotificationClassStatus},
	"github.githubIssueBacklogSink.LiftBlock":                 {Class: NotificationClassStatus},
	"github.handleMergedPRIssueStage":                         {Class: NotificationClassStatus},
	"github.runRecheckSoak":                                   {Class: NotificationClassStatus},
	"main.postReviewAcknowledgements":                         {Class: NotificationClassStatus},
	"main.rfcNotifySource.Comment":                            {Class: NotificationClassStatus},
	"main.unparkEscalatedPR":                                  {Class: NotificationClassStatus},

	// attention: asks a person to look or act.
	"dashboard.(*ContributeWSHub).parkWrongBase":          {Class: NotificationClassAttention},
	"dashboard.(*Server).handleRepoItemReroute":           {Class: NotificationClassAttention},
	"dashboard.(*Server).handleReviewPipelineSendToHuman": {Class: NotificationClassAttention},
	"dashboard.(*Server).reroutePickupLapsed":             {Class: NotificationClassAttention},
	"dashboard.(*Server).settleRebaseMergeLedger":         {Class: NotificationClassAttention},
	"github.(*Client).NudgeTrackingIssueClosingRefs":      {Class: NotificationClassAttention},
	"github.(*Client).appendFoldedFinding":                {Class: NotificationClassAttention},
	"github.(*Client).bindProwLgtm":                       {Class: NotificationClassAttention},
	"github.(*Client).commentOnHumanFiledTwin":            {Class: NotificationClassAttention},
	"github.(*Client).createIssue":                        {Class: NotificationClassAttention},
	"github.(*Client).handleOneIssueRequest":              {Class: NotificationClassAttention},
	"github.(*Client).parkClankerRequestedPR":             {Class: NotificationClassAttention},
	"github.(*Client).redirectToExistingPR":               {Class: NotificationClassAttention},
	"github.(*Client).resolveDuplicateFixGroup":           {Class: NotificationClassAttention},
	"main.claimEscalationLookup":                          {Class: NotificationClassAttention},
	"main.maintainHeldPRMergeability":                     {Class: NotificationClassAttention},
	"main.rfcCoverageForge.CreateComment":                 {Class: NotificationClassAttention},
	"main.runEscalationSweep":                             {Class: NotificationClassAttention},
	"prfollowup.postStaleHeadPushNotes":                   {Class: NotificationClassAttention},
	"scheduler.(*Scheduler).judgePriorArt":                {Class: NotificationClassAttention},
	"scheduler.(*Scheduler).parkUnclearIssue":             {Class: NotificationClassAttention},
	"scheduler.(*Scheduler).postTriageClarifyComment":     {Class: NotificationClassAttention},

	// required_review: a review submission or request a merge depends on.
	"github.(*Client).QueuePRAutoMergeWithNoteResultAtHead": {Class: NotificationClassRequiredReview},
	"github.(*Client).handleOneReviewRequest":               {Class: NotificationClassRequiredReview},

	// protocol_command: machine-consumed comments.
	"dashboard.postClaimComment":                         {Class: NotificationClassProtocolCommand},
	"github.(*Client).PostClaimNote":                     {Class: NotificationClassProtocolCommand},
	"github.(*Client).reportOperatorPullRequestApproval": {Class: NotificationClassProtocolCommand},
	"main.githubClaimHooks":                              {Class: NotificationClassProtocolCommand},

	// security_event: trust and integrity signals.
	"github.(*Client).handleOnePRRequest":           {Class: NotificationClassSecurityEvent},
	"github.(*Client).markReporterTrustAwaiting":    {Class: NotificationClassSecurityEvent},
	"github.(*Client).reconcileReporterTrustSignal": {Class: NotificationClassSecurityEvent},
	"main.enforceHoldGuard":                         {Class: NotificationClassSecurityEvent},
}

// NotificationWriteSites returns a copy of the write-site inventory.
func NotificationWriteSites() map[string]NotificationWriteSite {
	out := make(map[string]NotificationWriteSite, len(notificationWriteSites))
	for k, v := range notificationWriteSites {
		out[k] = v
	}
	return out
}
