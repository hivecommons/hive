// Command workflow-survey ingests every repository in a corpus directory
// into a workflow model and writes, per repository, the model JSON, the
// Mermaid diagrams and the Activity Monitor columns, plus a corpus-wide
// summary. It is the proving harness for hivecommons/hive#17620.
//
// Corpus layout (as collected by the survey script): <corpus>/repos/
// <owner>__<repo>/<path...> with an index.json per repository.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/ingest"
	corpuslist "github.com/hivecommons/hive/pkg/workflowmodel/ingest/corpus"
	"github.com/hivecommons/hive/pkg/workflowmodel/render"
)

const (
	// worstN is how many repositories the summary lists by finding count.
	worstN = 20
	// topN is how many repositories the summary lists by pipeline count.
	topN = 20
	// repoDirSeparator joins owner and repo in a corpus directory name.
	repoDirSeparator = "__"
	// defaultForge is recorded in every model's scope.
	defaultForge = "github.com"
	// exitUsage and exitFailure are the process exit codes.
	exitUsage   = 2
	exitFailure = 1
)

// repoIndex is the per-repository index the collector writes.
type repoIndex struct {
	Repo    string `json:"repo"`
	Project string `json:"project,omitempty"`
	Files   []struct {
		Path string `json:"path"`
		Tag  string `json:"tag,omitempty"`
	} `json:"files"`
}

// repoResult is one row of the summary.
type repoResult struct {
	Repo           string                         `json:"repo"`
	Project        string                         `json:"project,omitempty"`
	Files          int                            `json:"files"`
	Parsed         int                            `json:"parsed"`
	Pipelines      int                            `json:"pipelines"`
	GHAPipelines   int                            `json:"ghaPipelines"`
	TektonPipeline int                            `json:"tektonPipelines"`
	Roles          map[workflowmodel.Role]int     `json:"roles"`
	Findings       int                            `json:"findings"`
	FindingCodes   map[string]int                 `json:"findingCodes,omitempty"`
	Constructs     map[string]int                 `json:"constructs"`
	Columns        []string                       `json:"columns"`
	ColumnDiff     []string                       `json:"columnDiff,omitempty"`
	CISubstages    int                            `json:"ciSubstages"`
	MaxLevels      int                            `json:"maxLevels"`
	MaxNodes       int                            `json:"maxNodes"`
	Valid          bool                           `json:"valid"`
	ValidateError  string                         `json:"validateError,omitempty"`
	Hash           string                         `json:"hash"`
	Severities     map[workflowmodel.Severity]int `json:"severities,omitempty"`
	// Compiled says whether the lifecycle model (ingest.Lifecycle) compiles;
	// CompileCodes counts its diagnostics by code, errors and warnings.
	Compiled      bool           `json:"compiled"`
	CompileError  string         `json:"compileError,omitempty"`
	CompileCodes  map[string]int `json:"compileCodes,omitempty"`
	LifecycleCols int            `json:"lifecycleColumns"`
	// Intake is what the P5 intake half proposed from the repository's
	// process conventions (issue/PR templates, OWNERS, branch rules).
	Intake intakeStats `json:"intake"`
}

// intakeStats counts the intake-half proposals in one lifecycle model.
type intakeStats struct {
	IntakeStates   int  `json:"intakeStates"`
	Triage         bool `json:"triage"`
	MemberRoles    int  `json:"memberRoles"`
	InferredGates  int  `json:"inferredGates"`
	ManualChecks   int  `json:"manualChecks"`
	CIChecks       int  `json:"ciChecks"`
	BranchRules    bool `json:"branchRules"`
	IntakeFindings int  `json:"intakeFindings"`
}

type summary struct {
	Repos            int                            `json:"repos"`
	ReposWithGHA     int                            `json:"reposWithGHA"`
	ReposWithTekton  int                            `json:"reposWithTekton"`
	Files            int                            `json:"files"`
	Parsed           int                            `json:"parsed"`
	Pipelines        int                            `json:"pipelines"`
	Valid            int                            `json:"valid"`
	Compiled         int                            `json:"compiled"`
	CompileCodes     map[string]int                 `json:"compileCodes"`
	Roles            map[workflowmodel.Role]int     `json:"roles"`
	Constructs       map[string]int                 `json:"constructs"`
	FindingCodes     map[string]int                 `json:"findingCodes"`
	Severities       map[workflowmodel.Severity]int `json:"severities"`
	ColumnSets       map[string]int                 `json:"columnSets"`
	ReposChangedCols int                            `json:"reposWithChangedColumns"`
	// Intake counts repos where the intake half proposed each element.
	Intake    intakeSummary            `json:"intake"`
	ByProject map[string]*projectStats `json:"byProject"`
	Worst     []repoResult             `json:"worst"`
	Top       []repoResult             `json:"top"`
	Repos_    []repoResult             `json:"results"`
}

