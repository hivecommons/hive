package integrated

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func InspectCheckout(root, defaultBranch string) (RepositoryInspection, error) {
	if err := validateOrdinarySetupCheckout(root); err != nil {
		return RepositoryInspection{}, err
	}
	inspection := RepositoryInspection{
		DefaultBranch:  defaultBranch,
		Permissions:    map[string]bool{},
		Signals:        map[string]string{},
		packageScripts: map[string]map[string]string{},
		packageRunners: map[string]string{},
		baselineLint:   map[string]bool{},
	}
	committedFiles, hasCommittedHead := committedFileSet(root)
	languages, frameworks, managers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	packageFiles := findPackageJSONFiles(root)
	for _, packageFile := range packageFiles {
		var packageData packageJSON
		data, err := os.ReadFile(packageFile)
		if err != nil || json.Unmarshal(data, &packageData) != nil {
			continue
		}
		packageRoot := filepath.Dir(packageFile)
		packagePath, _ := filepath.Rel(root, packageRoot)
		packagePath = filepath.ToSlash(packagePath)
		if packagePath == "" {
			packagePath = "."
		}
		inspection.packageScripts[packagePath] = cloneScriptMap(packageData.Scripts)
		inspection.baselineLint[packagePath] = hasBaselineAwareLintCheck(packageRoot, packageData.Scripts)
		languages["TypeScript/JavaScript"] = true
		for name := range mergeMaps(packageData.Dependencies, packageData.DevDependencies) {
			switch {
			case name == "react" || strings.HasPrefix(name, "@react-"):
				frameworks["React"] = true
			case name == "next":
				frameworks["Next.js"] = true
			case name == "vue":
				frameworks["Vue"] = true
			case name == "@playwright/test":
				frameworks["Playwright"] = true
			case strings.Contains(name, "storybook"):
				frameworks["Storybook"] = true
			}
		}
		runner, err := packageRunnerForRoot(root, packageRoot)
		if err != nil {
			return RepositoryInspection{}, err
		}
		managers[map[string]string{"npm": "npm", "pnpm": "pnpm", "yarn": "yarn"}[runner]] = true
		inspection.packageRunners[packagePath] = runner
		commands := packageScriptCommands(packagePath, runner, packageData.Scripts)
		inspection.TestCommands = append(inspection.TestCommands, commands...)
		for name, script := range packageData.Scripts {
			lowerName := strings.ToLower(name)
			if name == "test" || name == "build" || name == "vh:plan" || name == "vh:run" || strings.Contains(lowerName, "test") || strings.Contains(lowerName, "lint") || strings.Contains(lowerName, "typecheck") || strings.Contains(lowerName, "suite") || strings.Contains(lowerName, "mutation") || strings.Contains(lowerName, "mutate") || strings.Contains(lowerName, "e2e") || strings.Contains(lowerName, "visual") {
				inspection.Signals["script:"+packagePath+":"+name] = script
			}
		}
	}
	inspection.TestCommands = preferGranularTestCommands(inspection.TestCommands)
	if exists(filepath.Join(root, "go.mod")) {
		languages["Go"] = true
		managers["Go modules"] = true
		inspection.TestCommands = append(inspection.TestCommands, []string{"go", "test", "./..."})
	}
	if exists(filepath.Join(root, "pyproject.toml")) || exists(filepath.Join(root, "requirements.txt")) {
		languages["Python"] = true
		managers["pip/pyproject"] = true
		if (exists(filepath.Join(root, "tests")) || fileContains(filepath.Join(root, "pyproject.toml"), "pytest")) && !commandsContain(inspection.TestCommands, "pytest") && !commandsContain(inspection.TestCommands, "test:python") {
			inspection.TestCommands = append(inspection.TestCommands, []string{"python", "-m", "pytest", "-q"})
		}
	}

	count := 0
	_ = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil || count > 10000 {
			return nil
		}
		relative, relErr := filepath.Rel(root, filePath)
		if relErr != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			base := entry.Name()
			if base == ".git" || base == "node_modules" || base == "dist" || base == "vendor" || base == ".visual-hive" {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		lower := strings.ToLower(relative)
		switch filepath.Ext(lower) {
		case ".go":
			languages["Go"] = true
		case ".py":
			languages["Python"] = true
		case ".rs":
			languages["Rust"] = true
		case ".java", ".kt":
			languages["JVM"] = true
		case ".ts", ".tsx", ".js", ".jsx":
			languages["TypeScript/JavaScript"] = true
		}
		if strings.HasPrefix(lower, ".github/workflows/") {
			inspection.CIFiles = append(inspection.CIFiles, relative)
		}
		if strings.Contains(lower, "dockerfile") || strings.HasPrefix(lower, "deploy/") || strings.HasPrefix(lower, "k8s/") || strings.HasSuffix(lower, "vercel.json") || strings.Contains(lower, "terraform") {
			inspection.DeploymentFiles = append(inspection.DeploymentFiles, relative)
		}
		if isReviewableBaselinePath(lower) && (!hasCommittedHead || committedFiles[relative]) {
			inspection.BaselineFiles = append(inspection.BaselineFiles, relative)
		}
		if strings.Contains(lower, "auth") || strings.Contains(lower, "security") || strings.Contains(lower, "secret") || strings.HasPrefix(lower, ".github/workflows/") {
			inspection.HighRiskPaths = append(inspection.HighRiskPaths, relative)
		}
		return nil
	})
	inspection.Languages = sortedKeys(languages)
	inspection.Frameworks = sortedKeys(frameworks)
	inspection.PackageManagers = sortedKeys(managers)
	inspection.CIFiles = sortedUnique(inspection.CIFiles)
	inspection.DeploymentFiles = sortedUnique(inspection.DeploymentFiles)
	// The broad repository walk deliberately skips generated .visual-hive
	// artifacts. Reviewed snapshots are the exception: setup must distinguish
	// committed baselines from transient actual/diff images so its plan does not
	// claim baseline review is missing when a repository already has it.
	for _, baseline := range existingVisualBaselines(root) {
		if isReviewableBaselinePath(baseline) && (!hasCommittedHead || committedFiles[baseline]) {
			inspection.BaselineFiles = append(inspection.BaselineFiles, baseline)
		}
	}
	inspection.BaselineFiles = sortedUnique(inspection.BaselineFiles)
	inspection.HighRiskPaths = sortedUnique(inspection.HighRiskPaths)
	inspection.TestCommands = uniqueTestCommands(inspection.TestCommands)
	sort.SliceStable(inspection.TestCommands, func(i, j int) bool {
		left, right := testCommandPriority(inspection.TestCommands[i]), testCommandPriority(inspection.TestCommands[j])
		if left != right {
			return left < right
		}
		return strings.Join(inspection.TestCommands[i], "\x00") < strings.Join(inspection.TestCommands[j], "\x00")
	})
	return inspection, nil
}

