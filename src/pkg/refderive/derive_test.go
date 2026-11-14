package refderive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// techdocsIntake is what the cncf-techdocs-assessment model declares: the
// rendered headings of assessment-request.yml.
var techdocsIntake = workflowmodel.Intake{
	ReferenceRepoFields: []string{"Website or documentation repository", "Other repositories or sites in scope"},
	OutOfScopeFields:    []string{"Out of scope"},
}

// fixture395 is the trimmed body of cncf/techdocs#395.
func fixture395(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "techdocs-395.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// accepted395 is cncf/techdocs#395 as a maintainer accepted it.
func accepted395(t *testing.T) RequestIssue {
	return RequestIssue{
		Repo:                 "cncf/techdocs",
		Number:               395,
		Title:                "[Assessment request]: Hive Commons (pilot of the AI-assisted assessment workflow)",
		Body:                 fixture395(t),
		Labels:               []string{"Docs analysis", GrantLabel, "stage/020-claimed", "author/human"},
		AcceptedBy:           "nate-double-u",
		AcceptedByPermission: "maintain",
	}
}

func TestParseIssueForm(t *testing.T) {
	body := "preamble that is not a field\n### Project name\n\nHive Commons\n\n### Empty one\n\n_No response_\n\n### Multi\r\n\r\n- a\r\n- b\r\n### Last\nvalue"
	got := ParseIssueForm(body)
	want := []FormField{
		{Heading: "Project name", Value: "Hive Commons"},
		{Heading: "Empty one", Value: ""},
		{Heading: "Multi", Value: "- a\n- b"},
		{Heading: "Last", Value: "value"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields:\n got %+v\nwant %+v", got, want)
	}
	if got := ParseIssueForm(""); len(got) != 0 {
		t.Fatalf("empty body: %+v", got)
	}
}

func TestReposInText(t *testing.T) {
	cases := []struct {
		name, text, host string
		want             []string
	}{
		{"plain url", "https://github.com/hivecommons/docs", "", []string{"hivecommons/docs"}},
		{"url with path and prose", "see https://github.com/hivecommons/hive/blob/v5/README.md for details", "", []string{"hivecommons/hive"}},
		{"git suffix and trailing punctuation", "clone https://github.com/a/b.git, then https://github.com/c/d).", "", []string{"a/b", "c/d"}},
		{"www and http", "http://www.github.com/a/b", "", []string{"a/b"}},
		{"foreign host is text", "https://gitlab.com/a/b and https://example.com/c/d", "", nil},
		{"custom host", "https://ghe.example.com/a/b https://github.com/c/d", "ghe.example.com", []string{"a/b"}},
		{"bare slug whole line", "hivecommons/docs", "", []string{"hivecommons/docs"}},
		{"bare slug list item in backticks", "- `hivecommons/docs`\n* other/repo\n+ third/one", "", []string{"hivecommons/docs", "other/repo", "third/one"}},
		{"bare path inside a sentence is not a repo", "in-repo docs live under `src/docs/` and src/pkg here", "", nil},
		{"bare path with trailing slash is not a repo", "src/docs/", "", nil},
		{"dedupe case-insensitive keeps first", "https://github.com/HiveCommons/Docs\nhivecommons/docs", "", []string{"HiveCommons/Docs"}},
		{"not a slug", "github.com\ndocs.hivecommons.dev\n- [x] checked", "", nil},
		{"url in a list item with trailing note", "- https://github.com/hivecommons/hive — `src/docs/` (reference docs)", "", []string{"hivecommons/hive"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReposInText(tc.text, tc.host)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasWriteAccess(t *testing.T) {
	for perm, want := range map[string]bool{"admin": true, "Maintain": true, " write ": true, "triage": false, "read": false, "none": false, "": false} {
		if got := HasWriteAccess(perm); got != want {
			t.Errorf("HasWriteAccess(%q) = %v, want %v", perm, got, want)
		}
	}
}

func TestDeriveTechdocs395(t *testing.T) {
	res, err := Derive(accepted395(t), techdocsIntake, Options{Authorized: []string{"cncf/techdocs"}})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	wantRepos := []string{"hivecommons/docs", "hivecommons/hivecommons.github.io", "hivecommons/hive"}
	if len(res.Repos) != len(wantRepos) {
		t.Fatalf("want %d repos, got %+v", len(wantRepos), res.Repos)
	}
	for i, rr := range res.Repos {
		if rr.Repo != wantRepos[i] {
			t.Errorf("repo %d: %q, want %q", i, rr.Repo, wantRepos[i])
		}
		if !rr.WritesIssues() || !rr.WritesPRs() || rr.WriteLabel() != "issues+prs" {
			t.Errorf("%s: tier %q, want issues+prs", rr.Repo, rr.WriteLabel())
		}
		if rr.DerivedFrom != "cncf/techdocs#395" || !rr.Derived() {
			t.Errorf("%s: DerivedFrom %q", rr.Repo, rr.DerivedFrom)
		}
		if rr.WriteTokenFile != "" {
			t.Errorf("%s: a derived entry must never carry write_token_file", rr.Repo)
		}
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("nothing in #395 should be skipped, got %+v", res.Skipped)
	}
	// The derived list reads back through the same accessors a hand-written
	// one does.
	project := config.ProjectConfig{Org: "cncf", Repos: []string{"techdocs"}, ReferenceRepos: res.Repos}
	tier, ok := project.ReferenceRepoTier("HiveCommons/Hive")
	if !ok || !tier.Issues || !tier.PRs {
		t.Fatalf("tier lookup: ok=%v tier=%v", ok, tier)
	}
	if _, ok := project.ReferenceRepoTier("cncf/techdocs"); ok {
		t.Fatal("the authorized repo must never read as a reference repo")
	}
	for _, rr := range res.Repos {
		if rr.Owner() != "hivecommons" || rr.Name() == "" || strings.ContainsAny(rr.Repo, " :@") {
			t.Fatalf("%q is not an explicit owner/name slug", rr.Repo)
		}
	}
}

func TestDeriveRefuses(t *testing.T) {
	base := accepted395(t)
	cases := []struct {
		name   string
		mutate func(*RequestIssue)
		intake workflowmodel.Intake
		want   error
	}{
		{"no intake fields", func(*RequestIssue) {}, workflowmodel.Intake{OutOfScopeFields: []string{"Out of scope"}}, ErrNoIntakeFields},
		{"closed", func(r *RequestIssue) { r.Closed = true }, techdocsIntake, ErrClosed},
		{"label removed", func(r *RequestIssue) { r.Labels = []string{"Docs analysis"} }, techdocsIntake, ErrNotAccepted},
		{"applier unknown", func(r *RequestIssue) { r.AcceptedBy = " " }, techdocsIntake, ErrAccepterUnknown},
		{"applier is a bot", func(r *RequestIssue) { r.AcceptedBy = "hivecommons-hive[bot]"; r.AcceptedByIsBot = true }, techdocsIntake, ErrAccepterIsBot},
		{"applier has triage only", func(r *RequestIssue) { r.AcceptedByPermission = "triage" }, techdocsIntake, ErrAccepterNoWrite},
		{"applier permission unknown", func(r *RequestIssue) { r.AcceptedByPermission = "" }, techdocsIntake, ErrAccepterNoWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := base
			issue.Labels = append([]string(nil), base.Labels...)
			tc.mutate(&issue)
			res, err := Derive(issue, tc.intake, Options{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "cncf/techdocs#395") {
				t.Fatalf("refusal must name the issue: %v", err)
			}
			if len(res.Repos) != 0 {
				t.Fatalf("a refusal must derive nothing, got %+v", res.Repos)
			}
		})
	}
}

func TestDeriveSkipsAndDedupes(t *testing.T) {
	body := strings.Join([]string{
		"### Website or documentation repository",
		"",
		"https://github.com/acme/docs",
		"",
		"### Other repositories or sites in scope",
		"",
		"- https://github.com/ACME/docs (again, different case)",
		"- https://github.com/acme/site",
		"- https://github.com/acme/legacy",
		"- https://github.com/cncf/techdocs",
		"- https://github.com/cncf/toc",
		"",
		"### Out of scope",
		"",
		"- https://github.com/acme/legacy",
		"",
	}, "\n")
	issue := accepted395(t)
	issue.Body = body
	issue.Number = 7
	res, err := Derive(issue, techdocsIntake, Options{Authorized: []string{"cncf/techdocs", "CNCF/toc"}})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	gotRepos := make([]string, 0, len(res.Repos))
	for _, rr := range res.Repos {
		gotRepos = append(gotRepos, rr.Repo)
		if rr.DerivedFrom != "cncf/techdocs#7" {
			t.Errorf("%s: DerivedFrom %q", rr.Repo, rr.DerivedFrom)
		}
	}
	if want := []string{"acme/docs", "acme/site"}; !reflect.DeepEqual(gotRepos, want) {
		t.Fatalf("repos %v, want %v", gotRepos, want)
	}
	wantSkipped := []Skipped{
		{Repo: "acme/legacy", Reason: ReasonOutOfScope},
		{Repo: "cncf/techdocs", Reason: ReasonOwnRepo},
		{Repo: "cncf/toc", Reason: ReasonAuthorized},
	}
	if !reflect.DeepEqual(res.Skipped, wantSkipped) {
		t.Fatalf("skipped:\n got %+v\nwant %+v", res.Skipped, wantSkipped)
	}
}

func TestDeriveNothingNamedIsNotARefusal(t *testing.T) {
	issue := accepted395(t)
	issue.Body = "### Website or documentation repository\n\n_No response_\n\n### Other repositories or sites in scope\n\nOur docs live on https://docs.example.com only.\n"
	res, err := Derive(issue, techdocsIntake, Options{})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(res.Repos) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("want empty result, got %+v", res)
	}
}

func TestDeriveMatchesHeadingsCaseInsensitively(t *testing.T) {
	issue := accepted395(t)
	issue.Body = "### website or Documentation Repository \n\nhttps://github.com/acme/docs\n"
	res, err := Derive(issue, techdocsIntake, Options{Host: "GitHub.com"})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(res.Repos) != 1 || res.Repos[0].Repo != "acme/docs" {
		t.Fatalf("got %+v", res.Repos)
	}
}

func TestDerivedWriteIsNeverWiderThanIssuesAndPRs(t *testing.T) {
	known := map[string]bool{config.ReferenceWriteIssues: true, config.ReferenceWritePRs: true}
	for _, w := range DerivedWrite {
		if !known[w] {
			t.Fatalf("DerivedWrite names unknown write kind %q", w)
		}
	}
	tier := (config.ReferenceRepo{Write: DerivedWrite}).Tier()
	if !tier.Issues || !tier.PRs || tier.String() != "issues+prs" || len(DerivedWrite) != len(known) {
		t.Fatalf("DerivedWrite = %v, tier %v", DerivedWrite, tier)
	}
}

// TestIntakeSchema pins the workflow-model half: the field decodes from the
// model's JSON, an empty intake is omitted so existing models keep their
// hash, and a model without the field derives nothing.
func TestIntakeSchema(t *testing.T) {
	doc := `{"apiVersion":"workflow.hivecommons.dev/v1alpha1","kind":"WorkflowModel","metadata":{"name":"m"},
	  "spec":{"scope":{"forge":"github.com","repository":"cncf/techdocs"},"initial":"s","states":[{"id":"s","label":"S","owner":"human"}],
	  "intake":{"referenceRepoFields":["Website or documentation repository"],"outOfScopeFields":["Out of scope"]}}}`
	var m workflowmodel.Model
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Spec.Intake.DerivesReferenceRepos() || len(m.Spec.Intake.OutOfScopeFields) != 1 {
		t.Fatalf("intake: %+v", m.Spec.Intake)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a model with intake must validate: %v", err)
	}
	with := m.Hash()
	m.Spec.Intake = workflowmodel.Intake{}
	if m.Spec.Intake.DerivesReferenceRepos() {
		t.Fatal("empty intake must derive nothing")
	}
	without := m.Hash()
	if with == without {
		t.Fatal("declaring intake fields must change the spec hash")
	}
	raw, err := json.Marshal(m.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "intake") {
		t.Fatalf("empty intake must be omitted from the spec JSON so stored models keep their hash:\n%s", raw)
	}
}