// intakeSummary counts repositories, not elements.
type intakeSummary struct {
	ReposWithIntakeStates int `json:"reposWithIntakeStates"`
	ReposWithTriage       int `json:"reposWithTriage"`
	ReposWithRoles        int `json:"reposWithRoles"`
	ReposWithGates        int `json:"reposWithGates"`
	ReposWithChecks       int `json:"reposWithChecks"`
	ReposWithBranchRules  int `json:"reposWithBranchRules"`
}

type projectStats struct {
	Repos     int `json:"repos"`
	Pipelines int `json:"pipelines"`
	Findings  int `json:"findings"`
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain parses args and runs the survey, returning the exit code. It
// is separated from main so tests can drive the binary in-process.
func realMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow-survey", flag.ContinueOnError)
	fs.SetOutput(stderr)
	corpus := fs.String("corpus", "", "corpus root (contains repos/)")
	out := fs.String("out", "", "output root (default <corpus>/out)")
	only := fs.String("only", "", "comma-separated owner/repo filter")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *corpus == "" {
		fmt.Fprintln(stderr, "usage: workflow-survey -corpus DIR [-out DIR] [-only owner/repo,...]")
		return exitUsage
	}
	if *out == "" {
		*out = filepath.Join(*corpus, "out")
	}
	filter := map[string]bool{}
	for _, r := range strings.Split(*only, ",") {
		if r = strings.TrimSpace(r); r != "" {
			filter[r] = true
		}
	}
	if err := run(*corpus, *out, filter, stdout, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailure
	}
	return 0
}

// loadProjects maps owner/repo to project maturity from the collector's
// projects.json, when present.
// loadProjects reads <corpus>/projects.json; when the corpus directory
// carries none it falls back to the embedded CNCF list (#17620 phase 7).
func loadProjects(corpus string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(corpus, "projects.json"))
	if err != nil {
		return corpuslist.Maturity()
	}
	var projects []struct {
		Project string   `json:"project"`
		Repos   []string `json:"repos"`
	}
	if err := json.Unmarshal(b, &projects); err != nil {
		return out
	}
	for _, p := range projects {
		for _, r := range p.Repos {
			out[strings.ToLower(r)] = p.Project
		}
	}
	return out
}

func run(corpus, out string, filter map[string]bool, stdout, stderr io.Writer) error {
	dirs, err := os.ReadDir(filepath.Join(corpus, "repos"))
	if err != nil {
		return err
	}
	maturity := loadProjects(corpus)
	sum := &summary{Roles: map[workflowmodel.Role]int{}, Constructs: map[string]int{}, FindingCodes: map[string]int{}, Severities: map[workflowmodel.Severity]int{}, CompileCodes: map[string]int{}, ColumnSets: map[string]int{}, ByProject: map[string]*projectStats{}}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		repo := strings.Replace(d.Name(), repoDirSeparator, "/", 1)
		if len(filter) > 0 && !filter[repo] {
			continue
		}
		res, err := surveyRepo(filepath.Join(corpus, "repos", d.Name()), repo, filepath.Join(out, d.Name()))
		if res.Project == "" {
			res.Project = maturity[strings.ToLower(repo)]
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", repo, err)
			continue
		}
		fmt.Fprintf(stdout, "%-50s files=%-3d parsed=%-3d pipelines=%-3d findings=%-3d cols=%d%s\n", repo, res.Files, res.Parsed, res.Pipelines, res.Findings, len(res.Columns), ternary(res.Valid, "", " INVALID"))
		accumulate(sum, res)
	}
	finish(sum)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "summary.json"), sum); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "summary.md"), []byte(markdown(sum)), 0o644)
}

