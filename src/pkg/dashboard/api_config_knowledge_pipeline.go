package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// knowledgePipelineBody is the PUT /api/config/knowledge/pipeline request:
// the scalar knowledge.bead_synthesizer, knowledge.curator,
// knowledge.code_maps and knowledge.primer keys. Every field is a pointer so
// an absent key leaves the config untouched; 0 or "" restores the default.
// Local vault paths and the engine are display-only
// and not accepted here.
type knowledgePipelineBody struct {
	BeadSynthesizer *struct {
		Enabled             *bool    `json:"enabled"`
		Schedule            *string  `json:"schedule"`
		MinConfidence       *float64 `json:"min_confidence"`
		TargetLayer         *string  `json:"target_layer"`
		MaxFactsPerCycle    *int     `json:"max_facts_per_cycle"`
		MaxAPICallsPerCycle *int     `json:"max_api_calls_per_cycle"`
		Publish             *struct {
			Enabled    *bool   `json:"enabled"`
			Repo       *string `json:"repo"`
			Path       *string `json:"path"`
			BaseBranch *string `json:"base_branch"`
			Mode       *string `json:"mode"`
			Cadence    *string `json:"cadence"`
		} `json:"publish"`
		RetentionPolicy *struct {
			MaxBeads               *int  `json:"max_beads"`
			ArchiveAfterSynthDays  *int  `json:"archive_after_synth_days"`
			HighPriorityRetainDays *int  `json:"high_priority_retain_days"`
			PreserveWithDeps       *bool `json:"preserve_with_deps"`
		} `json:"retention_policy"`
	} `json:"bead_synthesizer"`
	Curator *struct {
		Enabled              *bool    `json:"enabled"`
		Schedule             *string  `json:"schedule"`
		AutoPromoteThreshold *float64 `json:"auto_promote_threshold"`
		PromoteFrom          *string  `json:"promote_from"`
		PromoteTo            *string  `json:"promote_to"`
	} `json:"curator"`
	CodeMaps *struct {
		Enabled  *bool   `json:"enabled"`
		Schedule *string `json:"schedule"`
		Layer    *string `json:"layer"`
		MaxBytes *int    `json:"max_bytes"`
	} `json:"code_maps"`
	Primer *struct {
		MaxFacts *int `json:"max_facts"`
		MaxBytes *int `json:"max_bytes"`
	} `json:"primer"`
}

func trimLower(p *string) string { return strings.ToLower(strings.TrimSpace(*p)) }

