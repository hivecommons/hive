package hub

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"
)

// ============================================================================
// HOSTED NAMESPACE IDENTITY — making a namespace self-describing
// ============================================================================
//
// A hosted hive's Kubernetes namespace is created ONCE, at provisioning time,
// named after whatever ID the hive had THEN — almost always a pool
// placeholder ID like "hive-hosted-hosted-available-oke-03-placeholder-y99x".
// When that placeholder is later claimed by a real owner/org (approve-provision
// or the manual assign path), the hive's meta.json is rewritten with the real
// identity, but the NAMESPACE NAME is never renamed — Kubernetes has no atomic
// namespace-rename primitive, and renaming would mean recreating every object
// inside it. The namespace therefore keeps its placeholder name FOREVER, and
// there is no way to go from "which namespace is this?" back to "which hive/
// org is running here?" without cross-referencing the hub's hive registry and
// reverse-engineering a mangled slug.
//
// The fix is not to rename the namespace — it is to make the namespace
// self-describing by stamping identity labels (and a human-readable
// annotation) onto it at every point the hive's identity is known or changes:
//
//  1. hosted namespace CREATION (provisionHive) — stamp what's known then
//     (at minimum the hive_id; org/name may still be placeholder values).
//  2. CLAIM (handleApproveProvision) — the hive's real name/org become known
//     here for the first time, so the labels MUST be (re)written.
//  3. ASSIGN / reassign (handleAssignHive) — same: refresh the labels so a
//     reassigned placeholder's namespace reflects its new owner.
//
// All three call stampHostedNamespaceIdentity, which idempotently patches the
// namespace's labels/annotations via `kubectl label`/`kubectl annotate`
// (--overwrite) rather than recreating it, and merges into whatever labels
// already exist instead of clobbering them.

const (
	// hiveLabelPrefix is the label/annotation prefix already established
	// elsewhere in this package (see self_upgrade.go's restart-at/
	// upgrade-target-sha annotations) — reused here rather than inventing a
	// second convention.
	hiveLabelPrefix = "hive.kubestellar.io/"

	// hiveNameLabel/hiveOrgLabel/hiveIDLabel are RFC-1123-safe label VALUES
	// (see sanitizeLabelValue) recording the hive's identity on its namespace.
	hiveNameLabel = hiveLabelPrefix + "hive-name"
	hiveOrgLabel  = hiveLabelPrefix + "org"
	hiveIDLabel   = hiveLabelPrefix + "hive-id"

	// hiveDisplayNameAnnotation carries the EXACT, unsanitized hive name.
	// Annotations allow arbitrary values, so this is the recoverable source of
	// truth when the sanitized label value has been lossily transformed
	// (capitals folded, unicode stripped, truncated to 63 chars, ...).
	hiveDisplayNameAnnotation = hiveLabelPrefix + "display-name"

	// maxLabelValueLen is the Kubernetes label-value length limit (RFC 1123
	// label / DNS label rules), enforced by sanitizeLabelValue.
	maxLabelValueLen = 63
)

// hiveNamespaceIdentityLabels builds the labels and annotations that make a
// hosted hive's namespace self-describing: given the hive's display name, its
// forge org, and its internal hive_id, it returns the label map (sanitized,
// RFC-1123-safe values) and the annotation map (the exact display name).
//
// A field whose sanitized value is empty is OMITTED from the labels map
// rather than written as an invalid/empty label — Kubernetes rejects an empty
// label value only in some API versions, but an omitted key is unambiguous
// and never fails validation. The display-name annotation is included only
// when the raw name is non-empty (an empty annotation value is legal but
// pointless — the omission of hiveNameLabel already tells the same story).
func hiveNamespaceIdentityLabels(name, org, hiveID string) (labels map[string]string, annotations map[string]string) {
	labels = map[string]string{}
	annotations = map[string]string{}

	if v := sanitizeLabelValue(name); v != "" {
		labels[hiveNameLabel] = v
	}
	if v := sanitizeLabelValue(org); v != "" {
		labels[hiveOrgLabel] = v
	}
	if v := sanitizeLabelValue(hiveID); v != "" {
		labels[hiveIDLabel] = v
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		annotations[hiveDisplayNameAnnotation] = trimmed
	}

	return labels, annotations
}