func surveyRepo(dir, repo, outDir string) (repoResult, error) {
	var idx repoIndex
	if b, err := os.ReadFile(filepath.Join(dir, "index.json")); err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "index.json" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return repoResult{}, err
	}
	m := ingest.Repository(workflowmodel.Scope{Forge: defaultForge, Repository: repo}, files)
	pipelines := m.Spec.Pipelines
	cov := m.Spec.Coverage
	var ghaCount, tknCount int
	for _, p := range pipelines {
		if p.System == workflowmodel.SystemTekton {
			tknCount++
		} else {
			ghaCount++
		}
	}

	res := repoResult{Repo: repo, Project: idx.Project, Files: cov.Files, Parsed: cov.Parsed, Pipelines: len(pipelines), GHAPipelines: ghaCount, TektonPipeline: tknCount, Roles: map[workflowmodel.Role]int{}, FindingCodes: map[string]int{}, Constructs: map[string]int{}, Severities: map[workflowmodel.Severity]int{}, Findings: len(cov.Findings), Hash: m.Hash()}
	if err := m.Validate(); err != nil {
		res.ValidateError = err.Error()
	} else {
		res.Valid = true
	}
	cov = m.Spec.Coverage
	res.Findings = len(cov.Findings)
	for _, f := range cov.Findings {
		res.FindingCodes[f.Code]++
		res.Severities[f.Severity]++
	}
	for _, p := range pipelines {
		res.Roles[p.Role]++
		countConstructs(res.Constructs, p)
		if p.Levels > res.MaxLevels {
			res.MaxLevels = p.Levels
		}
		if len(p.Nodes) > res.MaxNodes {
			res.MaxNodes = len(p.Nodes)
		}
	}
	cols := render.Columns(m)
	for _, c := range cols {
		res.Columns = append(res.Columns, c.Label)
		res.CISubstages += len(c.Substages)
	}
	res.ColumnDiff = render.Diff(m)

	lm := ingest.Lifecycle(workflowmodel.Scope{Forge: defaultForge, Repository: repo}, files)
	res.LifecycleCols = len(lm.Spec.Columns)
	res.CompileCodes = map[string]int{}
	_, diags, cerr := workflowmodel.Compile(lm)
	for _, d := range diags {
		res.CompileCodes[d.Code]++
	}
	if cerr != nil {
		res.CompileError = cerr.Error()
	} else {
		res.Compiled = true
	}
	res.Intake = countIntake(lm)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return res, err
	}
	if err := writeJSON(filepath.Join(outDir, "lifecycle.json"), lm); err != nil {
		return res, err
	}
	if err := writeJSON(filepath.Join(outDir, "model.json"), m); err != nil {
		return res, err
	}
	if err := writeJSON(filepath.Join(outDir, "columns.json"), cols); err != nil {
		return res, err
	}
	var mm strings.Builder
	mm.WriteString("```mermaid\n" + render.Mermaid(m) + "```\n")
	for i := range m.Spec.Pipelines {
		mm.WriteString("\n```mermaid\n" + render.PipelineMermaid(&m.Spec.Pipelines[i]) + "```\n")
	}
	if err := os.WriteFile(filepath.Join(outDir, "diagram.md"), []byte(mm.String()), 0o644); err != nil {
		return res, err
	}
	return res, writeJSON(filepath.Join(outDir, "result.json"), res)
}

// countIntake tallies the intake-half proposals in a lifecycle model.
func countIntake(m *workflowmodel.Model) intakeStats {
	var st intakeStats
	for _, s := range m.Spec.States {
		switch {
		case s.ID == ingest.StateTriage:
			st.Triage = true
		case strings.HasPrefix(s.ID, ingest.StateRequestedPrefix):
			st.IntakeStates++
		}
	}
	for _, r := range m.Spec.Roles {
		if r.Kind == workflowmodel.RoleMembers {
			st.MemberRoles++
		}
	}
	for _, g := range m.Spec.Gates {
		if g.Confidence == workflowmodel.ConfidenceInferred {
			st.InferredGates++
		}
	}
	for _, c := range m.Spec.Checks {
		switch c.Kind {
		case workflowmodel.CheckManual:
			st.ManualChecks++
		case workflowmodel.CheckCI:
			st.CIChecks++
			st.BranchRules = true
		}
	}
	for _, g := range m.Spec.Gates {
		if g.ID == ingest.GateCodeownerReview || (g.ID == ingest.GateReview && g.MinApprovals > 0) {
			st.BranchRules = true
		}
	}
	st.BranchRules = st.BranchRules || len(m.Spec.Bookkeeping) > 0
	for _, f := range m.Spec.Coverage.Findings {
		if strings.HasPrefix(f.Code, "issue-template-") {
			st.IntakeFindings++
		}
	}
	return st
}

