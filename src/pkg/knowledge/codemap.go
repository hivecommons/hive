package knowledge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Repository code maps (#11106) are GENERATED knowledge: a deterministic
// summary of a checked-out repository (packages, entry points, ownership
// hotspots, public APIs, test layout and extension points) written into a
// vault as a repo-scoped reference fact. They are never hand-authored; the
// generator owns the file and rewrites it when the source revision changes.
const (
	// CodeMapGenerator is recorded in the `generator:` frontmatter key.
	CodeMapGenerator = "hive-codemap"
	// CodeMapGeneratorVersion is bumped whenever the rendered format changes so
	// existing maps are regenerated even when the source SHA is unchanged.
	CodeMapGeneratorVersion = "1"

	// codeMapTag marks every generated code map fact.
	codeMapTag = "codemap"

	codeMapMaxEntryBytes  = 240
	codeMapMaxRepoRunes   = 100
	codeMapMaxSourceBytes = 512 << 10
	codeMapMaxAPIPerPkg   = 15
	codeMapChurnCommits   = 200
	codeMapGitTimeout     = 15 * time.Second
	// codeMapFooterReserve keeps room for the final "sections omitted" note so
	// the rendered body never exceeds MaxTotalBytes.
	codeMapFooterReserve = 96
	codeMapMinTotalBytes = 1024
)

// Section titles, in render order.
const (
	CodeMapSectionPackages       = "Packages"
	CodeMapSectionEntryPoints    = "Entry points"
	CodeMapSectionOwnership      = "Ownership hotspots"
	CodeMapSectionPublicAPIs     = "Public APIs"
	CodeMapSectionTestLayout     = "Test layout"
	CodeMapSectionExtensionPoint = "Extension points"
)

// CodeMapCaps bounds a rendered code map so it stays small enough to prime an
// agent prompt. Zero fields take the matching DefaultCodeMapCaps value.
type CodeMapCaps struct {
	MaxEntriesPerSection int
	MaxSectionBytes      int
	MaxTotalBytes        int
	// MaxFilesScanned stops the walk on very large trees; the map notes that
	// the scan was partial.
	MaxFilesScanned int
}

// DefaultCodeMapCaps are the caps used when none are configured.
var DefaultCodeMapCaps = CodeMapCaps{
	MaxEntriesPerSection: 40,
	MaxSectionBytes:      4096,
	MaxTotalBytes:        16384,
	MaxFilesScanned:      20000,
}

func (c CodeMapCaps) withDefaults() CodeMapCaps {
	if c.MaxEntriesPerSection <= 0 {
		c.MaxEntriesPerSection = DefaultCodeMapCaps.MaxEntriesPerSection
	}
	if c.MaxSectionBytes <= 0 {
		c.MaxSectionBytes = DefaultCodeMapCaps.MaxSectionBytes
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = DefaultCodeMapCaps.MaxTotalBytes
	}
	if c.MaxTotalBytes < codeMapMinTotalBytes {
		c.MaxTotalBytes = codeMapMinTotalBytes
	}
	if c.MaxFilesScanned <= 0 {
		c.MaxFilesScanned = DefaultCodeMapCaps.MaxFilesScanned
	}
	return c
}

// CodeMapSection is one titled list of entries, sorted deterministically.
type CodeMapSection struct {
	Title   string
	Entries []string
}

// CodeMap is a generated repository summary plus its freshness metadata.
type CodeMap struct {
	Repo             string
	SourceSHA        string
	GeneratorVersion string
	GeneratedAt      time.Time
	Sections         []CodeMapSection
	// Partial is true when the walk stopped at MaxFilesScanned.
	Partial bool
	Caps    CodeMapCaps
}

// CodeMapOptions configures GenerateCodeMap.
type CodeMapOptions struct {
	// Repo names the repository; it becomes the repo:<name> scope tag.
	Repo string
	// Root is the checked-out repository directory.
	Root string
	// SourceSHA is the revision the map describes. Empty resolves HEAD when
	// Root is a git work tree.
	SourceSHA string
	Caps      CodeMapCaps
	Now       func() time.Time
}

// codeMapSkipDirs are never descended into: dependencies, build output and
// VCS metadata say nothing about the repository's own structure.
var codeMapSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "third_party": true, "dist": true,
	"target": true, "__pycache__": true, "venv": true, "site-packages": true,
}