// sanitizeLabelValue converts an arbitrary string into a valid Kubernetes
// label value: lower-case, restricted to [a-z0-9.-], trimmed of leading and
// trailing non-alphanumeric characters, and capped at 63 characters (the
// RFC-1123 label-value limit).
//
// Kubernetes label values must match
// (([A-Za-z0-9])|([A-Za-z0-9][A-Za-z0-9\-_.]*[A-Za-z0-9]))? — this function
// deliberately narrows that to a stricter [a-z0-9.-] subset (no uppercase, no
// underscore) so the SAME sanitizer is safe to reuse anywhere a
// lower-cased/DNS-label-like value is wanted, matching the style of the
// existing `sanitize` helper in saas_provision.go (which does the same for
// hive IDs, minus dots). Unicode and any other byte is dropped outright
// rather than transliterated — a confident wrong transliteration is worse
// than an honestly shortened value, and the exact original is always
// recoverable from the paired display-name annotation.
//
// Edge cases handled explicitly so this never produces an invalid label
// value: empty input returns ""; a string that sanitizes to only separators
// (e.g. "---", "...") returns "" after trimming; a value longer than 63
// characters is truncated and then re-trimmed in case truncation left a
// trailing separator.
func sanitizeLabelValue(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			// Unicode letters/digits, spaces, underscores, and everything else
			// are dropped rather than mapped to '-': collapsing runs of dropped
			// characters to a single separator would still be a lossy guess at
			// intent, and the raw value survives in the paired annotation.
		}
	}
	out := b.String()

	out = strings.Trim(out, "-.")
	if out == "" {
		return ""
	}

	if len(out) > maxLabelValueLen {
		out = out[:maxLabelValueLen]
		out = strings.Trim(out, "-.")
	}

	return out
}

// hostedNamespaceForHive returns the Kubernetes namespace a hosted hive runs
// in. Every call site in this package has historically inlined
// "hive-hosted-"+id (see hiveHostedNamespacePrefix in saas.go); this is the
// same computation, added here so the claim/assign call sites below and any
// future caller share one definition instead of re-deriving it.
func hostedNamespaceForHive(h *SaaSHive) string {
	if h == nil {
		return ""
	}
	return hiveHostedNamespacePrefix + h.ID
}

// stampHostedNamespaceIdentity idempotently patches a hosted hive's namespace
// with the identity labels/annotations from hiveNamespaceIdentityLabels, via
// `kubectl label`/`kubectl annotate --overwrite`. It merges into whatever
// labels the namespace already carries rather than recreating it — safe to
// call on a namespace that does not exist yet only if the caller applies the
// namespace-creation manifest first (see provisionHive, which does).
//
// Best-effort: a failure here (e.g. no kubectl on PATH, namespace not yet
// created, transient API error) is logged and swallowed rather than failing
// the caller's request. The alternative — failing a claim/assign because a
// cosmetic label patch failed — would turn an operability nicety into an
// availability regression, and the heartbeat/provisioning retry paths in this
// package already tolerate exactly this kind of partial failure (see
// makeVanityHostServable's "log at Warn, keep working" precedent).
//
// Bounded by stampNamespaceIdentityTimeout via kubectlForClusterContext —
// the same context+"--request-timeout" idiom buildSingleClusterHealth and
// RolloutRestartSelf already use for kubectl calls that must never hang the
// HTTP request that triggered them (claim/assign are synchronous admin API
// calls; an unreachable cluster must fail this cosmetic step fast, not stall
// the response).
func stampHostedNamespaceIdentity(cluster *ClusterConfig, namespace, name, org, hiveID string, logger *slog.Logger) {
	if cluster == nil || strings.TrimSpace(namespace) == "" {
		return
	}
	labels, annotations := hiveNamespaceIdentityLabels(name, org, hiveID)
	if len(labels) == 0 && len(annotations) == 0 {
		return
	}

	if len(labels) > 0 {
		args := []string{"--request-timeout", stampNamespaceIdentityTimeout.String(), "label", "namespace", namespace, "--overwrite"}
		for k, v := range labels {
			args = append(args, fmt.Sprintf("%s=%s", k, v))
		}
		ctx, cancel := context.WithTimeout(context.Background(), stampNamespaceIdentityTimeout)
		out, err := kubectlForClusterContext(ctx, cluster, args...).CombinedOutput()
		cancel()
		if err != nil {
			if logger != nil {
				logger.Warn("failed to label hosted namespace with hive identity",
					"namespace", namespace, "cluster", cluster.ID, "output", string(out), "error", err)
			}
		}
	}

	if len(annotations) > 0 {
		args := []string{"--request-timeout", stampNamespaceIdentityTimeout.String(), "annotate", "namespace", namespace, "--overwrite"}
		for k, v := range annotations {
			args = append(args, fmt.Sprintf("%s=%s", k, v))
		}
		ctx, cancel := context.WithTimeout(context.Background(), stampNamespaceIdentityTimeout)
		out, err := kubectlForClusterContext(ctx, cluster, args...).CombinedOutput()
		cancel()
		if err != nil {
			if logger != nil {
				logger.Warn("failed to annotate hosted namespace with hive display name",
					"namespace", namespace, "cluster", cluster.ID, "output", string(out), "error", err)
			}
		}
	}
}

// stampNamespaceIdentityTimeout bounds each kubectl label/annotate call in
// stampHostedNamespaceIdentity. Matches upgradeKubectlTimeout's value — both
// are a single synchronous kubectl call issued from an HTTP handler, so both
// need to fail fast against an unreachable cluster rather than hold the
// request (or, in tests with no kubectl on PATH / no reachable API server,
// hold the test) for kubectl's own default timeout.
const stampNamespaceIdentityTimeout = 15 * time.Second

