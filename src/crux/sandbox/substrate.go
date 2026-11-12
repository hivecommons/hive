package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/sandbox/substrateapi"
	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// Agent Substrate runtime (ADR-0026 phase 2; sibling of the OpenShell runtime
// in openshell.go).
//
// SubstrateLauncher implements Launcher on top of a Substrate control plane
// (ate-api-server): it registers a per-kick ActorTemplate from the kick spec,
// creates an Actor from it, attaches the egress policy, resumes the actor,
// drives the kick through the in-actor bridge (substratebridge) over the
// atenet-router CONNECT tunnel, and deletes — or, with snapshot_on_idle,
// suspends — the actor afterwards. Substrate owns the gVisor/microVM sandbox,
// the snapshot store and the egress gateway; hive's part is the spec, the
// policy and the same cleanup and timeout semantics the Job launcher has.
//
// Two points differ from the OpenShell and Job runtimes and are deliberate:
//
//   - Substrate has no exec/attach RPC and cannot mount an existing PVC, so
//     the workspace is a per-actor durableDir and hive copies it in and out
//     through the bridge (push, run, pull). The local workspace hive opened
//     the kick in is therefore still the source of truth after the kick.
//   - Substrate refuses tag-only images; the sandbox image must be pinned by
//     digest (…@sha256:…). Validate enforces it up front so the failure is a
//     config error, not a CreateActorTemplate rejection mid-kick.

const (
	// SubstrateDefaultEndpoint is the in-cluster Control API address the
	// default ate-setup install exposes.
	SubstrateDefaultEndpoint = "api.ate-system.svc:443"
	// SubstrateDefaultRouterURL is the in-cluster atenet-router CONNECT
	// listener hive reaches actors through.
	SubstrateDefaultRouterURL = "https://atenet-router.ate-system.svc:8444"
	// SubstrateDefaultAtespace is the atespace actors land in when none is
	// configured.
	SubstrateDefaultAtespace = "hive"
	// SubstrateDefaultTokenFile is the projected ServiceAccount token
	// (audience api.ate-system.svc) the Control API authenticates.
	SubstrateDefaultTokenFile = "/var/run/secrets/ate/token"
	// SubstrateDefaultCAFile is the servicedns ClusterTrustBundle projection
	// the Control API serving certificate chains to.
	SubstrateDefaultCAFile = "/var/run/secrets/ate/ca.crt"
	// SubstrateDefaultRouterClientBundle is the projected podCertificate
	// bundle atenet-router authenticates hive with.
	SubstrateDefaultRouterClientBundle = "/run/router-client/credential-bundle.pem"
	// SubstrateDefaultRouterCAFile is the servicedns ClusterTrustBundle the
	// router's serving certificate chains to.
	SubstrateDefaultRouterCAFile = "/run/router-client/router-ca.pem"
	// SubstrateWorkspaceMount is where the actor sees the per-kick
	// workspace, matching the other launchers.
	SubstrateWorkspaceMount = substratebridge.DefaultWorkspace
	// SubstrateDefaultCPUMillicores and SubstrateDefaultMemoryBytes are the
	// template resource limits when the operator sets none; Substrate
	// requires both.
	SubstrateDefaultCPUMillicores = 2000
	SubstrateDefaultMemoryBytes   = 4 << 30
	// SubstrateExitCodeUnknown mirrors OpenShellExitCodeUnknown.
	SubstrateExitCodeUnknown = -1

	// SubstratePolicyProfileLabel carries sandbox.substrate.policy_profile on
	// the per-kick template's worker selector key space; Substrate has no
	// annotations, so the profile travels as a template label hive and
	// operators can filter on.
	SubstratePolicyProfileLabel = "hive.dev/policy-profile"

	substrateNamePrefix   = "hive-kick-"
	substrateNameMax      = 63
	substrateRequestWait  = 30 * time.Second
	substrateReadyPoll    = 2 * time.Second
	substrateEgressPolicy = "default"
	substrateContainer    = "agent"
	substrateVolume       = "workspace"
	substrateHTTPSPort    = 443
)

// SubstrateOptions is the launcher's configuration; crux/agent builds it
// from config.SandboxSubstrateConfig.
type SubstrateOptions struct {
	Endpoint           string
	Atespace           string
	Token              string
	TokenFile          string
	CAFile             string
	InsecureTLS        bool
	Timeout            time.Duration
	RouterURL          string
	RouterClientBundle string
	RouterCAFile       string
	BridgePort         int
	Image              string
	Command            []string
	CPUMillicores      int64
	MemoryBytes        int64
	WorkerSelector     map[string]string
	SnapshotOnIdle     bool
	AllowHosts         []string
	PolicyProfile      string
	// Logger receives cleanup diagnostics that do not fail the kick; nil
	// means slog.Default().
	Logger *slog.Logger
}

