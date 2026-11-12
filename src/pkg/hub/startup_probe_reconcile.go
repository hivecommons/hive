package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// startupProbeReconcileInterval throttles the sweep. Like the NET_ADMIN and
	// host_aliases lanes the drift is STATIC: a Deployment either carries the
	// template's startupProbe / current init image or it does not.
	startupProbeReconcileInterval = 15 * time.Minute

	// startupProbeKubectlTimeout bounds each per-hive kubectl get/patch so one
	// unreachable cluster cannot stall the whole sweep.
	startupProbeKubectlTimeout = 15 * time.Second

	// startupProbeMaxPatchesPerCycle caps PATCHES per sweep: every patch
	// changes the pod template and rolls that hive's pod once.
	startupProbeMaxPatchesPerCycle = 3

	// legacyHiveImageRepo is the pre-rename image repository. Its tags are no
	// longer published, so an init container still pinned to it fails to pull
	// on the next reschedule (#17827).
	legacyHiveImageRepo = "ghcr.io/kubestellar/hive"

	// templateHiveImageRepo is the repository the provisioning template renders
	// for every hive container and init container.
	templateHiveImageRepo = bulkHiveImageRepo

	// hiveContainerName is the main container's name in the template.
	hiveContainerName = "hive"
)

// templateStartupProbe is the startupProbe the provisioning template renders
// onto the hive container (#9692), as the JSON value a patch writes.
type templateStartupProbe struct {
	HTTPGet struct {
		Path string `json:"path"`
		Port int    `json:"port"`
	} `json:"httpGet"`
	InitialDelaySeconds int `json:"initialDelaySeconds"`
	PeriodSeconds       int `json:"periodSeconds"`
	FailureThreshold    int `json:"failureThreshold"`
}

func wantHostedStartupProbe() templateStartupProbe {
	var p templateStartupProbe
	p.HTTPGet.Path = "/api/health"
	p.HTTPGet.Port = dashboardPort
	p.InitialDelaySeconds = hostedStartupProbeInitialDelaySeconds
	p.PeriodSeconds = hostedStartupProbePeriodSeconds
	p.FailureThreshold = hostedStartupProbeFailureThreshold
	return p
}

// liveProbe is the subset of a live probe the template controls. Fields the
// API server defaults (scheme, timeoutSeconds, successThreshold) are ignored
// so a converged Deployment never compares unequal.
type liveProbe struct {
	HTTPGet *struct {
		Path string          `json:"path"`
		Port json.RawMessage `json:"port"`
	} `json:"httpGet"`
	InitialDelaySeconds int `json:"initialDelaySeconds"`
	PeriodSeconds       int `json:"periodSeconds"`
	FailureThreshold    int `json:"failureThreshold"`
}

type liveContainer struct {
	Name         string     `json:"name"`
	Image        string     `json:"image"`
	StartupProbe *liveProbe `json:"startupProbe"`
}

type liveHiveDeployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				InitContainers []liveContainer `json:"initContainers"`
				Containers     []liveContainer `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func startupProbeMatches(live *liveProbe, want templateStartupProbe) bool {
	if live == nil || live.HTTPGet == nil {
		return false
	}
	port := strings.Trim(string(live.HTTPGet.Port), `"`)
	return live.HTTPGet.Path == want.HTTPGet.Path &&
		port == strconv.Itoa(want.HTTPGet.Port) &&
		live.InitialDelaySeconds == want.InitialDelaySeconds &&
		live.PeriodSeconds == want.PeriodSeconds &&
		live.FailureThreshold == want.FailureThreshold
}

// rewriteLegacyHiveImage returns the image with a legacyHiveImageRepo
// repository swapped for templateHiveImageRepo, keeping the tag (or digest).
// ok is false when the image is not on the legacy repository.
func rewriteLegacyHiveImage(image string) (string, bool) {
	image = strings.TrimSpace(image)
	if !strings.HasPrefix(image, legacyHiveImageRepo) {
		return "", false
	}
	rest := image[len(legacyHiveImageRepo):]
	if rest != "" && rest[0] != ':' && rest[0] != '@' {
		return "", false
	}
	return templateHiveImageRepo + rest, true
}