var codeMapSourceExts = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".jsx": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".rs": "rust", ".java": "java",
	".kt": "kotlin", ".rb": "ruby", ".sh": "shell", ".c": "c", ".h": "c",
	".cc": "c++", ".cpp": "c++", ".hpp": "c++", ".cs": "c#", ".swift": "swift",
	".php": "php", ".scala": "scala",
}

var codeMapTestDirNames = map[string]bool{
	"test": true, "tests": true, "e2e": true, "testdata": true,
	"__tests__": true, "spec": true, "integration": true, "fixtures": true,
}

var codeMapExtensionDirNames = map[string]bool{
	"plugin": true, "plugins": true, "extension": true, "extensions": true,
	"hooks": true, "providers": true, "connector": true, "connectors": true,
	"adapters": true, "drivers": true, "addons": true, "skills": true,
}

var codeMapEntryFiles = map[string]bool{
	"Makefile": true, "Dockerfile": true, "Taskfile.yml": true, "justfile": true,
	"__main__.py": true, "manage.py": true, "setup.py": true, "package.json": true,
	"Cargo.toml": true, "pyproject.toml": true, "go.mod": true,
}

type codeMapDir struct {
	langs      map[string]int
	goPkg      string
	goFiles    int
	goTests    int
	hasMain    bool
	tests      int
	exported   []string
	interfaces []string
	registers  []string
}

type codeMapScan struct {
	dirs       map[string]*codeMapDir
	entryFiles []string
	scripts    []string
	codeowners []string
	testDirs   []string
	extDirs    []string
	filesSeen  int
	partial    bool
}

func (s *codeMapScan) dir(rel string) *codeMapDir {
	d := s.dirs[rel]
	if d == nil {
		d = &codeMapDir{langs: map[string]int{}}
		s.dirs[rel] = d
	}
	return d
}