func hasBaselineAwareLintCheck(packageRoot string, scripts map[string]string) bool {
	lintBody, hasLint := scripts["lint"]
	checkBody, hasCheck := scripts["lint:check"]
	if !hasLint || !hasCheck || strings.TrimSpace(strings.ToLower(lintBody)) != "eslint ." ||
		!safeAutomationScript("lint", lintBody) || !hasSafePackageScriptClosure("lint", scripts) ||
		!safeAutomationScript("lint:check", checkBody) || !hasSafePackageScriptClosure("lint:check", scripts) {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(checkBody))
	if len(fields) != 2 || strings.ToLower(fields[0]) != "node" {
		return false
	}
	relative := filepath.Clean(fields[1])
	if filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	extension := strings.ToLower(filepath.Ext(relative))
	if extension != ".js" && extension != ".mjs" && extension != ".cjs" {
		return false
	}
	source, err := os.ReadFile(filepath.Join(packageRoot, relative))
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(source))
	for _, required := range []string{
		"eslint . --format json",
		".eslint-baseline.json",
		"newentries.length > 0",
		"process.exit(1)",
		"process.exit(0)",
	} {
		if !strings.Contains(lower, required) {
			return false
		}
	}
	return exists(filepath.Join(packageRoot, ".eslint-baseline.json"))
}

func committedFileSet(root string) (map[string]bool, bool) {
	topOutput, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, false
	}
	rootInfo, rootErr := os.Stat(root)
	topInfo, topErr := os.Stat(strings.TrimSpace(string(topOutput)))
	if rootErr != nil || topErr != nil || !os.SameFile(rootInfo, topInfo) {
		return nil, false
	}
	output, err := exec.Command("git", "-C", root, "ls-tree", "-r", "--name-only", "-z", "HEAD").Output()
	if err != nil {
		return nil, false
	}
	files := map[string]bool{}
	for _, value := range strings.Split(string(output), "\x00") {
		if value = filepath.ToSlash(strings.TrimSpace(value)); value != "" {
			files[value] = true
		}
	}
	return files, true
}

