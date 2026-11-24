package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/authz"
)

// guardChangedFiles records the outgoing file list on res and refuses an
// empty push or one that touches a protected path.
func (b *Broker) guardChangedFiles(ctx context.Context, res *Result) error {
	files, err := b.changedFiles(ctx)
	if err != nil {
		return err
	}
	res.ChangedFiles = files
	protected := b.protectedPaths()
	if len(files) == 0 {
		b.observeChangedFiles(files, protected, authz.Verdict{Effect: authz.EffectDeny, ReasonCode: authz.ReasonEmptyPush})
		return errors.New("pushbroker: no committed changes to push")
	}
	if rejected := ProtectedPathViolations(files, protected); len(rejected) > 0 {
		b.observeChangedFiles(files, protected, authz.Verdict{Effect: authz.EffectDeny, ReasonCode: authz.ReasonProtectedPath})
		res.ProtectedReject = rejected
		return fmt.Errorf("pushbroker: protected paths changed: %s", strings.Join(rejected, ", "))
	}
	b.observeChangedFiles(files, protected, authz.Verdict{Effect: authz.EffectAllow, ReasonCode: authz.ReasonOK})
	return nil
}

// observeChangedFiles reports this guard's verdict to the authz shadow. It is
// called after the guard has decided and its result is not read back: the
// guard above stays the only authority (RFC #18988 phase 0/1).
func (b *Broker) observeChangedFiles(files, protected []string, legacy authz.Verdict) {
	shadow := b.Shadow
	if shadow == nil {
		shadow = authz.DefaultShadow()
	}
	if shadow == nil {
		return
	}
	shadow.Observe(authz.Request{
		Actor:    authz.Actor{Agent: b.AgentName, Identified: strings.TrimSpace(b.AgentName) != ""},
		Action:   authz.Action{Kind: authz.KindBranchPush, Surface: authz.SurfaceBroker, Forge: authz.ForgeGitHub},
		Resource: authz.Resource{Repo: b.Repo, Branch: b.Branch, Paths: files},
		Context:  authz.Context{ProtectedPaths: protected},
	}, legacy)
}

func (b *Broker) protectedPaths() []string {
	if len(b.ProtectedPaths) > 0 {
		return b.ProtectedPaths
	}
	return DefaultProtectedPaths
}