// GenerateCodeMap walks opts.Root and builds a deterministic code map. The
// same tree at the same revision always yields identical sections.
func GenerateCodeMap(ctx context.Context, opts CodeMapOptions) (*CodeMap, error) {
	root := strings.TrimSpace(opts.Root)
	if root == "" {
		return nil, fmt.Errorf("code map: root directory is required")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("code map: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("code map: %s is not a directory", root)
	}
	repo := codeMapClip(sanitizeFrontmatterValue(opts.Repo), codeMapMaxRepoRunes)
	if repo == "" {
		repo = filepath.Base(root)
	}
	caps := opts.Caps.withDefaults()
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	sha := strings.TrimSpace(opts.SourceSHA)
	if sha == "" {
		sha = CodeMapHeadSHA(ctx, root)
	}

	scan := &codeMapScan{dirs: map[string]*codeMapDir{}}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := d.Name()
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if (strings.HasPrefix(name, ".") && name != ".github") || codeMapSkipDirs[name] {
				return fs.SkipDir
			}
			if codeMapTestDirNames[name] {
				scan.testDirs = append(scan.testDirs, rel)
			}
			if codeMapExtensionDirNames[strings.ToLower(name)] {
				scan.extDirs = append(scan.extDirs, rel)
			}
			if name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if scan.filesSeen >= caps.MaxFilesScanned {
			scan.partial = true
			return fs.SkipAll
		}
		scan.filesSeen++
		scan.visitFile(root, rel, name)
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	cm := &CodeMap{
		Repo:             repo,
		SourceSHA:        sha,
		GeneratorVersion: CodeMapGeneratorVersion,
		GeneratedAt:      now().UTC().Truncate(time.Second),
		Partial:          scan.partial,
		Caps:             caps,
	}
	cm.Sections = []CodeMapSection{
		{Title: CodeMapSectionPackages, Entries: scan.packageEntries()},
		{Title: CodeMapSectionEntryPoints, Entries: scan.entryPointEntries()},
		{Title: CodeMapSectionOwnership, Entries: scan.ownershipEntries(ctx, root)},
		{Title: CodeMapSectionPublicAPIs, Entries: scan.publicAPIEntries()},
		{Title: CodeMapSectionTestLayout, Entries: scan.testEntries()},
		{Title: CodeMapSectionExtensionPoint, Entries: scan.extensionEntries()},
	}
	return cm, nil
}

func (s *codeMapScan) visitFile(root, rel, name string) {
	dirRel := path.Dir(rel)
	if (dirRel == "." || dirRel == ".github" || dirRel == "docs") && name == "CODEOWNERS" {
		s.readCodeowners(filepath.Join(root, filepath.FromSlash(rel)))
		return
	}
	if strings.HasPrefix(dirRel, ".github") {
		return
	}
	if codeMapEntryFiles[name] {
		s.entryFiles = append(s.entryFiles, rel)
	}
	ext := strings.ToLower(filepath.Ext(name))
	lang, isSource := codeMapSourceExts[ext]
	if !isSource {
		return
	}
	d := s.dir(dirRel)
	d.langs[lang]++
	if codeMapIsTestFile(name) {
		d.tests++
		if ext == ".go" {
			d.goTests++
		}
		return
	}
	if ext == ".sh" || codeMapInScriptsDir(dirRel) {
		s.scripts = append(s.scripts, rel)
	}
	if ext == ".go" {
		d.goFiles++
		s.parseGoFile(filepath.Join(root, filepath.FromSlash(rel)), d)
	}
}

func codeMapInScriptsDir(dirRel string) bool {
	for _, seg := range strings.Split(dirRel, "/") {
		if seg == "scripts" || seg == "bin" {
			return true
		}
	}
	return false
}

func codeMapIsTestFile(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "_test.go"),
		strings.HasSuffix(lower, "_test.py"),
		strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py"),
		strings.Contains(lower, ".test."),
		strings.Contains(lower, ".spec."),
		strings.HasSuffix(lower, "_spec.rb"),
		strings.HasSuffix(lower, "_test.rb"),
		strings.HasSuffix(lower, "test.java"),
		strings.HasSuffix(lower, "tests.cs"):
		return true
	}
	return false
}

// parseGoFile records the package clause and exported top-level identifiers.
// Function bodies are never inspected; parse errors simply skip the file.
func (s *codeMapScan) parseGoFile(p string, d *codeMapDir) {
	info, err := os.Stat(p)
	if err != nil || info.Size() > codeMapMaxSourceBytes {
		return
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
	if err != nil || f.Name == nil {
		return
	}
	if d.goPkg == "" || d.goPkg == "main" {
		d.goPkg = f.Name.Name
	}
	for _, decl := range f.Decls {
		switch dd := decl.(type) {
		case *ast.FuncDecl:
			if dd.Recv != nil || dd.Name == nil {
				continue
			}
			if f.Name.Name == "main" && dd.Name.Name == "main" {
				d.hasMain = true
			}
			if !dd.Name.IsExported() {
				continue
			}
			d.exported = append(d.exported, dd.Name.Name)
			if strings.HasPrefix(dd.Name.Name, "Register") {
				d.registers = append(d.registers, dd.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range dd.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if !sp.Name.IsExported() {
						continue
					}
					d.exported = append(d.exported, sp.Name.Name)
					if _, ok := sp.Type.(*ast.InterfaceType); ok {
						d.interfaces = append(d.interfaces, sp.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.IsExported() {
							d.exported = append(d.exported, n.Name)
						}
					}
				}
			}
		}
	}
}

func (s *codeMapScan) readCodeowners(p string) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		s.codeowners = append(s.codeowners, fields[0]+" → "+strings.Join(fields[1:], " "))
	}
}