// startupProbePatchJSON is the PURE decision function for the sweep. Given the
// live Deployment JSON (`kubectl get deployment hive -o json`), it returns the
// JSON-Patch that sets the hive container's startupProbe to the template's and
// rewrites any init-container image on the legacy repository, plus a short
// description of each change. An empty patch means already converged.
func startupProbePatchJSON(deployJSON []byte) (string, []string, error) {
	var d liveHiveDeployment
	if err := json.Unmarshal(deployJSON, &d); err != nil {
		return "", nil, fmt.Errorf("parsing deployment: %w", err)
	}
	podSpec := d.Spec.Template.Spec
	var ops []map[string]any
	var changes []string

	idx := -1
	for i, c := range podSpec.Containers {
		if c.Name == hiveContainerName {
			idx = i
			break
		}
	}
	if idx >= 0 {
		want := wantHostedStartupProbe()
		if !startupProbeMatches(podSpec.Containers[idx].StartupProbe, want) {
			ops = append(ops, map[string]any{
				"op":    "add",
				"path":  fmt.Sprintf("/spec/template/spec/containers/%d/startupProbe", idx),
				"value": want,
			})
			changes = append(changes, "startupProbe")
		}
	}

	for i, c := range podSpec.InitContainers {
		img, ok := rewriteLegacyHiveImage(c.Image)
		if !ok {
			continue
		}
		ops = append(ops, map[string]any{
			"op":    "replace",
			"path":  fmt.Sprintf("/spec/template/spec/initContainers/%d/image", i),
			"value": img,
		})
		changes = append(changes, "initContainers["+c.Name+"].image="+img)
	}

	if len(ops) == 0 {
		return "", nil, nil
	}
	b, err := json.Marshal(ops)
	if err != nil {
		return "", nil, err
	}
	return string(b), changes, nil
}

// reconcileStartupProbeIfDue runs the sweep at most once per
// startupProbeReconcileInterval. Safe to call from the poller loop every tick;
// same guarding mutex as reconcileHostAliasesIfDue (poller-loop-only state).
func (s *HubServer) reconcileStartupProbeIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastStartupProbeReconcile.IsZero() ||
		time.Since(s.lastStartupProbeReconcile) >= startupProbeReconcileInterval
	if due {
		s.lastStartupProbeReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcileStartupProbe()
}

// reconcileStartupProbe converges the template's startupProbe (#9692) and the
// current init-container image repository onto hosted hive Deployments
// provisioned before those landed (#17827). Without the startupProbe a slow
// boot is killed by the livenessProbe at ~90s and never recovers. At most
// startupProbeMaxPatchesPerCycle patches per cycle, since each rolls a pod.
func (s *HubServer) reconcileStartupProbe() {
	patched := 0
	deferred := 0
	for _, h := range listSaaSHives() {
		if !netAdminSweepEligible(h.Status) {
			continue
		}
		cluster := s.clusterForHive(&h)
		if cluster == nil || s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}
		ns := hiveHostedNamespacePrefix + h.ID

		ctx, cancel := context.WithTimeout(context.Background(), startupProbeKubectlTimeout)
		getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", "hive",
			"-n", ns, "-o", "json")
		out, err := getCmd.Output()
		cancel()
		if err != nil {
			s.logger.Debug("startupProbe reconcile: could not read hive deployment",
				"hive_id", h.ID, "cluster", cluster.ID, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)

		patch, changes, perr := startupProbePatchJSON(out)
		if perr != nil {
			s.logger.Warn("startupProbe reconcile: could not build patch",
				"hive_id", h.ID, "cluster", cluster.ID, "error", perr)
			continue
		}
		if patch == "" {
			continue
		}
		if patched >= startupProbeMaxPatchesPerCycle {
			deferred++
			continue
		}
		pctx, pcancel := context.WithTimeout(context.Background(), startupProbeKubectlTimeout)
		patchCmd := kubectlForClusterContext(pctx, cluster, "patch", "deployment", "hive",
			"-n", ns, "--type=json", "-p", patch)
		pout, err := patchCmd.CombinedOutput()
		pcancel()
		if err != nil {
			s.markClusterUnreachable(cluster.ID)
			s.logger.Warn("startupProbe reconcile: patch failed — will retry next sweep",
				"hive_id", h.ID, "cluster", cluster.ID,
				"output", strings.TrimSpace(string(pout)), "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)
		patched++
		s.logger.Info("reconciled template startupProbe/init image onto hive deployment (rolls the pod once)",
			"hive_id", h.ID, "cluster", cluster.ID,
			"changes", strings.Join(changes, ","), "patched_this_cycle", patched)
	}
	if deferred > 0 {
		s.logger.Info("startupProbe reconcile: rate limit reached, remaining hives deferred to next cycle",
			"patched", patched, "deferred", deferred,
			"max_per_cycle", startupProbeMaxPatchesPerCycle,
			"next_cycle_in", startupProbeReconcileInterval.String())
	}
}
