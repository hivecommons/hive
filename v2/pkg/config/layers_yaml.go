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

import "gopkg.in/yaml.v3"

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