func (s *codeMapScan) sortedDirs() []string {
	out := make([]string, 0, len(s.dirs))
	for k := range s.dirs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func codeMapLangSummary(langs map[string]int) string {
	keys := make([]string, 0, len(langs))
	for k := range langs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+strconv.Itoa(langs[k]))
	}
	return strings.Join(parts, ", ")
}

func (s *codeMapScan) packageEntries() []string {
	var out []string
	for _, rel := range s.sortedDirs() {
		d := s.dirs[rel]
		if d.goFiles > 0 && d.goPkg != "" {
			out = append(out, fmt.Sprintf("%s — go package %s (%d files, %d tests)", rel, d.goPkg, d.goFiles, d.goTests))
			continue
		}
		total := 0
		for _, n := range d.langs {
			total += n
		}
		if total-d.tests <= 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s — %s", rel, codeMapLangSummary(d.langs)))
	}
	return out
}

func (s *codeMapScan) entryPointEntries() []string {
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, rel := range s.sortedDirs() {
		d := s.dirs[rel]
		switch {
		case d.hasMain:
			add(rel + " — go main package")
		case rel == "cmd" || strings.HasPrefix(rel, "cmd/") || strings.Contains(rel, "/cmd/"):
			add(rel + " — command directory")
		}
	}
	files := append(append([]string(nil), s.entryFiles...), s.scripts...)
	sort.Strings(files)
	for _, f := range files {
		add(f)
	}
	sort.Strings(out)
	return out
}

func (s *codeMapScan) ownershipEntries(ctx context.Context, root string) []string {
	out := make([]string, 0, len(s.codeowners))
	for _, o := range s.codeowners {
		out = append(out, "CODEOWNERS: "+o)
	}
	return append(out, codeMapChurn(ctx, root)...)
}

// codeMapChurn ranks top-level directories by how many file changes the most
// recent commits touched. It is deterministic for a given HEAD and empty when
// root is not a git work tree or git is unavailable.
func codeMapChurn(ctx context.Context, root string) []string {
	if !isGitRepo(root) {
		return nil
	}
	out, err := codeMapGit(ctx, root, "log", "--no-merges", "-n", strconv.Itoa(codeMapChurnCommits), "--name-only", "--pretty=format:")
	if err != nil {
		return nil
	}
	counts := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		segs := strings.Split(line, "/")
		key := "."
		switch {
		case len(segs) >= 3:
			key = segs[0] + "/" + segs[1]
		case len(segs) == 2:
			key = segs[0]
		}
		counts[key]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	res := make([]string, 0, len(keys))
	for _, k := range keys {
		res = append(res, fmt.Sprintf("churn: %s — %d file changes in last %d commits", k, counts[k], codeMapChurnCommits))
	}
	return res
}

func (s *codeMapScan) publicAPIEntries() []string {
	var out []string
	for _, rel := range s.sortedDirs() {
		d := s.dirs[rel]
		if d.goPkg == "" || d.goPkg == "main" || len(d.exported) == 0 {
			continue
		}
		names := codeMapUniqueSorted(d.exported)
		extra := ""
		if len(names) > codeMapMaxAPIPerPkg {
			extra = fmt.Sprintf(" (+%d more)", len(names)-codeMapMaxAPIPerPkg)
			names = names[:codeMapMaxAPIPerPkg]
		}
		out = append(out, fmt.Sprintf("%s (%s): %s%s", rel, d.goPkg, strings.Join(names, ", "), extra))
	}
	return out
}