// prepare validates the request with config.ValidateKnowledgePipeline and
// returns the change to apply. Only the keys the request carries are
// validated, so a value already in hive.yaml that this editor would reject
// does not block saving an unrelated field.
func (b knowledgePipelineBody) prepare(cur config.KnowledgeConfig) (func(*config.KnowledgeConfig), error) {
	var probe config.KnowledgeConfig
	var steps []func(*config.KnowledgeConfig)
	if bs := b.BeadSynthesizer; bs != nil {
		if bs.Enabled != nil {
			v := *bs.Enabled
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.Enabled = &v })
		}
		if bs.Schedule != nil {
			v := trimLower(bs.Schedule)
			probe.BeadSynthesizer.Schedule = v
			if v == "" {
				v = config.DefaultBeadSynthSchedule
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.Schedule = v })
		}
		if bs.MinConfidence != nil {
			v := *bs.MinConfidence
			probe.BeadSynthesizer.MinConfidence = v
			if v == 0 {
				v = config.DefaultBeadSynthMinConfidence
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.MinConfidence = v })
		}
		if bs.TargetLayer != nil {
			v := trimLower(bs.TargetLayer)
			probe.BeadSynthesizer.TargetLayer = v
			if v == "" {
				v = config.DefaultBeadSynthTargetLayer
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.TargetLayer = v })
		}
		if bs.MaxFactsPerCycle != nil {
			v := *bs.MaxFactsPerCycle
			probe.BeadSynthesizer.MaxFactsPerCycle = v
			if v == 0 {
				v = config.DefaultBeadSynthMaxFactsPerCycle
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.MaxFactsPerCycle = v })
		}
		if bs.MaxAPICallsPerCycle != nil {
			v := *bs.MaxAPICallsPerCycle
			probe.BeadSynthesizer.MaxAPICallsPerCycle = v
			if v == 0 {
				v = config.DefaultBeadSynthMaxAPICallsPerCycle
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.BeadSynthesizer.MaxAPICallsPerCycle = v })
		}
		if p := bs.Publish; p != nil {
			// Validate only carried fields; an unrelated legacy value must
			// not prevent a partial update. Nothing here publishes facts.
			if p.Path != nil {
				probe.BeadSynthesizer.Publish.Path = strings.TrimSpace(*p.Path)
			}
			if p.Mode != nil {
				probe.BeadSynthesizer.Publish.Mode = trimLower(p.Mode)
			}
			if p.Cadence != nil {
				probe.BeadSynthesizer.Publish.Cadence = trimLower(p.Cadence)
			}
			r := *p
			steps = append(steps, func(k *config.KnowledgeConfig) {
				v := &k.BeadSynthesizer.Publish
				if r.Enabled != nil {
					v.Enabled = *r.Enabled
				}
				if r.Repo != nil {
					v.Repo = strings.TrimSpace(*r.Repo)
				}
				if r.Path != nil {
					v.Path = strings.TrimSpace(*r.Path)
				}
				if r.BaseBranch != nil {
					v.BaseBranch = strings.TrimSpace(*r.BaseBranch)
				}
				if r.Mode != nil {
					v.Mode = trimLower(r.Mode)
				}
				if r.Cadence != nil {
					v.Cadence = trimLower(r.Cadence)
				}
				*v = v.WithDefaults()
			})
		}
		if rp := bs.RetentionPolicy; rp != nil {
			probe.BeadSynthesizer.RetentionPolicy = &config.RetentionPolicy{}
			if rp.MaxBeads != nil {
				probe.BeadSynthesizer.RetentionPolicy.MaxBeads = *rp.MaxBeads
			}
			if rp.ArchiveAfterSynthDays != nil {
				probe.BeadSynthesizer.RetentionPolicy.ArchiveAfterSynthDays = *rp.ArchiveAfterSynthDays
			}
			if rp.HighPriorityRetainDays != nil {
				probe.BeadSynthesizer.RetentionPolicy.HighPriorityRetainDays = *rp.HighPriorityRetainDays
			}
			r := *rp
			steps = append(steps, func(k *config.KnowledgeConfig) {
				// An absent block boots with preserve_with_deps on, so a block
				// created here starts from that rather than the zero value.
				if k.BeadSynthesizer.RetentionPolicy == nil {
					k.BeadSynthesizer.RetentionPolicy = &config.RetentionPolicy{PreserveWithDeps: true}
				}
				p := k.BeadSynthesizer.RetentionPolicy
				if r.MaxBeads != nil {
					p.MaxBeads = *r.MaxBeads
				}
				if r.ArchiveAfterSynthDays != nil {
					p.ArchiveAfterSynthDays = *r.ArchiveAfterSynthDays
				}
				if r.HighPriorityRetainDays != nil {
					p.HighPriorityRetainDays = *r.HighPriorityRetainDays
				}
				if r.PreserveWithDeps != nil {
					p.PreserveWithDeps = *r.PreserveWithDeps
				}
			})
		}
	}
	if c := b.Curator; c != nil {
		if c.Enabled != nil {
			v := *c.Enabled
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Curator.Enabled = &v })
		}
		if c.Schedule != nil {
			v := trimLower(c.Schedule)
			probe.Curator.Schedule = v
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Curator.Schedule = v })
		}
		if c.AutoPromoteThreshold != nil {
			v := *c.AutoPromoteThreshold
			probe.Curator.AutoPromoteThreshold = v
			if v == 0 {
				v = config.DefaultCuratorAutoPromoteThreshold
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Curator.AutoPromoteThreshold = v })
		}
		if c.PromoteFrom != nil || c.PromoteTo != nil {
			from, to := cur.Curator.PromoteFrom, cur.Curator.PromoteTo
			if c.PromoteFrom != nil {
				from = trimLower(c.PromoteFrom)
			}
			if c.PromoteTo != nil {
				to = trimLower(c.PromoteTo)
			}
			probe.Curator.PromoteFrom, probe.Curator.PromoteTo = from, to
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Curator.PromoteFrom, k.Curator.PromoteTo = from, to })
		}
	}
	if cm := b.CodeMaps; cm != nil {
		if cm.Enabled != nil {
			v := *cm.Enabled
			steps = append(steps, func(k *config.KnowledgeConfig) { k.CodeMaps.Enabled = &v })
		}
		// Code map zero values are already the defaults (hourly, project,
		// the generator's byte cap), so they are stored as-is.
		if cm.Schedule != nil {
			v := trimLower(cm.Schedule)
			probe.CodeMaps.Schedule = v
			steps = append(steps, func(k *config.KnowledgeConfig) { k.CodeMaps.Schedule = v })
		}
		if cm.Layer != nil {
			v := trimLower(cm.Layer)
			probe.CodeMaps.Layer = v
			steps = append(steps, func(k *config.KnowledgeConfig) { k.CodeMaps.Layer = v })
		}
		if cm.MaxBytes != nil {
			v := *cm.MaxBytes
			probe.CodeMaps.MaxBytes = v
			steps = append(steps, func(k *config.KnowledgeConfig) { k.CodeMaps.MaxBytes = v })
		}
	}
	if p := b.Primer; p != nil {
		if p.MaxFacts != nil {
			v := *p.MaxFacts
			probe.Primer.MaxFacts = v
			if v == 0 {
				v = config.DefaultKnowledgePrimerMaxFacts
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Primer.MaxFacts = v })
		}
		if p.MaxBytes != nil {
			v := *p.MaxBytes
			probe.Primer.MaxBytes = v
			if v == 0 {
				v = config.DefaultKnowledgePrimerMaxBytes
			}
			steps = append(steps, func(k *config.KnowledgeConfig) { k.Primer.MaxBytes = v })
		}
	}
	if err := config.ValidateKnowledgePipeline(probe); err != nil {
		return nil, err
	}
	return func(k *config.KnowledgeConfig) {
		for _, step := range steps {
			step(k)
		}
	}, nil
}

