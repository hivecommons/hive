package main

import (
	"errors"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// sandboxTopicHandler applies a sandboxed kick's spooled topic request to
// its agent's own bead store (hivecommons/hive#18572), the store `bd topic`
// writes in-pod. The relay has already attributed the call to the kick's
// manifest agent; this only looks that agent's store up. stores is read at
// call time, not at wire time.
func sandboxTopicHandler(stores func() map[string]*beads.Store) github.SandboxTopicHandler {
	return func(call github.SandboxTopicCall) github.SandboxTopicResponse {
		var store *beads.Store
		if stores != nil {
			store = stores()[call.Agent]
		}
		if store == nil {
			return github.SandboxTopicResponse{Error: "agent " + call.Agent + " has no bead store on this hive"}
		}
		switch call.Op {
		case github.SandboxTopicOpClaim:
			var claim beads.TopicClaim
			var err error
			if call.Pending {
				claim, err = store.ClaimTopicForBranch(call.Agent, call.Key, call.Repo, call.Branch)
			} else {
				claim, err = store.ClaimTopic(call.Agent, beads.TopicClaim{Key: call.Key, Repo: call.Repo, PR: call.PR, Branch: call.Branch})
			}
			if err != nil {
				resp := github.SandboxTopicResponse{Error: err.Error(), Refused: errors.Is(err, beads.ErrTopicClaimed)}
				if resp.Refused {
					resp.Claim = &claim
				}
				return resp
			}
			return github.SandboxTopicResponse{OK: true, Claim: &claim, Pending: claim.PR == 0}
		case github.SandboxTopicOpGet:
			claim, ok := store.Topic(call.Key)
			if !ok {
				return github.SandboxTopicResponse{Refused: true, Error: "no open claim for topic " + call.Key}
			}
			return github.SandboxTopicResponse{OK: true, Claim: &claim, Pending: claim.PR == 0}
		case github.SandboxTopicOpList:
			return github.SandboxTopicResponse{OK: true, Claims: store.OpenTopics()}
		case github.SandboxTopicOpRelease:
			released, err := store.ReleaseTopic(call.Key, beads.TopicStateReleased)
			if err != nil {
				return github.SandboxTopicResponse{Error: err.Error()}
			}
			return github.SandboxTopicResponse{OK: true, Released: released}
		}
		return github.SandboxTopicResponse{Error: "unknown topic op " + call.Op}
	}
}