func (s *codeMapScan) testEntries() []string {
	var out []string
	for _, rel := range s.sortedDirs() {
		if d := s.dirs[rel]; d.tests > 0 {
			out = append(out, fmt.Sprintf("%s — %d test files", rel, d.tests))
		}
	}
	dirs := codeMapUniqueSorted(s.testDirs)
	for _, td := range dirs {
		out = append(out, td+"/ — test directory")
	}
	sort.Strings(out)
	return out
}

func (s *codeMapScan) extensionEntries() []string {
	var out []string
	for _, rel := range s.sortedDirs() {
		d := s.dirs[rel]
		for _, n := range codeMapUniqueSorted(d.interfaces) {
			out = append(out, fmt.Sprintf("interface %s.%s (%s)", d.goPkg, n, rel))
		}
		for _, n := range codeMapUniqueSorted(d.registers) {
			out = append(out, fmt.Sprintf("registry func %s.%s (%s)", d.goPkg, n, rel))
		}
	}
	for _, ed := range codeMapUniqueSorted(s.extDirs) {
		out = append(out, ed+"/ — plugin/extension directory")
	}
	sort.Strings(out)
	return out
}

func codeMapUniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	cp := append([]string(nil), in...)
	sort.Strings(cp)
	out := cp[:0]
	for i, v := range cp {
		if i == 0 || v != cp[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// codeMapClip truncates s to at most n runes.
func codeMapClip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// codeMapClipBytes truncates s to at most n bytes on a rune boundary, adding
// an ellipsis when it cut anything.
func codeMapClipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// CodeMapTruncatedMarker prefixes the line that replaces entries dropped by a
// size cap, so readers (and tests) can tell a primer was bounded.
const CodeMapTruncatedMarker = "… truncated:"

func codeMapTruncLine(n int) string {
	return fmt.Sprintf("- %s %d more entries omitted (size cap)\n", CodeMapTruncatedMarker, n)
}

// codeMapMaxRevBytes bounds the revision shown in the header (a SHA-256
// object id is 64 hex characters). The header always reserves this much so
// the section budget, and therefore truncation, does not depend on the SHA.
const codeMapMaxRevBytes = 64

func (cm *CodeMap) header() string {
	rev := cm.SourceSHA
	if rev == "" {
		rev = "unknown revision"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Code map: %s\n\n", cm.Repo)
	fmt.Fprintf(&b, "Generated by %s v%s from `%s`. Do not edit by hand: this page is regenerated when the repository changes.\n",
		CodeMapGenerator, cm.GeneratorVersion, codeMapClipBytes(rev, codeMapMaxRevBytes))
	if cm.Partial {
		fmt.Fprintf(&b, "\nScan stopped after %d files; the map is partial.\n", cm.Caps.withDefaults().MaxFilesScanned)
	}
	return b.String()
}

// Body renders the map as markdown, enforcing the per-section entry and byte
// caps and the total byte cap. Truncation is deterministic: entries are kept
// in sorted order until a cap is hit and the remainder is replaced by a
// marker line.
func (cm *CodeMap) Body() string {
	return cm.header() + cm.renderSections()
}

func (cm *CodeMap) renderSections() string {
	caps := cm.Caps.withDefaults()
	var b strings.Builder
	headerBudget := len(cm.header()) - len(codeMapClipBytes(cm.SourceSHA, codeMapMaxRevBytes)) + codeMapMaxRevBytes
	if cm.SourceSHA == "" {
		headerBudget = len(cm.header()) - len("unknown revision") + codeMapMaxRevBytes
	}
	remaining := caps.MaxTotalBytes - codeMapFooterReserve - headerBudget
	for i, sec := range cm.Sections {
		title := "\n## " + sec.Title + "\n\n"
		minNeeded := len(title) + len(codeMapTruncLine(len(sec.Entries)))
		if remaining < minNeeded {
			fmt.Fprintf(&b, "\n_%s %d sections omitted (total size cap)._\n", CodeMapTruncatedMarker, len(cm.Sections)-i)
			break
		}
		b.WriteString(title)
		remaining -= len(title)
		if len(sec.Entries) == 0 {
			b.WriteString("- (none found)\n")
			remaining -= len("- (none found)\n")
			continue
		}
		marker := codeMapTruncLine(len(sec.Entries))
		budget := remaining - len(marker)
		if caps.MaxSectionBytes < budget {
			budget = caps.MaxSectionBytes
		}
		used, kept := 0, 0
		for _, e := range sec.Entries {
			if kept >= caps.MaxEntriesPerSection {
				break
			}
			line := "- " + codeMapClipBytes(sanitizeFrontmatterValue(e), codeMapMaxEntryBytes) + "\n"
			if used+len(line) > budget {
				break
			}
			b.WriteString(line)
			used += len(line)
			kept++
		}
		remaining -= used
		if omitted := len(sec.Entries) - kept; omitted > 0 {
			line := codeMapTruncLine(omitted)
			b.WriteString(line)
			remaining -= len(line)
		}
	}
	return b.String()
}

// ContentHash fingerprints what the map says about the repository: the repo,
// generator version and rendered sections, but not the source SHA or
// generated_at. A new commit that does not change the summary therefore
// keeps the same hash and does not count as new content.
func (cm *CodeMap) ContentHash() string {
	sum := sha256.Sum256([]byte(cm.Repo + "\x00" + cm.GeneratorVersion + "\x00" + cm.renderSections()))
	return hex.EncodeToString(sum[:])
}

// CodeMapMeta is the freshness metadata stored in a code map's frontmatter.
type CodeMapMeta struct {
	Repo             string
	GeneratorVersion string
	SourceSHA        string
	ContentHash      string
	GeneratedAt      time.Time
}

// CodeMapSlug is the vault-relative slug of a repo's code map.
func CodeMapSlug(repo string) string {
	s := slugify(strings.ReplaceAll(repo, "/", "-"))
	if s == "" {
		s = "repo"
	}
	return "codemaps/codemap-" + s
}

// CodeMapPath is where WriteCodeMap stores repo's code map under vaultDir.
func CodeMapPath(vaultDir, repo string) string {
	return filepath.Join(vaultDir, filepath.FromSlash(CodeMapSlug(repo))+".md")
}

// ReadCodeMapMeta reads freshness metadata from an existing code map file.
// ok is false when the file does not exist or was not written by the
// generator.
func ReadCodeMapMeta(p string) (meta CodeMapMeta, ok bool, err error) {
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return CodeMapMeta{}, false, nil
		}
		return CodeMapMeta{}, false, err
	}
	content := string(data)
	if !strings.HasPrefix(content, "---\n") {
		return CodeMapMeta{}, false, nil
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return CodeMapMeta{}, false, nil
	}
	generator := ""
	for _, line := range strings.Split(content[4:4+end], "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), "\"'")
		switch strings.TrimSpace(key) {
		case "generator":
			generator = val
		case "generator_version":
			meta.GeneratorVersion = val
		case "source_sha":
			meta.SourceSHA = val
		case "content_hash":
			meta.ContentHash = val
		case "code_map_repo":
			meta.Repo = val
		case "generated_at":
			if ts, perr := time.Parse(time.RFC3339, val); perr == nil {
				meta.GeneratedAt = ts
			}
		}
	}
	if generator != CodeMapGenerator {
		return CodeMapMeta{}, false, nil
	}
	return meta, true, nil
}

