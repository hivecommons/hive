package config

// Exact-fidelity merge entry points.
//
// MergeLayers operates on parsed *Config values, which is convenient but loses
// one distinction the entrypoint heredoc depends on: whether a KEY WAS PRESENT
// in the YAML at all.
//
// The heredoc gates the hub.is_public inversion on key presence:
//
//	if isinstance(seed.get('hub'), dict) and 'is_public' in seed['hub']:
//	    overlay.setdefault('hub', {})['is_public'] = seed['hub']['is_public']
//
// HubConfig.IsPublic is a plain bool, so after parsing, "key absent" and
// "is_public: false" are both false and indistinguishable. A Go transcription
// that ignores this differs from the shell in exactly one case:
//
//	seed omits hub.is_public  AND  overlay sets it true
//	  → heredoc keeps the overlay's true
//	  → naive Go applies the seed's false
//
// That is a silent visibility flip on whichever hives happen to hit it. This
// PR exists to END divergence between the documented rule and the running
// behaviour, so shipping a known divergence inside it would be
// self-defeating. MergeLayersYAML parses the raw seed YAML to recover
// key-presence and reproduces the heredoc exactly.

import (
	"log"

	"gopkg.in/yaml.v3"
)

// MergeLayersYAML merges raw seed and overlay YAML with exact fidelity to the
// entrypoint heredoc, returning the merged config and its provenance.
//
// overlayYAML may be empty, meaning no overlay on disk: the seed is used as-is,
// exactly as at boot.
//
// This is the entry point the boot path should use. MergeLayers remains for
// callers that already hold parsed configs and accept the documented
// is_public caveat.
func MergeLayersYAML(seedYAML, overlayYAML []byte) (*Config, *Provenance, error) {
	var seed Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(seedYAML))), &seed); err != nil {
		return nil, nil, err
	}

	keys := seedKeys{isPublicPresent: seedHasIsPublic(seedYAML)}

	if len(overlayYAML) == 0 {
		cfg, prov := mergeLayers(&seed, nil, keys)
		return cfg, prov, nil
	}

	var overlay Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(overlayYAML))), &overlay); err != nil {
		// A malformed overlay is treated as absent, matching the entrypoint's
		// behaviour of falling back to the seed rather than failing the boot.
		cfg, prov := mergeLayers(&seed, nil, keys)
		prov.OverlayRejected = true
		prov.OverlayRejectReason = "overlay is not valid YAML: " + err.Error()
		return cfg, prov, nil
	}

	cfg, prov := mergeLayers(&seed, &overlay, keys)
	return cfg, prov, nil
}

// MergeLayersWithFallback is MergeLayersYAML plus a last-good fallback: when
// the overlay is rejected, it prefers lastGoodYAML (normally the rolling
// backup at /data/hive.yaml.bak) over the bare ConfigMap seed.
//
// ─────────────────────────────────────────────────────────────────────────
// WHY, AND WHY THIS IS OPT-IN
// ─────────────────────────────────────────────────────────────────────────
//
// Falling back to the seed is failing EMPTY, not failing safe. Live seeds are
// 4–11% of the running config and omit policies, data, knowledge,
// notifications and hive_id entirely, so a rejected overlay currently costs a
// hive most of its configuration. The rolling backup is a far closer
// approximation of what the hive was actually running a moment ago.
//
// This CHANGES WHICH CONFIG A HIVE BOOTS WITH in the rejection case, so it is
// deliberately a separate entry point rather than folded into MergeLayersYAML.
// Callers opt in explicitly and the change is reviewable on its own. The
// fallback is used ONLY when the overlay is rejected AND the backup itself
// passes the guard — a corrupt backup is never preferred to a valid seed.
//
// Provenance reports LastGoodUsed so the substitution is never silent.
func MergeLayersWithFallback(seedYAML, overlayYAML, lastGoodYAML []byte) (*Config, *Provenance, error) {
	cfg, prov, err := MergeLayersYAML(seedYAML, overlayYAML)
	if err != nil || prov == nil || !prov.OverlayRejected || len(lastGoodYAML) == 0 {
		return cfg, prov, err
	}

	var lastGood Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(lastGoodYAML))), &lastGood); err != nil {
		// An unparsable backup is no better than the seed; keep the seed.
		return cfg, prov, nil
	}
	if reason := overlayRejectReason(&lastGood); reason != "" {
		// The backup is itself implausible — do not trade a valid seed for it.
		return cfg, prov, nil
	}

	log.Printf("WARNING: dashboard overlay rejected (%s) — recovering from the last-good "+
		"config instead of the sparse ConfigMap seed", prov.OverlayRejectReason)

	keys := seedKeys{isPublicPresent: seedHasIsPublic(seedYAML)}
	var seed Config
	if err := yaml.Unmarshal([]byte(expandEnvVars(string(seedYAML))), &seed); err != nil {
		return cfg, prov, nil
	}
	merged, mergedProv := mergeLayers(&seed, &lastGood, keys)
	mergedProv.OverlayRejected = true
	mergedProv.OverlayRejectReason = prov.OverlayRejectReason
	mergedProv.LastGoodUsed = true
	return merged, mergedProv, nil
}

// seedHasIsPublic reports whether the raw seed YAML contains the hub.is_public
// key, regardless of its value. This is the fact that parsing into a bool
// destroys.
func seedHasIsPublic(seedYAML []byte) bool {
	var probe struct {
		Hub map[string]yaml.Node `yaml:"hub"`
	}
	if err := yaml.Unmarshal(seedYAML, &probe); err != nil {
		// Unparsable seed: assume present, which is the behaviour every
		// provisioned seed on the fleet exhibits (the provisioning template
		// always emits hub.is_public).
		return true
	}
	_, ok := probe.Hub["is_public"]
	return ok
}
