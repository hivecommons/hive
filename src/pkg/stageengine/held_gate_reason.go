package stageengine

// IsHeldGateReason reports whether a lane reason is a gate, not a stage
// (stageHeldGateReasons, hivecommons/hive#17327): an open sentinel alert or a
// hold-class label stopped the lane before it read anything about the head,
// mergeability included. The human route still learns the head's own state
// from elsewhere and rides the gate on it.
func IsHeldGateReason(reason string) bool {
	return stageHeldGateReasons[reason]
}