func isReviewableBaselinePath(value string) bool {
	lower := strings.ToLower(filepath.ToSlash(value))
	if !(strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".json")) {
		return false
	}
	for _, suffix := range []string{"-actual.png", "-diff.png", ".actual.png", ".diff.png", "-actual.json", "-diff.json", ".actual.json", ".diff.json"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return strings.Contains(lower, "baseline") || strings.Contains(lower, "__screenshots__") || strings.Contains(lower, "-snapshots/") || strings.HasPrefix(lower, ".visual-hive/snapshots/")
}

func commandsContain(commands [][]string, fragment string) bool {
	fragment = strings.ToLower(strings.TrimSpace(fragment))
	for _, command := range commands {
		if strings.Contains(strings.ToLower(strings.Join(command, " ")), fragment) {
			return true
		}
	}
	return false
}

func findPackageJSONFiles(root string) []string {
	result := []string{}
	_ = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".visual-hive", "node_modules", "dist", "build", "coverage", "vendor":
				if filePath != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() == "package.json" && len(result) < 100 {
			result = append(result, filePath)
		}
		return nil
	})
	sort.Strings(result)
	return result
}

func packageScriptCommands(packagePath, runner string, scripts map[string]string) [][]string {
	commands := [][]string{}
	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sort.Strings(names)
	liteBody, hasLite := scripts["test:ci:lite"]
	hasLite = hasLite && safeAutomationScript("test:ci:lite", liteBody) && hasSafePackageScriptClosure("test:ci:lite", scripts)
	if hasLite {
		commands = append(commands, packageScriptCommand(packagePath, runner, "test:ci:lite"))
	}
	for _, name := range names {
		// Visual Hive owns its namespaced lifecycle and evidence commands.
		// Running them again as independent repository-test jobs is both
		// redundant and incorrect: those jobs do not carry the sealed Visual
		// Hive CLI or evidence produced by earlier lifecycle phases.
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "vh:") {
			continue
		}
		if name == "test:ci:lite" || !safeAutomationScript(name, scripts[name]) || !hasSafePackageScriptClosure(name, scripts) {
			continue
		}
		// An unscoped Playwright invocation may fan out to browsers absent from
		// the managed runner. Prefer the repository's own explicit Chromium lane.
		if invokesSupersededUnscopedPlaywrightE2E(name, scripts) {
			continue
		}
		if hasLite && commandCoverageRank(packageScriptCommand(packagePath, runner, name)) == 1 {
			continue
		}
		commands = append(commands, packageScriptCommand(packagePath, runner, name))
	}
	return commands
}

func safeAutomationScript(name, body string) bool {
	name, body = strings.ToLower(strings.TrimSpace(name)), strings.ToLower(strings.TrimSpace(body))
	if name == "" || name == "vh:plan" || name == "vh:run" || name == "vh:build-source" {
		return false
	}
	// Every selected script runs as an isolated, non-interactive job. Exclude
	// operator/report/live lanes and browser projects that the managed workflow
	// does not install; segment equality avoids rejecting names like liveness.
	for _, segment := range strings.Split(name, ":") {
		switch segment {
		case "debug", "report", "live", "auth-drift", "browser-matrix", "macos-popup":
			return false
		}
	}
	for _, unsafe := range []string{"watch", "update", "snapshot:update", "interactive", "headed", "open", "dev", "serve", "preview"} {
		if strings.Contains(name, unsafe) {
			return false
		}
	}
	for _, unsafe := range []string{"--watch", "--watchall", "--update", "--update-snapshots", "--ui", "--open", "--headed", "--debug", "show-report", "@live-site", "playwright_storage_state=", "--project=firefox", "--project=webkit"} {
		if strings.Contains(body, unsafe) {
			return false
		}
	}
	// npm scripts run in a shell that does not guarantee pipefail. Reject
	// failure-masking or multi-statement forms rather than treating a swallowed
	// test failure as deterministic evidence. A strict && chain is allowed.
	if strings.ContainsAny(body, ";\r\n|`") || strings.Contains(strings.ReplaceAll(body, "&&", ""), "&") {
		return false
	}
	return commandCoverageRank([]string{"npm", "run", name}) > 0
}