// CodeMapNeedsRegeneration decides whether a stored map is stale relative to
// the repository's current revision. A missing map, a generator version
// change, an unknown current revision, or a new HEAD SHA all require
// regeneration; WriteCodeMap then skips the write when the content is
// unchanged.
func CodeMapNeedsRegeneration(meta CodeMapMeta, exists bool, headSHA string) (bool, string) {
	switch {
	case !exists:
		return true, "no code map yet"
	case meta.GeneratorVersion != CodeMapGeneratorVersion:
		return true, "generator version changed"
	case strings.TrimSpace(headSHA) == "":
		return true, "source revision unknown"
	case meta.SourceSHA != strings.TrimSpace(headSHA):
		return true, "source revision changed"
	}
	return false, "up to date"
}

// WriteCodeMap stores cm in vaultDir as a repo-scoped, approved reference
// fact. It returns written=false and leaves the file untouched when the
// stored map already has the same content hash, generator version and SHA.
// When only the SHA moved the file is rewritten to record the new SHA but
// keeps its original generated_at, since the summary itself did not change.
func WriteCodeMap(vaultDir string, cm *CodeMap, layer LayerType) (string, bool, error) {
	if cm == nil {
		return "", false, fmt.Errorf("code map: nil map")
	}
	if layer == "" {
		layer = LayerProject
	}
	p := CodeMapPath(vaultDir, cm.Repo)
	body := cm.Body()
	hash := cm.ContentHash()

	generatedAt := cm.GeneratedAt
	meta, exists, err := ReadCodeMapMeta(p)
	if err != nil {
		return p, false, err
	}
	if exists && meta.ContentHash == hash && meta.GeneratorVersion == cm.GeneratorVersion {
		if meta.SourceSHA == cm.SourceSHA {
			return p, false, nil
		}
		if !meta.GeneratedAt.IsZero() {
			generatedAt = meta.GeneratedAt
		}
	}
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC().Truncate(time.Second)
	}
	ts := generatedAt.UTC().Format(time.RFC3339)

	var buf strings.Builder
	buf.WriteString("---\n")
	fmt.Fprintf(&buf, "title: Code map: %s\n", sanitizeFrontmatterValue(cm.Repo))
	fmt.Fprintf(&buf, "type: %s\n", FactReference)
	fmt.Fprintf(&buf, "layer: %s\n", sanitizeFrontmatterValue(string(layer)))
	fmt.Fprintf(&buf, "state: %s\n", StateApproved)
	buf.WriteString("confidence: 1.00\n")
	fmt.Fprintf(&buf, "tags: [%s]\n", sanitizeFrontmatterList([]string{codeMapTag, "generated", repoTagPrefix + cm.Repo}))
	fmt.Fprintf(&buf, "source: %s\n", CodeMapGenerator)
	fmt.Fprintf(&buf, "generator: %s\n", CodeMapGenerator)
	fmt.Fprintf(&buf, "generator_version: %s\n", sanitizeFrontmatterValue(cm.GeneratorVersion))
	fmt.Fprintf(&buf, "code_map_repo: %s\n", sanitizeFrontmatterValue(cm.Repo))
	fmt.Fprintf(&buf, "source_sha: %s\n", sanitizeFrontmatterValue(cm.SourceSHA))
	fmt.Fprintf(&buf, "content_hash: %s\n", hash)
	fmt.Fprintf(&buf, "generated_at: %s\n", ts)
	// synced: feeds the existing freshness signal (Fact source date).
	fmt.Fprintf(&buf, "synced: %s\n", ts)
	buf.WriteString("---\n\n")
	buf.WriteString(body)

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return p, false, fmt.Errorf("code map: creating dir: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return p, false, fmt.Errorf("code map: writing: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return p, false, fmt.Errorf("code map: renaming: %w", err)
	}
	return p, true, nil
}

// CodeMapHeadSHA returns the HEAD commit of the git work tree at root, or ""
// when root is not a git repository or git is unavailable.
func CodeMapHeadSHA(ctx context.Context, root string) string {
	if !isGitRepo(root) {
		return ""
	}
	out, err := codeMapGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// codeMapGit runs a LOCAL git command (no remote access) in root.
func codeMapGit(ctx context.Context, root string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, codeMapGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
