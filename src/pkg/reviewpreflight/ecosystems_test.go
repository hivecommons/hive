package reviewpreflight

import (
	"reflect"
	"strings"
	"testing"
)

// Each case is one table entry's parser against a realistic patch. Adding an
// ecosystem means adding a case here (docs/review-swarm.md, "Review evidence").
func TestEcosystemParsers(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		patch string
		want  []string // "ecosystem name version"
	}{
		{"go.mod", "go.mod", "@@ -5,3 +5,4 @@\n-\tgithub.com/a/b v1.0.0\n+\tgithub.com/a/b v1.1.0\n \tgithub.com/c/d v2.0.0\n+require github.com/e/f v0.3.1 // indirect\n+go 1.22\n",
			[]string{"go github.com/a/b v1.1.0", "go github.com/e/f v0.3.1"}},
		{"go.sum", "sub/go.sum", "@@ -1 +1,2 @@\n+github.com/a/b v1.1.0 h1:abc=\n+github.com/a/b v1.1.0/go.mod h1:def=\n",
			[]string{"go github.com/a/b v1.1.0"}},
		{"package.json", "web/package.json", "@@ -3,3 +3,4 @@\n   \"dependencies\": {\n-    \"left-pad\": \"^1.0.0\",\n+    \"left-pad\": \"^1.3.0\",\n+    \"@scope/pkg\": \"~2.1.0\",\n+    \"version\": \"9.9.9\"\n",
			[]string{"npm left-pad 1.3.0", "npm @scope/pkg 2.1.0"}},
		{"package-lock", "package-lock.json", "@@ -1 +1 @@\n+      \"resolved\": \"https://registry.npmjs.org/@babel/core/-/core-7.23.0.tgz\",\n",
			[]string{"npm @babel/core 7.23.0"}},
		{"yarn.lock", "yarn.lock", "@@ -1,3 +1,3 @@\n lodash@^4.17.20:\n-  version \"4.17.20\"\n+  version \"4.17.21\"\n",
			[]string{"npm lodash 4.17.21"}},
		{"pnpm-lock", "pnpm-lock.yaml", "@@ -1 +1 @@\n+  /lodash@4.17.21:\n",
			[]string{"npm lodash 4.17.21"}},
		{"requirements", "requirements-dev.txt", "@@ -1 +1,3 @@\n+requests==2.31.0\n+Django==4.2.1 ; python_version > \"3\"\n+flask[async]>=2.0\n",
			[]string{"pypi requests 2.31.0", "pypi django 4.2.1"}},
		{"poetry.lock", "poetry.lock", "@@ -1,3 +1,3 @@\n [[package]]\n name = \"urllib3\"\n-version = \"1.26.0\"\n+version = \"2.0.7\"\n",
			[]string{"pypi urllib3 2.0.7"}},
		{"Cargo.lock", "Cargo.lock", "@@ -1,3 +1,3 @@\n [[package]]\n name = \"serde\"\n-version = \"1.0.100\"\n+version = \"1.0.200\"\n",
			[]string{"crates serde 1.0.200"}},
		{"gradle lockfile", "gradle.lockfile", "@@ -1 +1 @@\n+org.slf4j:slf4j-api:2.0.9=compileClasspath\n",
			[]string{"maven org.slf4j:slf4j-api 2.0.9"}},
		{"pom.xml", "pom.xml", "@@ -1,4 +1,4 @@\n   <dependency>\n     <groupId>com.google.guava</groupId>\n     <artifactId>guava</artifactId>\n-    <version>31.0-jre</version>\n+    <version>33.0.0-jre</version>\n",
			[]string{"maven com.google.guava:guava 33.0.0-jre"}},
		{"Gemfile.lock", "Gemfile.lock", "@@ -1 +1,2 @@\n+    rack (3.0.9)\n+    nokogiri (1.16.0-x86_64-linux)\n",
			[]string{"rubygems rack 3.0.9", "rubygems nokogiri 1.16.0"}},
		{"composer.lock", "composer.lock", "@@ -1,3 +1,3 @@\n         \"name\": \"monolog/monolog\",\n-        \"version\": \"3.0.0\",\n+        \"version\": \"v3.5.0\",\n",
			[]string{"packagist monolog/monolog 3.5.0"}},
		{"packages.lock.json", "packages.lock.json", "@@ -1,4 +1,4 @@\n     \"Newtonsoft.Json\": {\n       \"type\": \"Direct\",\n-      \"resolved\": \"12.0.1\",\n+      \"resolved\": \"13.0.3\",\n",
			[]string{"nuget Newtonsoft.Json 13.0.3"}},
		{"actions", ".github/workflows/ci.yml", "@@ -1 +1,4 @@\n+      - uses: actions/checkout@v4.1.1\n+      - uses: actions/setup-go@v5\n+      - uses: foo/bar@0123456789abcdef0123456789abcdef01234567\n+      - uses: ./local\n",
			[]string{"actions actions/checkout v4.1.1", "actions actions/setup-go ", "actions foo/bar "}},
		{"container", "Dockerfile", "@@ -1 +1,3 @@\n+FROM golang:1.22-alpine AS build\n+FROM scratch\n+FROM --platform=linux/amd64 ghcr.io/x/y@sha256:abc\n",
			[]string{"container golang 1.22-alpine", "container ghcr.io/x/y sha256:abc"}},
		{"compose", "docker-compose.yml", "@@ -1 +1 @@\n+    image: \"redis:7.2.4\"\n",
			[]string{"container redis 7.2.4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eco, ok := ecosystemFor(tc.file)
			if !ok {
				t.Fatalf("no ecosystem owns %q", tc.file)
			}
			var got []string
			for _, d := range eco.Parse(tc.file, patchLines(tc.patch)) {
				got = append(got, d.Ecosystem+" "+d.Name+" "+d.Version)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parsed %q, want %q", got, tc.want)
			}
		})
	}
}