func hasSafeScopedChromiumE2E(scripts map[string]string) bool {
	body, exists := scripts["test:e2e:chromium"]
	body = strings.ToLower(strings.TrimSpace(body))
	return exists && safeAutomationScript("test:e2e:chromium", body) && strings.Contains(body, "playwright test") && strings.Contains(body, "--project=chromium")
}

func isUnscopedPlaywrightE2E(body string) bool {
	body = strings.ToLower(strings.TrimSpace(body))
	return strings.Contains(body, "playwright test") && !strings.Contains(body, "--project") && !strings.Contains(body, "--config")
}

func hasSafePackageScriptClosure(name string, scripts map[string]string) bool {
	_, safe := terminalPackageScripts(scripts, name)
	return safe
}

func invokesSupersededUnscopedPlaywrightE2E(name string, scripts map[string]string) bool {
	if !hasSafeScopedChromiumE2E(scripts) || !isUnscopedPlaywrightE2E(scripts["test:e2e"]) {
		return false
	}
	terminals, safe := terminalPackageScripts(scripts, name)
	if !safe {
		return false
	}
	for terminal := range terminals {
		if terminal.Name == "test:e2e" && terminal.Forwarded != "--project=chromium" && terminal.Forwarded != "--project\x00chromium" {
			return true
		}
	}
	return false
}

func packageRunnerForRoot(root, packageRoot string) (string, error) {
	root = filepath.Clean(root)
	directory := filepath.Clean(packageRoot)
	for {
		locks := []struct {
			name   string
			runner string
		}{
			{name: "package-lock.json", runner: "npm"},
			{name: "pnpm-lock.yaml", runner: "pnpm"},
			{name: "yarn.lock", runner: "yarn"},
		}
		matched := []string{}
		runner := ""
		for _, lock := range locks {
			if exists(filepath.Join(directory, lock.name)) {
				matched = append(matched, lock.name)
				runner = lock.runner
			}
		}
		if len(matched) > 1 {
			relative, _ := filepath.Rel(root, directory)
			if relative == "" {
				relative = "."
			}
			return "", fmt.Errorf("package scope %s has ambiguous lockfiles: %s", filepath.ToSlash(relative), strings.Join(matched, ", "))
		}
		if runner != "" && (directory == filepath.Clean(packageRoot) || packageScopeOwnsPath(directory, packageRoot)) {
			return runner, nil
		}
		if directory == root {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		relative, err := filepath.Rel(root, parent)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			break
		}
		directory = parent
	}
	return "npm", nil
}

func packageScopeOwnsPath(scope, packageRoot string) bool {
	relative, err := filepath.Rel(scope, packageRoot)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return workspacePatternsMatch(workspacePatterns(scope), filepath.ToSlash(relative))
}