func knowledgePipelineResponse(cfg *config.Config) map[string]interface{} {
	k := cfg.Knowledge
	rp := config.RetentionPolicy{PreserveWithDeps: true}
	if k.BeadSynthesizer.RetentionPolicy != nil {
		rp = *k.BeadSynthesizer.RetentionPolicy
	}
	return map[string]interface{}{
		// Display-only: the engine picks the knowledge backend at boot.
		"engine": k.Engine,
		"bead_synthesizer": map[string]interface{}{
			"publish":                 k.BeadSynthesizer.Publish.WithDefaults(),
			"enabled":                 k.BeadSynthesizer.IsEnabled(),
			"schedule":                k.BeadSynthesizer.Schedule,
			"min_confidence":          k.BeadSynthesizer.MinConfidence,
			"target_layer":            k.BeadSynthesizer.TargetLayer,
			"max_facts_per_cycle":     k.BeadSynthesizer.MaxFactsPerCycle,
			"max_api_calls_per_cycle": k.BeadSynthesizer.MaxAPICallsPerCycle,
			// Display-only: a directory the hive creates and writes facts to.
			"vault_path": k.BeadSynthesizer.VaultPath,
			"retention_policy": map[string]interface{}{
				"max_beads":                 rp.MaxBeads,
				"archive_after_synth_days":  rp.ArchiveAfterSynthDays,
				"high_priority_retain_days": rp.HighPriorityRetainDays,
				"preserve_with_deps":        rp.PreserveWithDeps,
			},
		},
		"curator": map[string]interface{}{
			"enabled":                k.Curator.IsEnabled(),
			"schedule":               k.Curator.Schedule,
			"auto_promote_threshold": k.Curator.AutoPromoteThreshold,
			"promote_from":           k.Curator.PromoteFrom,
			"promote_to":             k.Curator.PromoteTo,
		},
		"code_maps": map[string]interface{}{
			"enabled":   k.CodeMaps.IsEnabled(),
			"schedule":  k.CodeMaps.Schedule,
			"layer":     k.CodeMaps.Layer,
			"max_bytes": k.CodeMaps.MaxBytes,
			// Display-only, like the bead synthesizer vault path.
			"vault_path": k.CodeMaps.VaultPath,
		},
		"primer": map[string]interface{}{
			"max_facts": k.Primer.MaxFacts,
			"max_bytes": k.Primer.MaxBytes,
			// Display-only: precedence is the only implemented strategy.
			"merge_strategy": k.Primer.MergeStrategy,
		},
		"schedules": config.KnowledgeSchedules,
		"layers":    []string{"personal", "project", "org", "community"},
		"defaults": map[string]interface{}{
			"bead_synthesizer_schedule":                config.DefaultBeadSynthSchedule,
			"bead_synthesizer_min_confidence":          config.DefaultBeadSynthMinConfidence,
			"bead_synthesizer_target_layer":            config.DefaultBeadSynthTargetLayer,
			"bead_synthesizer_max_facts_per_cycle":     config.DefaultBeadSynthMaxFactsPerCycle,
			"bead_synthesizer_max_api_calls_per_cycle": config.DefaultBeadSynthMaxAPICallsPerCycle,
			"bead_synthesizer_vault_path":              config.DefaultBeadSynthVaultPath,
			"curator_schedule":                         config.DefaultCuratorSchedule,
			"curator_auto_promote_threshold":           config.DefaultCuratorAutoPromoteThreshold,
			"curator_promote_from":                     "project",
			"curator_promote_to":                       "org",
			"primer_max_facts":                         config.DefaultKnowledgePrimerMaxFacts,
			"primer_max_bytes":                         config.DefaultKnowledgePrimerMaxBytes,
			"primer_merge_strategy":                    config.DefaultKnowledgePrimerMergeStrategy,
		},
	}
}