func TestActionsAndContainerNotes(t *testing.T) {
	eco, _ := ecosystemFor(".github/workflows/x.yaml")
	deps := eco.Parse("w", patchLines("+ uses: actions/setup-go@v5\n+ uses: foo/bar@0123456789abcdef0123456789abcdef01234567\n"))
	if len(deps) != 2 || !strings.Contains(deps[0].Note, "floating") || !strings.Contains(deps[1].Note, "SHA") {
		t.Fatalf("unexpected notes: %+v", deps)
	}
	if deps[0].Version != "" || deps[1].Version != "" {
		t.Fatalf("unversioned refs must carry no version: %+v", deps)
	}
}

func TestEveryEcosystemHasParserAndPatterns(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Ecosystems {
		if e.ID == "" || e.Parse == nil || len(e.Files) == 0 || seen[e.ID] {
			t.Fatalf("incomplete or duplicate table entry %+v", e)
		}
		seen[e.ID] = true
		if (e.Age == nil) != (e.Registry == "") {
			t.Fatalf("%s: Age and Registry must be set together", e.ID)
		}
	}
}

func TestUnsupportedManifests(t *testing.T) {
	for file, want := range map[string]string{"mix.lock": "Hex (Elixir)", "deep/Pipfile.lock": "Pipenv", "flake.lock": "Nix flakes"} {
		got, ok := unsupportedFor(file)
		if !ok || got != want {
			t.Fatalf("%s: got %q %v, want %q", file, got, ok, want)
		}
	}
	if _, ok := unsupportedFor("go.mod"); ok {
		t.Fatal("go.mod is supported")
	}
}

func TestPatchLinesSkipsRemovedAndHeaders(t *testing.T) {
	got := patchLines("@@ -1 +1 @@\n-old\n+new\n ctx\n\\ No newline at end of file\n")
	if len(got) != 2 || got[0].text != "new" || !got[0].added || got[1].text != "ctx" || got[1].added {
		t.Fatalf("unexpected lines: %+v", got)
	}
}