func workspacePatterns(scope string) []string {
	patterns := []string{}
	if data, err := os.ReadFile(filepath.Join(scope, "package.json")); err == nil {
		var document struct {
			Workspaces json.RawMessage `json:"workspaces"`
		}
		if json.Unmarshal(data, &document) == nil && len(document.Workspaces) > 0 {
			var direct []string
			if json.Unmarshal(document.Workspaces, &direct) == nil {
				patterns = append(patterns, direct...)
			} else {
				var nested struct {
					Packages []string `json:"packages"`
				}
				if json.Unmarshal(document.Workspaces, &nested) == nil {
					patterns = append(patterns, nested.Packages...)
				}
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(scope, "pnpm-workspace.yaml")); err == nil {
		var document struct {
			Packages []string `yaml:"packages"`
		}
		if yaml.Unmarshal(data, &document) == nil {
			patterns = append(patterns, document.Packages...)
		}
	}
	return patterns
}

func workspacePatternsMatch(patterns []string, relative string) bool {
	relative = strings.Trim(filepath.ToSlash(relative), "/")
	matched := false
	for _, raw := range patterns {
		pattern := strings.TrimSpace(filepath.ToSlash(raw))
		excluded := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "./")
		pattern = strings.Trim(pattern, "/")
		if pattern == "" {
			continue
		}
		expression, err := regexp.Compile(workspaceGlobExpression(pattern))
		if err == nil && expression.MatchString(relative) {
			matched = !excluded
		}
	}
	return matched
}

func workspaceGlobExpression(pattern string) string {
	var result strings.Builder
	result.WriteString("^")
	for index := 0; index < len(pattern); index++ {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					result.WriteString("(?:.*/)?")
				} else {
					result.WriteString(".*")
				}
			} else {
				result.WriteString("[^/]*")
			}
		case '?':
			result.WriteString("[^/]")
		default:
			if strings.ContainsRune(`.+()|[]{}^$\`, rune(character)) {
				result.WriteByte('\\')
			}
			result.WriteByte(character)
		}
	}
	result.WriteString("$")
	return result.String()
}

func packageScriptCommand(packagePath, runner, name string) []string {
	if packagePath == "." {
		return []string{runner, "run", name}
	}
	switch runner {
	case "pnpm":
		return []string{"pnpm", "--dir", packagePath, "run", name}
	case "yarn":
		return []string{"yarn", "--cwd", packagePath, "run", name}
	default:
		return []string{"npm", "--prefix", packagePath, "run", name}
	}
}

func fileContains(path, value string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(strings.ToLower(string(data)), strings.ToLower(value))
}

func uniqueTestCommands(commands [][]string) [][]string {
	seen := map[string]bool{}
	result := make([][]string, 0, len(commands))
	for _, command := range commands {
		key := strings.Join(command, "\x00")
		if !seen[key] {
			seen[key] = true
			result = append(result, command)
		}
	}
	return result
}

// testCommandPriority keeps generated repair validation plans dependency-safe.
// Repository producers (tests, suites, mutation runs) must execute before proof
// and validation scripts that consume their reports. Derived planning belongs
// last because it may consume both the primary and mutation evidence.
func testCommandPriority(command []string) int {
	value := strings.ToLower(strings.Join(command, " "))
	switch {
	case strings.Contains(value, "test-creation") || strings.Contains(value, "test_creation"):
		return 50
	case strings.Contains(value, "proof") || strings.Contains(value, "verify") || strings.Contains(value, "validation") || strings.Contains(value, "validate") || strings.Contains(value, "check-"):
		return 40
	case strings.Contains(value, "mutation") || strings.Contains(value, "mutate"):
		return 35
	case strings.Contains(value, "test:ci:lite"):
		return 25
	case strings.Contains(value, "suite") || strings.Contains(value, " e2e") || strings.Contains(value, " visual") || strings.Contains(value, " test") || strings.Contains(value, " run"):
		return 30
	case strings.Contains(value, "typecheck") || strings.Contains(value, "lint"):
		return 20
	case strings.Contains(value, " plan"):
		return 25
	case strings.Contains(value, " build"):
		return 10
	default:
		return 30
	}
}

func preferGranularTestCommands(commands [][]string) [][]string {
	hasGranularProducer := map[string]bool{}
	for _, command := range commands {
		packagePath, runner, _, ok := packageScriptCommandParts(command)
		if !ok {
			continue
		}
		name := commandScriptName(command)
		if !strings.Contains(name, "suite") && isEvidenceProducerScript(name) {
			hasGranularProducer[runner+"\x00"+packagePath] = true
		}
	}
	if len(hasGranularProducer) == 0 {
		return commands
	}
	result := make([][]string, 0, len(commands))
	for _, command := range commands {
		packagePath, runner, _, ok := packageScriptCommandParts(command)
		if ok && hasGranularProducer[runner+"\x00"+packagePath] && strings.Contains(commandScriptName(command), "suite") {
			continue
		}
		result = append(result, command)
	}
	return result
}

func commandScriptName(command []string) string {
	if len(command) == 0 {
		return ""
	}
	return strings.ToLower(command[len(command)-1])
}

func isEvidenceProducerScript(name string) bool {
	if name == "test" || name == "vh:run" || strings.Contains(name, "mutate") || strings.Contains(name, "e2e") || strings.Contains(name, "visual") {
		return true
	}
	return strings.Contains(name, "test") && !strings.Contains(name, "test-creation") && !strings.Contains(name, "proof") && !strings.Contains(name, "check") && !strings.Contains(name, "validate")
}

func mergeMaps(left, right map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func cloneScriptMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for name, body := range source {
		result[name] = body
	}
	return result
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