func countConstructs(c map[string]int, p workflowmodel.Pipeline) {
	c["pipelines"]++
	if p.Concurrency != nil {
		c["concurrency"]++
	}
	for _, t := range p.Triggers {
		c["trigger:"+t.Event]++
		if len(t.Paths) > 0 {
			c["trigger-paths"]++
		}
	}
	for _, n := range p.Nodes {
		c["nodes"]++
		c["node:"+string(n.Kind)]++
		if len(n.Needs) > 0 {
			c["needs"]++
		}
		if n.Matrix != nil {
			c["matrix"]++
			if n.Matrix.Dynamic {
				c["matrix-dynamic"]++
			}
			if n.Matrix.Include > 0 || n.Matrix.Exclude > 0 {
				c["matrix-include-exclude"]++
			}
		}
		if n.Environment != "" {
			c["environment"]++
		}
		if n.If != "" {
			c["if"]++
		}
		if len(n.When) > 0 {
			c["when"]++
		}
	}
}

func accumulate(s *summary, r repoResult) {
	s.Repos++
	s.Files += r.Files
	s.Parsed += r.Parsed
	s.Pipelines += r.Pipelines
	if r.GHAPipelines > 0 {
		s.ReposWithGHA++
	}
	if r.TektonPipeline > 0 {
		s.ReposWithTekton++
	}
	if r.Valid {
		s.Valid++
	}
	if r.Compiled {
		s.Compiled++
	}
	for k, v := range r.CompileCodes {
		s.CompileCodes[k] += v
	}
	if len(r.ColumnDiff) > 0 {
		s.ReposChangedCols++
	}
	if r.Intake.IntakeStates > 0 {
		s.Intake.ReposWithIntakeStates++
	}
	if r.Intake.Triage {
		s.Intake.ReposWithTriage++
	}
	if r.Intake.MemberRoles > 0 {
		s.Intake.ReposWithRoles++
	}
	if r.Intake.InferredGates > 0 {
		s.Intake.ReposWithGates++
	}
	if r.Intake.ManualChecks+r.Intake.CIChecks > 0 {
		s.Intake.ReposWithChecks++
	}
	if r.Intake.BranchRules {
		s.Intake.ReposWithBranchRules++
	}
	for k, v := range r.Roles {
		s.Roles[k] += v
	}
	for k, v := range r.Constructs {
		s.Constructs[k] += v
	}
	for k, v := range r.FindingCodes {
		s.FindingCodes[k] += v
	}
	for k, v := range r.Severities {
		s.Severities[k] += v
	}
	s.ColumnSets[strings.Join(r.Columns, " | ")]++
	proj := r.Project
	if proj == "" {
		proj = "unknown"
	}
	ps := s.ByProject[proj]
	if ps == nil {
		ps = &projectStats{}
		s.ByProject[proj] = ps
	}
	ps.Repos++
	ps.Pipelines += r.Pipelines
	ps.Findings += r.Findings
	s.Repos_ = append(s.Repos_, r)
}

func finish(s *summary) {
	sort.Slice(s.Repos_, func(i, j int) bool { return s.Repos_[i].Repo < s.Repos_[j].Repo })
	worst := append([]repoResult(nil), s.Repos_...)
	sort.SliceStable(worst, func(i, j int) bool {
		if worst[i].Valid != worst[j].Valid {
			return !worst[i].Valid
		}
		return worst[i].Findings > worst[j].Findings
	})
	s.Worst = worst[:min(worstN, len(worst))]
	top := append([]repoResult(nil), s.Repos_...)
	sort.SliceStable(top, func(i, j int) bool { return top[i].Pipelines > top[j].Pipelines })
	s.Top = top[:min(topN, len(top))]
}