// handleKnowledgePipelineGet returns the scalar knowledge pipeline settings
// for Settings → Knowledge. OWNER-ONLY, like the rest of the config editors.
func (s *Server) handleKnowledgePipelineGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, knowledgePipelineResponse(s.deps.Config))
}

// handleKnowledgePipelinePut updates the scalar bead synthesizer, curator,
// code map and primer keys (owner-only). The request is validated before
// anything is applied, then persisted and audited. The bead synthesizer
// switch starts or stops the running synthesizer at once, as
// PUT /api/knowledge/bead-synthesizer/enabled does; every other key is read
// when the hive boots, so it applies after a restart.
func (s *Server) handleKnowledgePipelinePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body knowledgePipelineBody
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	apply, err := body.prepare(s.deps.Config.Knowledge)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	apply(&s.deps.Config.Knowledge)

	if bs := body.BeadSynthesizer; bs != nil && bs.Enabled != nil && s.deps.BeadSynthesizer != nil {
		if *bs.Enabled {
			s.deps.BeadSynthesizer.StartBackground(s.deps.Ctx)
		} else {
			s.deps.BeadSynthesizer.Stop()
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge pipeline update", "error", err)
	}
	s.auditFromRequest(r, "config_knowledge_pipeline", auditDetail("section", "knowledge_pipeline"), "")
	s.refreshAndPersist()
	jsonResponse(w, knowledgePipelineResponse(s.deps.Config))
}