// ============================================================================
// OPTION B — a name-bearing Route, without renaming the namespace
// ============================================================================
//
// A hosted spoke's public URL on OpenShift is an OpenShift Route host. Because
// the namespace never renames (see above), a Route whose host is DERIVED from
// the namespace name — the original provisioning template sets exactly one
// host, DashboardHost = "<hive-id>.<cluster domain>" — permanently encodes the
// placeholder slug (e.g. "hosted-available-vllmd-06.apps...") rather than the
// hive's name, even after the hive is claimed by "TradingAsBuddies"/devx-prod.
//
// RENAMING OR MIGRATING THE NAMESPACE IS OFF THE TABLE: it would mean a
// teardown + recreate + PVC/PV migration + outage. Nothing here creates a
// replacement namespace or moves a PVC.
//
// The fix already exists in this file as of the vanity-URL feature
// (addVanityHostToIngress, called from handleAssignHive and the retroactive
// repair repairVanityURLForHive): OpenShift lets a Route's spec.host be set
// EXPLICITLY, independent of the namespace it lives in, so an ADDITIONAL
// "<name>-vanity" Route — same Service backend, same namespace — gives the
// hive a name-bearing URL without touching the namespace, PVC, Deployment, or
// Service. The ORIGINAL Route (and its placeholder host) is left completely
// alone, so any in-flight bookmark/callback against it keeps working.
//
// That existing mechanism keys its hostname off org+primary-repo
// (generateHiveID). hiveNameHostLabel below is the DNS-label-safe sanitizer
// for building the SAME kind of name-bearing host from the hive's own display
// NAME instead — reusing sanitizeLabelValue's stripping logic but enforcing
// hostname-specific rules (a label value may lead/trail with '.', a DNS label
// may not). hiveNameVanityHost composes it into a full host; handleAssignHive
// (saas.go) calls it to prefer a name-bearing host over the org/repo-derived
// one when the hive has a display name, then hands the result to the SAME
// existing get-or-create path — makeVanityHostServable /
// addVanityHostToIngress — so the idempotency, cluster-domain handling, and
// "never adopt an unservable host" guarantees are unchanged from before this
// feature existed.

// hiveNameHostLabel converts a hive's display name into a single valid DNS
// label (RFC 1123): lowercase, [a-z0-9-] only, no leading or trailing dash,
// max 63 characters. This is DELIBERATELY STRICTER than sanitizeLabelValue —
// a Kubernetes label VALUE may contain '.', but a single DNS LABEL (one dot-
// separated segment of a hostname) may not, so '.' is dropped here rather
// than kept.
//
// Unicode and punctuation are dropped outright, matching sanitizeLabelValue's
// stance: a confident wrong transliteration is worse than an honestly
// shortened value, and the exact name is recoverable from the
// hive.kubestellar.io/display-name annotation stamped on the namespace.
func hiveNameHostLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxLabelValueLen {
		out = strings.Trim(out[:maxLabelValueLen], "-")
	}
	return out
}

// vanityHostSuffixLen is the length of the random uniqueness suffix appended
// to a name-bearing vanity host, matching generateHiveID's existing suffix
// length so both host-generation schemes read consistently and collide with
// the same (very low) probability.
const vanityHostSuffixLen = 4

// randomHostSuffix returns a short lowercase alphanumeric string, used to
// keep two hives with similar/identical sanitized names from colliding on the
// same vanity host. Mirrors generateHiveID's own suffix generation.
func randomHostSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, vanityHostSuffixLen)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// hiveNameVanityHost builds a name-bearing hostname for a hive, of the form
// "<sanitized-name>-<suffix>.<domain>". name is typically the hive's
// ProjectName/display name (falling back to org-repo elsewhere when a hive
// has no display name — see hiveNameOrFallback). domain is the cluster's
// wildcard apps domain, which the caller must supply from a source of truth
// it already trusts (cluster.Domain from config, or one parsed off an
// existing Route — see vanityHostDomainFromExistingRoute) rather than a
// hardcoded value.
//
// Returns "" when either input is unusable (empty domain, or a name that
// sanitizes to nothing) — the caller must treat that as "cannot build a host"
// and skip rather than fabricate one, per the same fail-closed rule
// addVanityHostToIngress already follows for an unreachable cluster.
func hiveNameVanityHost(name, domain string) string {
	domain = strings.TrimSpace(strings.Trim(domain, "."))
	label := hiveNameHostLabel(name)
	if domain == "" || label == "" {
		return ""
	}
	return label + "-" + randomHostSuffix() + "." + domain
}

// vanityHostDomainFromExistingRoute extracts the wildcard apps domain from an
// existing Route's host, by stripping its leading "<hive-id>." label. Used as
// a fallback source of truth for the domain when the caller does not already
// have cluster.Domain in hand, so the domain is always DERIVED from
// something real on the cluster rather than assumed. Returns "" when
// existingHost does not look like "<something>.<domain...>" (no dot), which
// the caller must treat as "cannot determine the domain" and skip creating a
// Route rather than guess.
func vanityHostDomainFromExistingRoute(existingHost string) string {
	existingHost = strings.TrimSpace(existingHost)
	i := strings.Index(existingHost, ".")
	if i < 0 || i == len(existingHost)-1 {
		return ""
	}
	return existingHost[i+1:]
}