func markdown(s *summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Workflow model survey\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Repositories | %d |\n| with GitHub Actions pipelines | %d |\n| with Tekton pipelines | %d |\n", s.Repos, s.ReposWithGHA, s.ReposWithTekton)
	fmt.Fprintf(&b, "| Files read | %d |\n| Files parsed | %d (%.1f%%) |\n| Pipelines | %d |\n| Models valid | %d / %d |\n| Lifecycle models compiled | %d / %d |\n| Repos whose columns change | %d |\n\n", s.Files, s.Parsed, pct(s.Parsed, s.Files), s.Pipelines, s.Valid, s.Repos, s.Compiled, s.Repos, s.ReposChangedCols)
	b.WriteString("## Lifecycle compile diagnostics\n\n| Code | Count |\n|---|---|\n")
	for _, k := range sortedKeys(s.CompileCodes) {
		fmt.Fprintf(&b, "| %s | %d |\n", k, s.CompileCodes[k])
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "## Intake proposals (RFC phase 5)\n\n| Proposed | Repos |\n|---|---|\n| Intake states (issue templates) | %d |\n| Triage state | %d |\n| Member roles (CODEOWNERS/OWNERS) | %d |\n| Inferred gates | %d |\n| Checks (PR template / required status) | %d |\n| Branch rules | %d |\n\n",
		s.Intake.ReposWithIntakeStates, s.Intake.ReposWithTriage, s.Intake.ReposWithRoles, s.Intake.ReposWithGates, s.Intake.ReposWithChecks, s.Intake.ReposWithBranchRules)
	b.WriteString("## Pipeline roles\n\n| Role | Pipelines |\n|---|---|\n")
	for _, k := range sortedKeys(s.Roles) {
		fmt.Fprintf(&b, "| %s | %d |\n", k, s.Roles[workflowmodel.Role(k)])
	}
	b.WriteString("\n## Constructs seen\n\n| Construct | Count |\n|---|---|\n")
	for _, k := range sortedKeys(s.Constructs) {
		fmt.Fprintf(&b, "| %s | %d |\n", k, s.Constructs[k])
	}
	b.WriteString("\n## Findings by code\n\n| Code | Count |\n|---|---|\n")
	for _, k := range sortedKeys(s.FindingCodes) {
		fmt.Fprintf(&b, "| %s | %d |\n", k, s.FindingCodes[k])
	}
	b.WriteString("\n## Findings by severity\n\n| Severity | Count |\n|---|---|\n")
	for _, k := range sortedKeys(s.Severities) {
		fmt.Fprintf(&b, "| %s | %d |\n", k, s.Severities[workflowmodel.Severity(k)])
	}
	b.WriteString("\n## Column sets (Activity Monitor)\n\n| Repos | Columns |\n|---|---|\n")
	type kv struct {
		k string
		v int
	}
	var sets []kv
	for k, v := range s.ColumnSets {
		sets = append(sets, kv{k, v})
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].v > sets[j].v || (sets[i].v == sets[j].v && sets[i].k < sets[j].k) })
	for _, e := range sets {
		fmt.Fprintf(&b, "| %d | %s |\n", e.v, e.k)
	}
	b.WriteString("\n## By project maturity\n\n| Maturity | Repos | Pipelines | Findings |\n|---|---|---|---|\n")
	for _, k := range sortedKeys(s.ByProject) {
		p := s.ByProject[k]
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", k, p.Repos, p.Pipelines, p.Findings)
	}
	fmt.Fprintf(&b, "\n## Top %d repositories by pipelines\n\n| Repo | Pipelines | Max nodes | Max levels | CI substages | Columns |\n|---|---|---|---|---|---|\n", topN)
	for _, r := range s.Top {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s |\n", r.Repo, r.Pipelines, r.MaxNodes, r.MaxLevels, r.CISubstages, strings.Join(r.Columns, " › "))
	}
	fmt.Fprintf(&b, "\n## Worst %d repositories by findings\n\n| Repo | Valid | Findings | Codes |\n|---|---|---|---|\n", worstN)
	for _, r := range s.Worst {
		var codes []string
		for _, k := range sortedKeys(r.FindingCodes) {
			codes = append(codes, fmt.Sprintf("%s×%d", k, r.FindingCodes[k]))
		}
		fmt.Fprintf(&b, "| %s | %v | %d | %s |\n", r.Repo, r.Valid, r.Findings, strings.Join(codes, ", "))
	}
	return b.String()
}

func sortedKeys[V any, K ~string](m map[K]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) * 100 / float64(d)
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