func (o SubstrateOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// Validate reports the first configuration error a kick would hit.
func (o SubstrateOptions) Validate() error {
	if _, _, err := substrateAddress(o.endpoint()); err != nil {
		return err
	}
	if _, err := substrateRouterAddress(o.routerURL()); err != nil {
		return err
	}
	if o.Image != "" && !substrateImageDigest.MatchString(o.Image) {
		return fmt.Errorf("substrate: image %q must be pinned by digest (…@sha256:<64 hex>); Substrate refuses tag-only images", o.Image)
	}
	if o.CPUMillicores < 0 || o.MemoryBytes < 0 {
		return errors.New("substrate: cpu_millicores and memory_bytes must not be negative")
	}
	if o.BridgePort < 0 || o.BridgePort > 65535 {
		return fmt.Errorf("substrate: bridge_port %d is out of range", o.BridgePort)
	}
	_, err := SubstrateEgressPolicy(o.AllowHosts)
	return err
}

func (o SubstrateOptions) endpoint() string {
	if e := strings.TrimSpace(o.Endpoint); e != "" {
		return e
	}
	return SubstrateDefaultEndpoint
}

func (o SubstrateOptions) atespace() string {
	if a := strings.TrimSpace(o.Atespace); a != "" {
		return a
	}
	return SubstrateDefaultAtespace
}

func (o SubstrateOptions) routerURL() string {
	if r := strings.TrimSpace(o.RouterURL); r != "" {
		return r
	}
	return SubstrateDefaultRouterURL
}

func (o SubstrateOptions) bridgePort() int {
	if o.BridgePort > 0 {
		return o.BridgePort
	}
	return substratebridge.DefaultPort
}

// substrateControl is the slice of the Control API the launcher uses; it
// exists so tests can drive the launcher with a fake and so an upstream
// RPC rename shows up here, not in every call site.
type substrateControl interface {
	CreateActorTemplate(ctx context.Context, tpl *substrateapi.ActorTemplate) (*substrateapi.ActorTemplate, error)
	DeleteActorTemplate(ctx context.Context, ref *substrateapi.ObjectRef) error
	CreateActor(ctx context.Context, actor *substrateapi.Actor) (*substrateapi.Actor, error)
	GetActor(ctx context.Context, ref *substrateapi.ObjectRef) (*substrateapi.Actor, error)
	ResumeActor(ctx context.Context, ref *substrateapi.ObjectRef) error
	SuspendActor(ctx context.Context, ref *substrateapi.ObjectRef, mode substrateapi.DurabilityMode) error
	DeleteActor(ctx context.Context, ref *substrateapi.ObjectRef) error
	CreateActorEgressPolicy(ctx context.Context, ref *substrateapi.ObjectRef, policy *substrateapi.EgressPolicy) error
	ListWorkers(ctx context.Context) ([]*substrateapi.Worker, error)
}

// substrateBridge is the launcher's view of the in-actor bridge.
type substrateBridge interface {
	WaitReady(ctx context.Context, ref *substrateapi.ObjectRef, poll time.Duration) error
	Push(ctx context.Context, ref *substrateapi.ObjectRef, workspace string) error
	// Run copies the kick's output to live (nil allowed) as it arrives.
	Run(ctx context.Context, ref *substrateapi.ObjectRef, req substratebridge.RunRequest, live io.Writer) (substratebridge.RunResponse, error)
	Pull(ctx context.Context, ref *substrateapi.ObjectRef, workspace string) error
}

type substrateClient interface {
	Control() substrateControl
	Bridge() substrateBridge
	Close() error
}

// SubstrateLauncher runs sandboxed kicks as Substrate actors.
type SubstrateLauncher struct {
	Options SubstrateOptions

	// newClient overrides the client constructor in tests.
	newClient func(SubstrateOptions) (substrateClient, error)
	// now overrides the clock in tests.
	now func() time.Time
	// kicks is where each kick's actor states and output are recorded for
	// the dashboard's terminal pane; nil means DefaultSubstrateKicks.
	kicks *SubstrateKicks
}

func (l *SubstrateLauncher) kickRecord() *SubstrateKicks {
	if l.kicks != nil {
		return l.kicks
	}
	return DefaultSubstrateKicks
}

func (l *SubstrateLauncher) client() (substrateClient, error) {
	if l.newClient != nil {
		return l.newClient(l.Options)
	}
	return newSubstrateGRPCClient(l.Options)
}

// Run implements Launcher.
func (l *SubstrateLauncher) Run(ctx context.Context, spec LaunchSpec) (Result, error) {
	res := Result{ExitCode: SubstrateExitCodeUnknown}
	if spec.ClineProvider != nil {
		_ = spec.ClineProvider.Close()
		return res, errors.New("substrate: scoped Cline provider files are Podman-only; the Substrate runtime takes no credential files")
	}
	if err := l.Options.Validate(); err != nil {
		return res, err
	}
	// Validate just proved the hosts parse, so this cannot fail; the policy
	// is built before anything is created so a kick never has a half-built
	// actor to clean up on a config error.
	policy, err := SubstrateEgressPolicy(l.Options.AllowHosts)
	if err != nil {
		return res, err
	}
	image := strings.TrimSpace(l.Options.Image)
	if image == "" {
		image = strings.TrimSpace(spec.Image)
	}
	if image == "" {
		return res, errors.New("substrate: image is required")
	}
	if !substrateImageDigest.MatchString(image) {
		return res, fmt.Errorf("substrate: image %q must be pinned by digest (…@sha256:<64 hex>); set sandbox.substrate.image to the digest-pinned sandbox image", image)
	}
	if strings.TrimSpace(spec.Workspace) == "" {
		return res, errors.New("substrate: workspace is required")
	}
	if len(spec.Command) == 0 {
		return res, errors.New("substrate: command is required")
	}
	if l.Options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.Options.Timeout)
		defer cancel()
	}

	client, err := l.client()
	if err != nil {
		return res, err
	}
	defer func() { _ = client.Close() }()
	control, bridge := client.Control(), client.Bridge()

	now := time.Now
	if l.now != nil {
		now = l.now
	}
	name := substrateActorName(spec.Name, now())
	ref := &substrateapi.ObjectRef{Atespace: l.Options.atespace(), Name: name}

	// The caller's ctx may be dead already (kick timeout) or die between any
	// two of these requests; cleanup falls back to a fresh, request-bounded
	// context so a finished actor is still removed.
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	live := func() context.Context {
		if ctx.Err() == nil {
			return ctx
		}
		fresh, cancel := context.WithTimeout(context.Background(), substrateRequestWait)
		cancels = append(cancels, cancel)
		return fresh
	}

	if _, err := control.CreateActorTemplate(ctx, l.actorTemplate(ref, image)); err != nil {
		return res, fmt.Errorf("substrate: creating actor template %s: %w", name, err)
	}
	cleanupTemplate := func() { _ = control.DeleteActorTemplate(live(), ref) }

	created, err := control.CreateActor(ctx, &substrateapi.Actor{
		Metadata:       &substrateapi.ResourceMetadata{Atespace: ref.Atespace, Name: name},
		ActorTemplate:  ref,
		WorkerSelector: substrateSelector(l.Options.WorkerSelector),
	})
	if err != nil {
		cleanupTemplate()
		return res, fmt.Errorf("substrate: creating actor %s: %w", name, err)
	}
	kicks := l.kickRecord()
	kick := kicks.begin(spec.Name, SubstrateActorInfo{Name: name, Atespace: ref.Atespace, State: SubstrateStateCreated})
	kicks.observe(kick, created)
	// Delete is the default: a kick is a one-shot and keeping a snapshot
	// around costs object storage. snapshot_on_idle keeps the actor
	// suspended instead. Nothing resumes it yet (the next kick is a cold
	// boot), so only the latest snapshot per agent is retained: the one it
	// replaces is deleted. The Control API has no ListActors, so actors left
	// by an earlier hive process are not reclaimed.
	cleanupActor := func() {
		if l.Options.SnapshotOnIdle && ctx.Err() == nil {
			if err := control.SuspendActor(live(), ref, substrateapi.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS); err == nil {
				kicks.update(kick, func(i *SubstrateActorInfo) {
					i.State, i.Suspended, i.Done = SubstrateStateSuspended, true, true
				})
				cleanupTemplate()
				prev := kicks.swapSuspended(spec.Name, SubstrateActorInfo{Name: name, Atespace: ref.Atespace})
				switch {
				case prev.Name == "" || prev.Name == name:
				case prev.Atespace != ref.Atespace:
					// This launcher's client is for the current atespace;
					// an actor left in another one is not ours to reach.
					l.Options.logger().Warn("substrate: superseded suspended actor left in another atespace",
						"agent", spec.Name, "actor", prev.Name, "atespace", prev.Atespace, "current_atespace", ref.Atespace)
				default:
					prevRef := &substrateapi.ObjectRef{Atespace: prev.Atespace, Name: prev.Name}
					if err := control.DeleteActor(live(), prevRef); err != nil {
						l.Options.logger().Warn("substrate: deleting superseded suspended actor",
							"agent", spec.Name, "actor", prev.Name, "atespace", prev.Atespace, "error", err)
					}
				}
				return
			}
		}
		_ = control.DeleteActor(live(), ref)
		kicks.update(kick, func(i *SubstrateActorInfo) { i.State, i.Done = SubstrateStateDeleting, true })
		cleanupTemplate()
	}

	if spec.NetworkNone {
		policy.Rules = nil
	}
	// The Control API requires the singleton policy's atespace to match the
	// actor's (egress_policy.metadata.atespace is a required field).
	policy.Metadata.Atespace = ref.Atespace
	if err := control.CreateActorEgressPolicy(ctx, ref, policy); err != nil {
		cleanupActor()
		return res, fmt.Errorf("substrate: setting egress policy on actor %s: %w", name, err)
	}
	if err := control.ResumeActor(ctx, ref); err != nil {
		cleanupActor()
		return res, fmt.Errorf("substrate: resuming actor %s: %w", name, err)
	}
	kicks.update(kick, func(i *SubstrateActorInfo) { i.State = SubstrateStateResuming })
	observe := func(a *substrateapi.Actor) { kicks.observe(kick, a) }
	if err := substrateWaitRunning(ctx, control, ref, substrateReadyPoll, observe); err != nil {
		cleanupActor()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("substrate: actor %s never became running: %w", name, err)
	}
	if err := bridge.WaitReady(ctx, ref, substrateReadyPoll); err != nil {
		cleanupActor()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("substrate: bridge in actor %s never answered: %w", name, err)
	}
	if err := bridge.Push(ctx, ref, spec.Workspace); err != nil {
		cleanupActor()
		return res, fmt.Errorf("substrate: pushing workspace into actor %s: %w", name, err)
	}
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = SubstrateWorkspaceMount
	}
	out, runErr := bridge.Run(ctx, ref, substratebridge.RunRequest{Command: spec.Command, Env: substrateEnv(spec), WorkDir: workDir}, kicks.output(kick))
	if runErr == nil {
		res.Stdout, res.Stderr, res.ExitCode = out.Stdout, out.Stderr, out.ExitCode
		kicks.update(kick, func(i *SubstrateActorInfo) { i.Exited, i.ExitCode = true, out.ExitCode })
		if out.Error != "" {
			runErr = errors.New(out.Error)
		}
	}
	var pullErr error
	if ctx.Err() == nil {
		pullErr = bridge.Pull(ctx, ref, spec.Workspace)
	}
	cleanupActor()
	if ctx.Err() != nil {
		// Kick timeout or cancel: the actor has been removed above so the
		// workload does not keep running on the cluster.
		return res, ctx.Err()
	}
	if runErr != nil {
		return res, fmt.Errorf("substrate: running kick in actor %s: %w", name, runErr)
	}
	if pullErr != nil {
		return res, fmt.Errorf("substrate: pulling workspace out of actor %s: %w", name, pullErr)
	}
	return res, nil
}

// substrateWaitRunning polls GetActor until the actor is RUNNING, or fails
// when it crashes. A deleted or suspended actor during the wait is a failure
// too: nothing hive did asked for it. observe (nil allowed) sees every
// actor GetActor returns.
func substrateWaitRunning(ctx context.Context, control substrateControl, ref *substrateapi.ObjectRef, poll time.Duration, observe func(*substrateapi.Actor)) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		actor, err := control.GetActor(ctx, ref)
		if err != nil {
			return err
		}
		if observe != nil {
			observe(actor)
		}
		switch state := actor.GetStatus().GetState(); state {
		case substrateapi.ActorState_ACTOR_STATE_RUNNING:
			return nil
		case substrateapi.ActorState_ACTOR_STATE_CRASHED:
			return fmt.Errorf("actor crashed: %s", actor.GetStatus().GetCrash().GetMessage())
		case substrateapi.ActorState_ACTOR_STATE_DELETING, substrateapi.ActorState_ACTOR_STATE_SUSPENDING:
			return fmt.Errorf("actor entered %s while being resumed", state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
