package compliance

import (
	"os"

	"github.com/hivecommons/hive/pkg/config"
)

// ControlStatus is one control's evaluated state against a live config.
type ControlStatus struct {
	Framework  string          `json:"framework"`
	ControlID  string          `json:"control_id"`
	Title      string          `json:"title"`
	Citation   string          `json:"citation"`
	Domain     string          `json:"domain"`
	Status     Status          `json:"status"`
	NotCovered bool            `json:"not_covered,omitempty"`
	Rationale  string          `json:"rationale,omitempty"`
	Settings   []SettingStatus `json:"settings,omitempty"`
}

// SettingStatus is one mapping's current vs recommended value.
type SettingStatus struct {
	SettingPath string `json:"setting_path"`
	Evaluator   string `json:"evaluator"`
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
	Status      Status `json:"status"`
	// Builtin marks behaviour fixed in code rather than an operator setting.
	Builtin bool `json:"builtin,omitempty"`
}

// Summary counts controls by status.
type Summary struct {
	Meets      int `json:"meets"`
	Deviates   int `json:"deviates"`
	Off        int `json:"off"`
	NotCovered int `json:"not_covered"`
}

// Evaluate evaluates every framework selected in cfg.Compliance against cfg,
// reading env-backed settings from the process environment. Unknown
// framework IDs (rejected by config validation) are skipped. A nil cfg yields
// nil.
func Evaluate(cfg *config.Config) []ControlStatus {
	return EvaluateWith(cfg, os.Getenv)
}

// EvaluateWith is Evaluate with an injectable environment lookup.
func EvaluateWith(cfg *config.Config, getenv func(string) string) []ControlStatus {
	if cfg == nil {
		return nil
	}
	var out []ControlStatus
	for _, id := range cfg.Compliance.SelectedFrameworks() {
		p, ok := ProfileByID(id)
		if !ok {
			continue
		}
		out = append(out, EvaluateProfile(p, cfg, getenv)...)
	}
	return out
}

// EvaluateProfile evaluates one profile's controls against cfg, in profile
// order. A control meets when every mapping meets, is off when every mapping
// is off, and otherwise deviates.
func EvaluateProfile(p Profile, cfg *config.Config, getenv func(string) string) []ControlStatus {
	if cfg == nil {
		cfg = &config.Config{}
	}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	out := make([]ControlStatus, 0, len(p.Controls))
	for _, c := range p.Controls {
		cs := ControlStatus{
			Framework:  p.ID,
			ControlID:  c.ID,
			Title:      c.Title,
			Citation:   c.Citation,
			Domain:     c.Domain,
			NotCovered: c.NotCovered,
			Rationale:  c.Rationale,
		}
		if c.NotCovered {
			cs.Status = StatusNotCovered
			out = append(out, cs)
			continue
		}
		meets, off := 0, 0
		for _, m := range c.Mappings {
			ss := evaluateMapping(m, cfg, getenv)
			switch ss.Status {
			case StatusMeets:
				meets++
			case StatusOff:
				off++
			}
			cs.Settings = append(cs.Settings, ss)
		}
		switch {
		case meets == len(cs.Settings):
			cs.Status = StatusMeets
		case off == len(cs.Settings):
			cs.Status = StatusOff
		default:
			cs.Status = StatusDeviates
		}
		out = append(out, cs)
	}
	return out
}

func evaluateMapping(m Mapping, cfg *config.Config, getenv func(string) string) SettingStatus {
	ss := SettingStatus{SettingPath: m.SettingPath, Evaluator: m.Evaluator, Recommended: m.Recommended}
	s, okS := settings[m.SettingPath]
	ev, okE := evaluators[m.Evaluator]
	if !okS || !okE {
		// Unreachable for validated profiles; fail toward "deviates" so a
		// broken mapping can never report a pass.
		ss.Status, ss.Current = StatusDeviates, "unknown setting or evaluator"
		return ss
	}
	ss.Builtin = s.Builtin
	ss.Status, ss.Current = ev.eval(s.read(cfg, getenv), m.Recommended)
	return ss
}

// Summarize counts statuses.
func Summarize(statuses []ControlStatus) Summary {
	var s Summary
	for _, c := range statuses {
		switch c.Status {
		case StatusMeets:
			s.Meets++
		case StatusOff:
			s.Off++
		case StatusNotCovered:
			s.NotCovered++
		default:
			s.Deviates++
		}
	}
	return s
}

// FrameworkInfo identifies one shipped profile.
type FrameworkInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Controls int    `json:"controls"`
}

// Report is the full evaluated view of a config, as served by
// GET /api/compliance/status.
type Report struct {
	Disclaimer            string          `json:"disclaimer"`
	Frameworks            []string        `json:"frameworks"`
	UnknownFrameworks     []string        `json:"unknown_frameworks,omitempty"`
	Available             []FrameworkInfo `json:"available"`
	PostureCheckInterval  string          `json:"posture_check_interval"`
	Summary               Summary         `json:"summary"`
	Controls              []ControlStatus `json:"controls"`
	ProfileLoadError      string          `json:"profile_load_error,omitempty"`
	PostureChecksPending  bool            `json:"posture_checks_pending"`
	PostureChecksTracking string          `json:"posture_checks_tracking,omitempty"`
}

// BuildReport evaluates cfg into a Report. It never fails: a profile load
// error is reported in the body rather than hiding every status.
func BuildReport(cfg *config.Config, getenv func(string) string) Report {
	if cfg == nil {
		cfg = &config.Config{}
	}
	r := Report{
		Disclaimer:           Disclaimer,
		Frameworks:           []string{},
		Available:            []FrameworkInfo{},
		Controls:             []ControlStatus{},
		PostureCheckInterval: cfg.Compliance.PostureIntervalOrDefault().String(),
		// Posture checks run (hivecommons/hive#11079); results are served by
		// GET /api/compliance/posture. The fields stay for API compatibility.
		PostureChecksPending: false,
	}
	ps, err := Profiles()
	if err != nil {
		r.ProfileLoadError = err.Error()
	}
	for _, p := range ps {
		r.Available = append(r.Available, FrameworkInfo{ID: p.ID, Name: p.Name, Version: p.Version, Controls: len(p.Controls)})
	}
	for _, id := range cfg.Compliance.SelectedFrameworks() {
		if _, ok := ProfileByID(id); ok {
			r.Frameworks = append(r.Frameworks, id)
		} else {
			r.UnknownFrameworks = append(r.UnknownFrameworks, id)
		}
	}
	if st := EvaluateWith(cfg, getenv); st != nil {
		r.Controls = st
	}
	r.Summary = Summarize(r.Controls)
	return r
}
