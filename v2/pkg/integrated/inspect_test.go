package integrated

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInspectCheckoutBuildsRepositorySpecificSignals(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test":"vitest","typecheck":"tsc --noEmit"},"dependencies":{"react":"19.0.0"},"devDependencies":{"@playwright/test":"1.50.0"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)
	writeFixture(t, root, ".github/workflows/ci.yml", "name: ci")
	writeFixture(t, root, "src/auth/Login.tsx", "export const Login = 1")
	writeFixture(t, root, "visual-hive.baselines/linux/home.png", "png")
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(inspection.Languages, "TypeScript/JavaScript") || !contains(inspection.Frameworks, "React") || !contains(inspection.Frameworks, "Playwright") || !contains(inspection.PackageManagers, "npm") {
		t.Fatalf("missing detection signals: %+v", inspection)
	}
	if len(inspection.TestCommands) != 2 || len(inspection.CIFiles) != 1 || len(inspection.BaselineFiles) != 1 || len(inspection.HighRiskPaths) == 0 {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
}

func TestInspectCheckoutDiscoversNestedDashboardAndPythonTests(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"tests\"]")
	writeFixture(t, root, "tests/test_app.py", "def test_app(): assert True")
	writeFixture(t, root, "dashboard/package.json", `{"scripts":{"build":"vite build","test:unit":"vitest run","test:ci:lite":"npm run build && npm run test:unit"},"dependencies":{"react":"19.0.0","vite":"7.0.0"},"devDependencies":{"@playwright/test":"1.60.0"}}`)
	writeFixture(t, root, "dashboard/package-lock.json", `{"lockfileVersion":3}`)
	writeFixture(t, root, "dashboard/playwright.config.ts", "export default {}")
	writeFixture(t, root, "tests/__screenshots__/home.png", "baseline")

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(inspection.Languages, "Python") || !contains(inspection.Languages, "TypeScript/JavaScript") || !contains(inspection.Frameworks, "React") || !contains(inspection.Frameworks, "Playwright") {
		t.Fatalf("nested stack was not detected: %+v", inspection)
	}
	want := [][]string{{"npm", "--prefix", "dashboard", "run", "test:ci:lite"}, {"python", "-m", "pytest", "-q"}}
	if len(inspection.TestCommands) != len(want) {
		t.Fatalf("unexpected nested commands: %+v", inspection.TestCommands)
	}
	for index := range want {
		if strings.Join(inspection.TestCommands[index], " ") != strings.Join(want[index], " ") {
			t.Fatalf("command %d = %v, want %v", index, inspection.TestCommands[index], want[index])
		}
	}
	if len(inspection.BaselineFiles) != 1 || inspection.BaselineFiles[0] != "tests/__screenshots__/home.png" {
		t.Fatalf("nested screenshot baseline was not detected: %+v", inspection.BaselineFiles)
	}
}

func TestInspectCheckoutUsesEachPackageLockfileRunner(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "apps/pnpm/package.json", `{"scripts":{"test:unit":"vitest run"}}`)
	writeFixture(t, root, "apps/pnpm/pnpm-lock.yaml", "lockfileVersion: '9.0'")
	writeFixture(t, root, "apps/yarn/package.json", `{"scripts":{"test:unit":"vitest run"}}`)
	writeFixture(t, root, "apps/yarn/yarn.lock", "# yarn lockfile")

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCommand(inspection.TestCommands, "pnpm", "--dir", "apps/pnpm", "run", "test:unit") {
		t.Fatalf("pnpm workspace generated the wrong runner: %+v", inspection.TestCommands)
	}
	if !hasCommand(inspection.TestCommands, "yarn", "--cwd", "apps/yarn", "run", "test:unit") {
		t.Fatalf("yarn workspace generated the wrong runner: %+v", inspection.TestCommands)
	}
}

func TestInspectCheckoutInheritsNearestWorkspaceRunner(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "pnpm-lock.yaml", "lockfileVersion: '9.0'")
	writeFixture(t, root, "pnpm-workspace.yaml", "packages:\n  - packages/*\n")
	writeFixture(t, root, "package.json", `{"private":true}`)
	writeFixture(t, root, "packages/app/package.json", `{"scripts":{"test:unit":"vitest run"}}`)
	writeFixture(t, root, "examples/tool/package.json", `{"scripts":{"test:unit":"vitest run"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCommand(inspection.TestCommands, "pnpm", "--dir", "packages/app", "run", "test:unit") {
		t.Fatalf("nested package did not inherit the nearest pnpm lock: %+v", inspection.TestCommands)
	}
	if !hasCommand(inspection.TestCommands, "npm", "--prefix", "examples/tool", "run", "test:unit") {
		t.Fatalf("unrelated descendant incorrectly inherited the pnpm workspace lock: %+v", inspection.TestCommands)
	}
}

func TestInspectCheckoutRejectsAmbiguousPackageLocks(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test":"vitest run"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)
	writeFixture(t, root, "pnpm-lock.yaml", "lockfileVersion: '9.0'")
	if _, err := InspectCheckout(root, "main"); err == nil || !strings.Contains(err.Error(), "ambiguous lockfiles") {
		t.Fatalf("ambiguous package manager scope was accepted: %v", err)
	}
}

func TestTargetPackageInstallHandlesMixedLockedAndLocklessRoots(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	writeFixture(t, root, "package-lock.json", `{"name":"console","lockfileVersion":3,"requires":true,"packages":{}}`)
	writeFixture(t, root, "locked/package.json", `{}`)
	writeFixture(t, root, "locked/package-lock.json", `{}`)
	writeFixture(t, root, "lockless/package.json", `{}`)
	writeFixture(t, root, "workspace/package.json", `{"workspaces":["packages/*"]}`)
	writeFixture(t, root, "workspace/package-lock.json", `{}`)
	writeFixture(t, root, "workspace/packages/member/package.json", `{}`)
	writeFixture(t, root, "workspace/examples/tool/package.json", `{}`)
	writeFixture(t, root, "yarn-classic/package.json", `{}`)
	writeFixture(t, root, "yarn-classic/yarn.lock", "# classic")
	writeFixture(t, root, "yarn-berry/package.json", `{"packageManager":"yarn@4.9.1"}`)
	writeFixture(t, root, "yarn-berry/yarn.lock", "# berry")
	fakeBin := filepath.Join(root, "fake-bin")
	writeFixture(t, fakeBin, "npm", "#!/usr/bin/env bash\nprintf 'npm:%s:%s\\n' \"$(basename \"$PWD\")\" \"$*\" >> \"$INSTALL_LOG\"\n")
	writeFixture(t, fakeBin, "corepack", "#!/usr/bin/env bash\nexit 0\n")
	writeFixture(t, fakeBin, "yarn", "#!/usr/bin/env bash\nif [ \"${1:-}\" = --version ]; then case \"$PWD\" in *yarn-berry) echo 4.9.1 ;; *) echo 1.22.22 ;; esac; exit 0; fi\nprintf 'yarn:%s:%s\\n' \"$(basename \"$PWD\")\" \"$*\" >> \"$INSTALL_LOG\"\n")
	for _, executable := range []string{"npm", "corepack", "yarn"} {
		if err := os.Chmod(filepath.Join(fakeBin, executable), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(targetPackageInstallShell(), "\n")
	for index := range lines {
		lines[index] = strings.TrimPrefix(lines[index], "          ")
	}
	script := "export PATH=" + shellQuote(bashFilesystemPath(fakeBin)) + ":\"$PATH\"\n" + strings.Join(lines, "\n")
	command := exec.Command(bash, "-e", "-o", "pipefail", "-c", script)
	command.Dir = root
	command.Env = append(os.Environ(), "INSTALL_LOG="+bashFilesystemPath(filepath.Join(root, "install.log")))
	shellOutput, runErr := command.CombinedOutput()
	if runErr != nil {
		t.Fatalf("generated package install failed: %v\n%s", runErr, shellOutput)
	}
	log, err := os.ReadFile(filepath.Join(root, "install.log"))
	if err != nil {
		t.Fatal(err)
	}
	value := string(log)
	for _, expected := range []string{"npm:locked:ci", "npm:workspace:ci", "npm:lockless:install", "npm:tool:install", "yarn:yarn-classic:install --frozen-lockfile", "yarn:yarn-berry:install --immutable"} {
		if !strings.Contains(value, expected) {
			t.Fatalf("install plan omitted %q:\n%s", expected, value)
		}
	}
	if strings.Contains(value, "workspace/packages/member") {
		t.Fatalf("ancestor lock-owned workspace member was installed twice:\n%s\nshell output:\n%s", value, shellOutput)
	}
	if strings.Contains(value, "npm:"+filepath.Base(root)+":ci") {
		t.Fatalf("inert orphan npm lock was installed as a package scope:\n%s\nshell output:\n%s", value, shellOutput)
	}
	if !strings.Contains(string(shellOutput), "Ignoring provably inert orphan npm lock ./package-lock.json") {
		t.Fatalf("inert orphan npm lock was not reported deterministically:\n%s", shellOutput)
	}
	installShell := targetPackageInstallShell()
	for _, required := range []string{"yarn --version", "yarn install --immutable", "yarn install --frozen-lockfile"} {
		if !strings.Contains(installShell, required) {
			t.Fatalf("Yarn major-version compatibility missing %q:\n%s", required, installShell)
		}
	}
}

func TestTargetPackageInstallRejectsUnsafeOrphanLocks(t *testing.T) {
	bash := workflowBash(t)
	tests := []struct {
		name     string
		lockfile string
		content  string
	}{
		{name: "malformed npm", lockfile: "package-lock.json", content: `{`},
		{name: "missing packages", lockfile: "package-lock.json", content: `{"lockfileVersion":3}`},
		{name: "nonempty packages", lockfile: "package-lock.json", content: `{"lockfileVersion":3,"packages":{"node_modules/example":{"version":"1.0.0"}}}`},
		{name: "nonempty legacy dependencies", lockfile: "package-lock.json", content: `{"lockfileVersion":3,"packages":{},"dependencies":{"example":{"version":"1.0.0"}}}`},
		{name: "pnpm", lockfile: "pnpm-lock.yaml", content: "lockfileVersion: '9.0'\n"},
		{name: "yarn", lockfile: "yarn.lock", content: "# yarn\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, test.lockfile, test.content)
			lines := strings.Split(targetPackageInstallShell(), "\n")
			for index := range lines {
				lines[index] = strings.TrimPrefix(lines[index], "          ")
			}
			command := exec.Command(bash, "-e", "-o", "pipefail", "-c", strings.Join(lines, "\n"))
			command.Dir = root
			output, runErr := command.CombinedOutput()
			if runErr == nil {
				t.Fatalf("unsafe orphan lock was accepted:\n%s", output)
			}
			if !strings.Contains(string(output), "has no sibling package.json") &&
				!strings.Contains(string(output), "is not provably inert") {
				t.Fatalf("unsafe orphan lock failed without a clear diagnostic: %v\n%s", runErr, output)
			}
		})
	}
}

func TestTargetPackageInstallRevalidatesBeforeSkippingChangedScope(t *testing.T) {
	bash := workflowBash(t)
	root := t.TempDir()
	writeFixture(t, root, "aaa/package.json", `{}`)
	writeFixture(t, root, "aaa/package-lock.json", `{}`)
	writeFixture(t, root, "zzz/package.json", `{}`)
	writeFixture(t, root, "zzz/package-lock.json", `{"lockfileVersion":3,"packages":{"node_modules/example":{"version":"1.0.0"}}}`)
	fakeBin := filepath.Join(root, "fake-bin")
	writeFixture(t, fakeBin, "npm", "#!/usr/bin/env bash\nif [ \"$(basename \"$PWD\")\" = aaa ]; then rm -f \"$MUTATE_TARGET\"; fi\n")
	if err := os.Chmod(filepath.Join(fakeBin, "npm"), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(targetPackageInstallShell(), "\n")
	for index := range lines {
		lines[index] = strings.TrimPrefix(lines[index], "          ")
	}
	script := "export PATH=" + shellQuote(bashFilesystemPath(fakeBin)) + ":\"$PATH\"\n" + strings.Join(lines, "\n")
	command := exec.Command(bash, "-e", "-o", "pipefail", "-c", script)
	command.Dir = root
	command.Env = append(os.Environ(), "MUTATE_TARGET="+bashFilesystemPath(filepath.Join(root, "zzz", "package.json")))
	output, runErr := command.CombinedOutput()
	if runErr == nil {
		t.Fatalf("scope changed after validation was silently skipped:\n%s", output)
	}
	if !strings.Contains(string(output), "is not provably inert") {
		t.Fatalf("changed scope failed without the inert-lock diagnostic: %v\n%s", runErr, output)
	}
}

func TestInspectCheckoutDetectsPlaywrightAndVisualHiveReviewedBaselines(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{}`)
	writeFixture(t, root, "e2e/dashboard.spec.ts-snapshots/dashboard.png", "playwright baseline")
	writeFixture(t, root, ".visual-hive/snapshots/dashboard.png", "visual hive baseline")
	writeFixture(t, root, ".visual-hive/snapshots/unreviewed.png", "uncommitted bootstrap output")
	writeFixture(t, root, "e2e/dashboard.spec.ts-snapshots/dashboard-actual.png", "tracked actual artifact")
	writeFixture(t, root, "e2e/dashboard.spec.ts-snapshots/dashboard-diff.png", "tracked diff artifact")
	writeFixture(t, root, ".visual-hive/artifacts/dashboard-actual.png", "transient evidence")
	if _, err := git(context.Background(), root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"package.json",
		"e2e/dashboard.spec.ts-snapshots/dashboard.png",
		".visual-hive/snapshots/dashboard.png",
		"e2e/dashboard.spec.ts-snapshots/dashboard-actual.png",
		"e2e/dashboard.spec.ts-snapshots/dashboard-diff.png",
	} {
		if _, err := git(context.Background(), root, "add", "-f", "--", path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := git(context.Background(), root, "-c", "user.name=Hive Inspect Test", "-c", "user.email=hive-inspect@example.test", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".visual-hive/snapshots/dashboard.png",
		"e2e/dashboard.spec.ts-snapshots/dashboard.png",
	}
	if strings.Join(inspection.BaselineFiles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("reviewed baselines = %v, want %v", inspection.BaselineFiles, want)
	}
}

func TestInspectCheckoutKeepsRepositoryChecksOutOfVisualHiveLifecycle(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"vh:test-creation":"node test-creation.js","vh:suite":"node suite.js","vh:mutation-proof":"node proof.js","vh:mutate":"node mutate.js","vh:run":"node run.js","vh:plan":"node plan.js","vh:build-source":"node build-visual-hive-source.mjs","typecheck":"tsc --noEmit","build":"vite build"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "typecheck"}
	if len(inspection.TestCommands) != len(want) {
		t.Fatalf("unexpected commands: %+v", inspection.TestCommands)
	}
	for index, name := range want {
		command := inspection.TestCommands[index]
		if len(command) != 3 || command[2] != name {
			t.Fatalf("command %d = %v, want npm run %s", index, command, name)
		}
	}
}

func TestInspectCheckoutRejectsPersistentMockAPIWithoutDroppingAPIEvidence(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"mock-api":"node scripts/mock-api.mjs","test:api":"node --test api.test.js","api-smoke":"node scripts/api-smoke.mjs"}}`)
	writeFixture(t, root, "package-lock.json", `{}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if hasCommandNamed(inspection.TestCommands, "mock-api") {
		t.Fatalf("persistent mock API was selected as an isolated test: %+v", inspection.TestCommands)
	}
	if !hasCommandNamed(inspection.TestCommands, "test:api") || !hasCommandNamed(inspection.TestCommands, "api-smoke") {
		t.Fatalf("explicit API evidence commands were dropped: %+v", inspection.TestCommands)
	}
}

func TestUnsafeLiteDoesNotSuppressSafeUnitAndBuildCommands(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:unit":"vitest run","test:ci:lite":"npm run test:unit || true","test:masked":"vitest run | tee results.txt"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCommandNamed(inspection.TestCommands, "build") || !hasCommandNamed(inspection.TestCommands, "test:unit") {
		t.Fatalf("unsafe lite wrapper suppressed safe commands: %+v", inspection.TestCommands)
	}
	if hasCommandNamed(inspection.TestCommands, "test:ci:lite") || hasCommandNamed(inspection.TestCommands, "test:masked") {
		t.Fatalf("failure-masking command was selected as evidence: %+v", inspection.TestCommands)
	}
}

func TestGranularProducerDoesNotSuppressAnotherPackageSuite(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "apps/a/package.json", `{"scripts":{"test:unit":"vitest run"}}`)
	writeFixture(t, root, "apps/b/package.json", `{"scripts":{"test:suite":"node suite.js"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCommand(inspection.TestCommands, "npm", "--prefix", "apps/a", "run", "test:unit") || !hasCommand(inspection.TestCommands, "npm", "--prefix", "apps/b", "run", "test:suite") {
		t.Fatalf("repository-global suite suppression dropped a package: %+v", inspection.TestCommands)
	}
}

func TestCoverageDepthFiltersExpensiveAndUnsafeScripts(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:unit":"vitest run","test:e2e":"playwright test","test:e2e:update":"playwright test --update-snapshots","test:perf":"node perf.js","test:watch":"vitest --watch"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	essential := testCommandsForCoverage(inspection, CoverageEssential)
	standard := testCommandsForCoverage(inspection, CoverageStandard)
	comprehensive := testCommandsForCoverage(inspection, CoverageComprehensive)
	if hasCommandNamed(essential, "test:e2e") || hasCommandNamed(essential, "test:perf") || !hasCommandNamed(essential, "test:unit") {
		t.Fatalf("essential depth was not bounded: %+v", essential)
	}
	if !hasCommandNamed(standard, "test:e2e") || hasCommandNamed(standard, "test:perf") {
		t.Fatalf("standard depth was not distinct: %+v", standard)
	}
	if !hasCommandNamed(comprehensive, "test:perf") {
		t.Fatalf("comprehensive depth omitted bounded deep checks: %+v", comprehensive)
	}
	for _, commands := range [][][]string{essential, standard, comprehensive} {
		if hasCommandNamed(commands, "test:e2e:update") || hasCommandNamed(commands, "test:watch") {
			t.Fatalf("unsafe interactive/update script was admitted: %+v", commands)
		}
	}
}

func TestEssentialCoveragePrefersSafeMatchingCheckOverBuildWriter(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "AvProj/package.json", `{"scripts":{"build:css":"node scripts/build-tailwind.mjs","check:css":"node scripts/build-tailwind.mjs --check","build:assets":"node scripts/build-assets.mjs"}}`)
	writeFixture(t, root, "Other/package.json", `{"scripts":{"build:css":"node scripts/build-tailwind.mjs"}}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageEssential)
	if !hasCommand(commands, "npm", "--prefix", "AvProj", "run", "check:css") {
		t.Fatalf("safe CSS verifier was not selected: %+v", commands)
	}
	if hasCommand(commands, "npm", "--prefix", "AvProj", "run", "build:css") {
		t.Fatalf("matching CSS writer remained selected beside its verifier: %+v", commands)
	}
	for _, want := range [][]string{
		{"npm", "--prefix", "AvProj", "run", "build:assets"},
		{"npm", "--prefix", "Other", "run", "build:css"},
	} {
		if !hasCommand(commands, want...) {
			t.Fatalf("unmatched writer %v was suppressed: %+v", want, commands)
		}
	}
}

func TestEssentialCoveragePrefersProvenBaselineAwareLintCheck(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "web/package.json", `{"scripts":{"lint":"eslint .","lint:check":"node scripts/lint-baseline-check.mjs"}}`)
	writeFixture(t, root, "web/.eslint-baseline.json", `[]`)
	writeFixture(t, root, "web/scripts/lint-baseline-check.mjs", `
import { execSync } from "node:child_process";
const baseline = ".eslint-baseline.json";
execSync("npx eslint . --format json");
const newEntries = [];
if (newEntries.length > 0) process.exit(1);
process.exit(0);
`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageEssential)
	if !hasCommand(commands, "npm", "--prefix", "web", "run", "lint:check") {
		t.Fatalf("baseline-aware lint verifier was not selected: %+v", commands)
	}
	if hasCommand(commands, "npm", "--prefix", "web", "run", "lint") {
		t.Fatalf("redundant raw lint remained beside its proven verifier: %+v", commands)
	}
}

func TestUnprovenBaselineLintNeverSuppressesRawLint(t *testing.T) {
	for _, source := range []string{
		`process.exit(0)`,
		`execSync("npx eslint src --format json"); const newEntries = []; if (newEntries.length > 0) process.exit(1); process.exit(0)`,
	} {
		root := t.TempDir()
		writeFixture(t, root, "package.json", `{"scripts":{"lint":"eslint .","lint:check":"node scripts/lint-baseline-check.mjs"}}`)
		writeFixture(t, root, ".eslint-baseline.json", `[]`)
		writeFixture(t, root, "scripts/lint-baseline-check.mjs", source)
		inspection, err := InspectCheckout(root, "main")
		if err != nil {
			t.Fatal(err)
		}
		commands := testCommandsForCoverage(inspection, CoverageEssential)
		if !hasCommandNamed(commands, "lint") || !hasCommandNamed(commands, "lint:check") {
			t.Fatalf("unproven verifier suppressed lint execution: %+v", commands)
		}
	}
}

func TestBuildWriterRequiresSafeSelectedRankOneCheckSibling(t *testing.T) {
	for _, test := range []struct {
		name     string
		scripts  string
		coverage Coverage
		build    string
	}{
		{
			name:     "unsafe check is not admitted",
			scripts:  `{"scripts":{"build:css":"node build-css.mjs","check:css":"node check-css.mjs || true"}}`,
			coverage: CoverageEssential,
			build:    "build:css",
		},
		{
			name:     "rank two check cannot replace writer",
			scripts:  `{"scripts":{"build:visual":"node build-visual.mjs","check:visual":"node build-visual.mjs --check"}}`,
			coverage: CoverageStandard,
			build:    "build:visual",
		},
		{
			name:     "no-op check is not equivalent",
			scripts:  `{"scripts":{"build:css":"node build-css.mjs","check:css":"echo ok"}}`,
			coverage: CoverageEssential,
			build:    "build:css",
		},
		{
			name:     "unrelated check is not equivalent",
			scripts:  `{"scripts":{"build:css":"node build-css.mjs","check:css":"node unrelated.mjs --check"}}`,
			coverage: CoverageEssential,
			build:    "build:css",
		},
		{
			name:     "transitive writer is not an exact verifier",
			scripts:  `{"scripts":{"build:css":"node build-css.mjs","check:css":"npm run build:css -- --check"}}`,
			coverage: CoverageEssential,
			build:    "build:css",
		},
		{
			name:     "chained build body remains a writer",
			scripts:  `{"scripts":{"build:css":"node preflight.mjs && node build-css.mjs","check:css":"node preflight.mjs && node build-css.mjs --check"}}`,
			coverage: CoverageEssential,
			build:    "build:css",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "package.json", test.scripts)
			inspection, err := InspectCheckout(root, "main")
			if err != nil {
				t.Fatal(err)
			}
			commands := testCommandsForCoverage(inspection, test.coverage)
			if !hasCommandNamed(commands, test.build) {
				t.Fatalf("writer %q was suppressed without a safe selected rank-one sibling: %+v", test.build, commands)
			}
		})
	}
}

func TestSafeAutomationScriptRejectsNonProductionBrowserLanes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "test:auth-drift", body: "playwright test --config auth.config.ts"},
		{name: "test:e2e:debug", body: "playwright test --debug"},
		{name: "test:e2e:report", body: "playwright show-report"},
		{name: "test:visual:live", body: "playwright test --grep @live-site"},
		{name: "test:visual:browser-matrix", body: "playwright test --config browser-matrix.config.ts"},
		{name: "test:visual:browser-matrix:compare", body: "node compare-browser-matrix.cjs"},
		{name: "test:visual:macos-popup", body: "playwright test --config macos-popup.config.ts"},
		{name: "test:visual:report:pr", body: "node build-report.cjs"},
		{name: "test:e2e:ux-sweep", body: "PLAYWRIGHT_STORAGE_STATE=auth.json playwright test --config ux.config.ts"},
		{name: "test:e2e:firefox", body: "playwright test --project=firefox"},
		{name: "test:e2e:webkit", body: "playwright test --project=webkit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if safeAutomationScript(test.name, test.body) {
				t.Fatalf("non-production browser lane %q was admitted", test.name)
			}
		})
	}
}

func TestSafeAutomationScriptRetainsBoundedLocalEvidenceLanes(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "test:liveness", body: "node liveness.test.js"},
		{name: "test:reporter-contract", body: "node reporter.test.js"},
		{name: "test:e2e:chromium", body: "playwright test --project=chromium"},
		{name: "test:e2e:fullstack", body: "cd .. && bash scripts/run-fullstack-e2e.sh"},
		{name: "test:visual:mutations", body: "playwright test --config intensive.config.ts --grep @mutation"},
		{name: "test:visual:adequacy", body: "node harness/analyze-test-directory.cjs"},
		{name: "test:visual:pr", body: "playwright test --config pr.config.ts"},
		{name: "test:e2e:perf:ttfi:gate", body: "npm run test:e2e:perf:ttfi && node compare-ttfi.mjs"},
		{name: "test:e2e:ui-compliance:gate", body: "npm run test:e2e:ui-compliance && node compare-compliance.mjs"},
		{name: "test:visual:groundtruth", body: "playwright test --project=semantic-groundtruth"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !safeAutomationScript(test.name, test.body) {
				t.Fatalf("bounded local evidence lane %q was rejected", test.name)
			}
		})
	}
}

func TestPackageScriptCommandsPrefersExplicitChromiumOverUnscopedPlaywright(t *testing.T) {
	commands := packageScriptCommands(".", "npm", map[string]string{
		"test:e2e":          "npm run build && playwright test",
		"test:e2e:chromium": "playwright test --project=chromium",
	})
	if hasCommandNamed(commands, "test:e2e") || !hasCommandNamed(commands, "test:e2e:chromium") {
		t.Fatalf("unscoped Playwright lane was not replaced by its explicit Chromium lane: %+v", commands)
	}
}

func TestComprehensiveCoverageDoesNotReintroduceSupersededUnscopedPlaywright(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:e2e":"playwright test","test:e2e:chromium":"playwright test --project=chromium","test:all":"npm run test:e2e && npm run test:e2e:chromium"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !invokesSupersededUnscopedPlaywrightE2E("test:all", inspection.packageScripts["."]) {
		t.Fatal("aggregate's unscoped Playwright terminal was not detected")
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if hasCommandNamed(commands, "test:e2e") || hasCommandNamed(commands, "test:all") || !hasCommandNamed(commands, "test:e2e:chromium") {
		t.Fatalf("comprehensive aggregation reintroduced the superseded multi-browser lane: %+v", commands)
	}
}

func TestComprehensiveCoverageRejectsWrapperAroundFilteredBrowserLanes(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:e2e":"playwright test","test:e2e:chromium":"playwright test --project=chromium","test:auth-drift":"playwright test --config auth.config.ts","test:all":"npm run test:e2e && npm run test:auth-drift && npm run test:e2e:chromium"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	for _, rejected := range []string{"test:e2e", "test:auth-drift", "test:all"} {
		if hasCommandNamed(commands, rejected) {
			t.Fatalf("filtered browser lane %q re-entered through its wrapper: %+v", rejected, commands)
		}
	}
	if !hasCommandNamed(commands, "test:e2e:chromium") {
		t.Fatalf("safe Chromium lane was lost with its unsafe wrapper: %+v", commands)
	}
}

func TestPackageScriptCommandsRetainsExplicitlyForwardedChromiumWrapper(t *testing.T) {
	commands := packageScriptCommands(".", "npm", map[string]string{
		"test:e2e":          "playwright test",
		"test:e2e:chromium": "playwright test --project=chromium",
		"test:e2e:ci":       "npm run test:e2e -- --project=chromium",
	})
	if !hasCommandNamed(commands, "test:e2e:ci") {
		t.Fatalf("explicit Chromium wrapper was dropped: %+v", commands)
	}
}

func TestPackageScriptCommandsRetainsUnscopedPlaywrightWithoutSafeChromiumAlternative(t *testing.T) {
	for name, scripts := range map[string]map[string]string{
		"only unscoped": {
			"test:e2e": "playwright test",
		},
		"unsafe alternative": {
			"test:e2e":          "playwright test",
			"test:e2e:chromium": "playwright test --project=firefox",
		},
		"configured default": {
			"test:e2e":          "playwright test --config local.config.ts",
			"test:e2e:chromium": "playwright test --project=chromium",
		},
	} {
		t.Run(name, func(t *testing.T) {
			commands := packageScriptCommands(".", "npm", scripts)
			if !hasCommandNamed(commands, "test:e2e") {
				t.Fatalf("repository's only independently scoped Playwright lane was dropped: %+v", commands)
			}
		})
	}
}

func hasCommandNamed(commands [][]string, name string) bool {
	for _, command := range commands {
		if commandScriptName(command) == name {
			return true
		}
	}
	return false
}

func TestInspectCheckoutRetainsSuiteWhenItIsOnlyProducer(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:suite":"node suite.js"}}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.TestCommands) != 2 || inspection.TestCommands[1][2] != "test:suite" {
		t.Fatalf("standalone suite should be retained: %+v", inspection.TestCommands)
	}
}

func TestBuildSetupPlanKeepsCoverageAndAuthoritySeparate(t *testing.T) {
	plan := buildSetupPlan(SetupOptions{Repository: "owner/repo", Coverage: CoverageComprehensive, Automation: AutomationIssues, Provider: "codex", VisualHive: true, MaxActiveIssues: 5, MaxRepairAttempts: 3}, RepositoryInspection{DefaultBranch: "main"})
	if plan.Coverage != CoverageComprehensive || plan.Automation != AutomationIssues || plan.ACMMLevel != 4 {
		t.Fatalf("dimensions were conflated: %+v", plan)
	}
	if plan.MaxActiveIssues != 5 {
		t.Fatalf("active issue WIP limit was not preserved: %+v", plan)
	}
	if plan.MaxRepairAttempts != 3 {
		t.Fatalf("repair attempt limit was not preserved: %+v", plan)
	}
	if len(plan.TestingLayers) < 10 || !plan.ReadOnly {
		t.Fatalf("comprehensive read-only plan incomplete: %+v", plan)
	}
}

func TestComprehensiveSetupBootstrapsNodeTestWithoutWeakeningAutoMergePaths(t *testing.T) {
	inspection := RepositoryInspection{
		Languages:    []string{"TypeScript/JavaScript"},
		TestCommands: [][]string{{"npm", "run", "build"}, {"npm", "run", "vh:run"}},
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("comprehensive Node setup must include the zero-test-safe built-in runner: %+v", commands)
	}
	if hasCommand(testCommandsForCoverage(inspection, CoverageStandard), "node", "--test") {
		t.Fatal("standard coverage must not add the comprehensive unit-test bootstrap")
	}
	if contains(defaultAllowedAutoMergePaths(), "package.json") {
		t.Fatal("test bootstrap must not broaden auto-merge to package metadata")
	}
}

func TestVisualTestConfigRequiresHumanMergeAuthority(t *testing.T) {
	if !contains(defaultAllowedRepairPaths(), "visual-hive.config.yaml") {
		t.Fatal("repair PR mode must be able to propose repository testing-plan improvements")
	}
	if contains(defaultAllowedAutoMergePaths(), "visual-hive.config.yaml") {
		t.Fatal("Visual Hive config changes must remain outside the default auto-merge allowlist")
	}
}

func TestScriptsTestingDefaultsRequireHumanMergeAuthority(t *testing.T) {
	repairPaths := defaultAllowedRepairPaths()
	autoMergePaths := defaultAllowedAutoMergePaths()
	for _, pattern := range []string{"scripts/testing/**", "**/scripts/testing/**"} {
		if !contains(repairPaths, pattern) {
			t.Fatalf("repair PR mode must allow repository test infrastructure through %q", pattern)
		}
		if contains(autoMergePaths, pattern) {
			t.Fatalf("repository test infrastructure %q must remain outside the default auto-merge allowlist", pattern)
		}
	}
}

func TestExactCommitPinRejectsAbbreviatedOrDifferentRefs(t *testing.T) {
	sha := "0123456789012345678901234567890123456789"
	if !exactCommitPin(sha, sha) {
		t.Fatal("exact immutable commit should match")
	}
	if exactCommitPin(sha[:8], sha) || exactCommitPin(sha, "1123456789012345678901234567890123456789") {
		t.Fatal("abbreviated or different refs must not match")
	}
}

func TestWorkflowUsesTwoArtifactProvenanceAndPinnedActions(t *testing.T) {
	config := Config{DefaultBranch: "main", VisualHiveRepo: "owner/visual-hive", VisualHiveRef: "0123456789012345678901234567890123456789", ACMMLevel: 4, TestCommands: [][]string{{"node", "--test"}, {"npm", "--prefix", "dashboard", "run", "test:ci:lite"}, {"python", "-m", "pytest", "-q"}}}
	value := workflow(config)
	for _, required := range []string{checkoutActionSHA, setupNodeActionSHA, setupPythonActionSHA, uploadArtifactActionSHA, downloadArtifactActionSHA, "name: Hive repository test 001", "name: Hive repository test 002", "name: Hive repository test 003", "visual-hive-execution", "visual-hive-production", "setup-baseline-capture", "setup-baseline-verify", "--bootstrap-baselines", "hive-setup-baselines-${{ inputs.hive_dispatch_id }}", "setup-baseline-manifest.json", `runner: "ubuntu-latest"`, "sudo -u hive-target -- env -i", "env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY", "npm --prefix dashboard run test:ci:lite", "python -m pytest -q", "find . -name package-lock.json", `python -m pip install "$scope"`, "steps.evidence.outputs.artifact-id", "visual-hive-raw-${{ github.run_id }}", "visual-hive-bundle-${{ github.run_id }}", "hive-runner-outcome.json", "plan and report lack an exact one-to-one contract evaluation binding", "evaluated-contracts.txt", "authoritative-resolution.txt", "baselines list", "Raw evidence contains a symbolic link", "--authoritative-for-resolution"} {
		if !containsString(value, required) {
			t.Fatalf("workflow missing %q", required)
		}
	}
	if !containsString(value, "pipeline-exit-code.txt") || !containsString(value, "set +e") {
		t.Fatal("workflow must publish evidence after a deterministic red verdict")
	}
	credentiallessCheckouts := 0
	for jobName, job := range parseIsolatedWorkflow(t, value).Jobs {
		for _, step := range job.Steps {
			if step.Uses != "actions/checkout@"+checkoutActionSHA {
				continue
			}
			credentiallessCheckouts++
			if persist, exists := step.With["persist-credentials"]; !exists || persist != false {
				t.Fatalf("production workflow checkout in job %q retained credentials: %v", jobName, persist)
			}
		}
	}
	if credentiallessCheckouts != len(config.TestCommands)+9 {
		t.Fatalf("production workflow expanded to %d credentialless checkouts, want %d", credentiallessCheckouts, len(config.TestCommands)+9)
	}
	if strings.Count(value, "include-hidden-files: true") != 5 {
		t.Fatal("raw, independently verified, and lifecycle-bundle uploads must opt in to hidden .visual-hive artifacts")
	}
	if !containsString(value, "hive integration-smoke") {
		t.Fatal("workflow must validate Hive import artifacts before finalizing a lifecycle bundle")
	}
	if !containsString(value, "playwright_cli") || !containsString(value, "install --with-deps chromium") || !containsString(value, "--skip-install") {
		t.Fatal("workflow must install the target Playwright browser revision once before the production scan")
	}
	if !containsString(value, "--acmm-request 4") {
		t.Fatal("workflow must bind bundle authority to Hive's configured ACMM level")
	}
	if !containsString(value, "node --test") {
		t.Fatal("production workflow must execute the repository unit-test bootstrap")
	}
	if !containsString(value, "needs:\n      - visual-hive-execution\n      - repository-test-001") || !containsString(value, "if: ${{ always() && inputs.hive_operation == 'production' }}") {
		t.Fatal("repository tests and isolated target collection must be runner-owned dependencies of the fresh verifier")
	}
	for _, jobID := range []string{"repository-test-001", "repository-test-002", "repository-test-003"} {
		start := strings.Index(value, "  "+jobID+":\n")
		if start < 0 {
			t.Fatalf("production workflow is missing %s", jobID)
		}
		rest := value[start+1:]
		end := strings.Index(rest, "\n  ")
		if end < 0 {
			end = len(rest)
		}
		if strings.Contains(rest[:end], "continue-on-error: true") {
			t.Fatalf("repository test %s is not a terminal runner-owned job", jobID)
		}
	}
	if containsString(value, "pull_request_target") || containsString(value, "issues: write") || containsString(value, "pull-requests: write") {
		t.Fatalf("workflow has an unsafe write lane:\n%s", value)
	}
	if containsString(value, "schedule:") || containsString(value, "push:") {
		t.Fatalf("production workflow must be dispatch-only so every run has exactly one Hive consumer:\n%s", value)
	}
	var document any
	if err := yaml.Unmarshal([]byte(value), &document); err != nil || strings.Contains(value, "%!") {
		t.Fatalf("generated production workflow is invalid: %v\n%s", err, value)
	}
}

func TestComprehensiveCoverageKeepsNestedCIUnitSuiteWithoutNodeFallback(t *testing.T) {
	inspection := RepositoryInspection{
		Languages:    []string{"TypeScript/JavaScript"},
		TestCommands: [][]string{{"npm", "--prefix", "dashboard", "run", "test:ci:lite"}},
		packageScripts: map[string]map[string]string{
			"dashboard": {"test:ci:lite": "npm run test:unit", "test:unit": "vitest run"},
		},
		packageRunners: map[string]string{"dashboard": "npm"},
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if len(commands) != 1 || strings.Join(commands[0], " ") != "npm --prefix dashboard run test:ci:lite" {
		t.Fatalf("unexpected comprehensive commands: %+v", commands)
	}
}

func TestComprehensiveCoverageAddsUnitFallbackWhenCIScriptHasNoUnitRunner(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:e2e":"playwright test","test:ci:lite":"npm run build && npm run test:e2e"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("generic CI script without a unit runner suppressed fallback: %+v", commands)
	}
}

func TestComprehensiveCoverageDoesNotTreatRunnerNameInProseAsExecution(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:unit":"echo vitest is not configured","test:ci:lite":"npm run test:unit"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("runner name in prose suppressed the unit fallback: %+v", commands)
	}
}

func TestComprehensiveCoverageDoesNotTreatRunnerNameInScriptNameAsExecution(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:vitest-docs":"echo runner is not configured"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("runner name in a package script name suppressed the unit fallback: %+v", commands)
	}
}

func TestComprehensiveCoverageDoesNotTrustForwardedUnitHelpInvocation(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:unit":"vitest run","test:ci:lite":"npm run test:unit -- --help"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("forwarded unit help invocation suppressed the unit fallback: %+v", commands)
	}
}

func TestComprehensiveCoverageDoesNotTrustUnitRunnerHelpMode(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:unit":"vitest --help"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommand(commands, "node", "--test") {
		t.Fatalf("unit runner help mode suppressed the unit fallback: %+v", commands)
	}
}

func TestComprehensiveCoverageRequiresExactForwardedInvocationWithoutFullSelection(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:unit":"vitest run","test:ci:lite":"npm run test:unit -- --help","test:all":"npm run test:unit && npm run build"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if hasCommandNamed(commands, "test:all") || !hasCommand(commands, "node", "--test") {
		t.Fatalf("unfiltered aggregate was treated as exact for a forwarded-only selection: %+v", commands)
	}
}

func TestComprehensiveCoverageCandidateCannotSelfMaskForwardedRequirement(t *testing.T) {
	inspection := RepositoryInspection{
		Languages: []string{"TypeScript/JavaScript"},
		TestCommands: [][]string{
			{"npm", "run", "test:all"},
			{"npm", "run", "test:e2e:smoke"},
		},
		packageScripts: map[string]map[string]string{
			".": {
				"build":          "vite build",
				"test:unit":      "vitest run",
				"test:e2e:smoke": "npm run test:unit -- --help",
				"test:all":       "npm run test:unit && npm run build",
			},
		},
		packageRunners: map[string]string{".": "npm"},
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if !hasCommandNamed(commands, "test:e2e:smoke") {
		t.Fatalf("candidate self-coverage masked a forwarded-only requirement: %+v", commands)
	}
}

func TestComprehensiveCoveragePrefersProvenRepositoryAggregateWithoutDuplication(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"tests\"]")
	writeFixture(t, root, "tests/test_api.py", "def test_api(): assert True")
	writeFixture(t, root, "dashboard/package-lock.json", `{}`)
	writeFixture(t, root, "dashboard/package.json", `{
		"scripts": {
			"format:check": "prettier --check .",
			"lint": "eslint .",
			"typecheck": "tsc --noEmit",
			"build": "vite build",
			"test:unit": "vitest run",
			"test:coverage": "vitest run --coverage",
			"test:e2e": "playwright test --project=e2e",
			"test:a11y": "playwright test --project=a11y",
			"test:security": "npm audit",
			"test:perf": "lhci autorun",
			"test:visual": "playwright test --project=visual",
			"test:ci:lite": "npm run build && npm run test:unit && npm run test:e2e -- --grep=smoke && npm run test:a11y -- --grep=smoke",
			"test:ci:heavy": "npm run format:check && npm run lint && npm run typecheck && npm run test:coverage && npm run build && npm run test:e2e && npm run test:a11y && npm run test:security && npm run test:perf",
			"test:all": "npm run test:ci:heavy && npm run test:visual"
		}
	}`)

	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	want := [][]string{{"npm", "--prefix", "dashboard", "run", "test:all"}, {"python", "-m", "pytest", "-q"}}
	if len(commands) != len(want) {
		t.Fatalf("comprehensive plan is duplicative: %+v", commands)
	}
	for index := range want {
		if strings.Join(commands[index], "\x00") != strings.Join(want[index], "\x00") {
			t.Fatalf("command %d = %v, want %v", index, commands[index], want[index])
		}
	}
}

func TestComprehensiveCoverageRejectsAggregateThatOmitsSelectedLeaf(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package-lock.json", `{}`)
	writeFixture(t, root, "package.json", `{"scripts":{"build":"vite build","test:unit":"vitest run","test:e2e":"playwright test","test:ci:lite":"npm run build && npm run test:unit","test:all":"npm run build && npm run test:unit"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if hasCommandNamed(commands, "test:all") || !hasCommandNamed(commands, "test:e2e") {
		t.Fatalf("unproven aggregate replaced selected leaf tests: %+v", commands)
	}
}

func TestComprehensiveCoverageRejectsTextualOrConditionalAggregateEdges(t *testing.T) {
	for _, aggregate := range []string{
		"echo npm run test:e2e && npm run test:unit",
		"npm run test:e2e || npm run test:unit",
		"npm run test:e2e | npm run test:unit",
	} {
		root := t.TempDir()
		writeFixture(t, root, "package.json", `{"scripts":{"test:unit":"vitest run","test:e2e":"playwright test","test:ci:lite":"npm run test:unit","test:all":`+strconv.Quote(aggregate)+`}}`)
		inspection, err := InspectCheckout(root, "main")
		if err != nil {
			t.Fatal(err)
		}
		commands := testCommandsForCoverage(inspection, CoverageComprehensive)
		if hasCommandNamed(commands, "test:all") {
			t.Fatalf("non-unconditional aggregate %q was trusted: %+v", aggregate, commands)
		}
	}
}

func TestComprehensiveCoverageRejectsFilteredAggregateForFullSelectedLeaf(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "package.json", `{"scripts":{"test:unit":"vitest run","test:e2e":"playwright test","test:ci:lite":"npm run test:unit","test:all":"npm run test:e2e -- --grep=smoke && npm run test:unit"}}`)
	inspection, err := InspectCheckout(root, "main")
	if err != nil {
		t.Fatal(err)
	}
	commands := testCommandsForCoverage(inspection, CoverageComprehensive)
	if hasCommandNamed(commands, "test:all") || !hasCommandNamed(commands, "test:e2e") {
		t.Fatalf("filtered aggregate invocation replaced a full selected leaf: %+v", commands)
	}
}

func TestRepositoryTestShellCapturesFailureAndContinues(t *testing.T) {
	bash := workflowBash(t)
	config := Config{TestCommands: [][]string{{"bash", "-c", "exit 7"}, {"bash", "-c", "printf continued > continued.txt"}}}
	lines := strings.Split(repositoryTestShell(config), "\n")
	for index := range lines {
		lines[index] = strings.TrimPrefix(lines[index], "          ")
	}
	command := exec.Command(bash, "-e", "-o", "pipefail", "-c", strings.Join(lines, "\n"))
	command.Dir = t.TempDir()
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("generated shell aborted before evidence capture: %v\n%s", runErr, output)
	}
	result, err := os.ReadFile(filepath.Join(command.Dir, ".visual-hive", "repository-test-exit-code.txt"))
	if err != nil || strings.TrimSpace(string(result)) != "7" {
		t.Fatalf("repository exit artifact = %q, err %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(command.Dir, "continued.txt")); err != nil {
		t.Fatalf("later repository command did not run: %v", err)
	}
	pipelineSentinel, err := os.ReadFile(filepath.Join(command.Dir, ".visual-hive", "pipeline-exit-code.txt"))
	if err != nil || strings.TrimSpace(string(pipelineSentinel)) != "1" {
		t.Fatalf("pipeline sentinel = %q, err %v", pipelineSentinel, err)
	}
}

func TestPullRequestWorkflowIsReadOnlyPinnedAndVerdictEnforcing(t *testing.T) {
	value := pullRequestWorkflow(Config{RepositoryID: "123", DefaultBranch: "main", SetupBranch: "hive/setup-123", SetupAuthorizationActorID: 456, VisualHive: true, VisualHiveRepo: "owner/visual-hive", VisualHiveRef: "0123456789012345678901234567890123456789", TestCommands: [][]string{{"node", "--test"}}})
	for _, required := range []string{checkoutActionSHA, setupNodeActionSHA, setupPythonActionSHA, uploadArtifactActionSHA, downloadArtifactActionSHA, "name: Hive repository test 001", "node --test", "visual-hive-execution", "visual-hive-pr-raw-${{ github.run_id }}", "name: visual-hive-pr", "pipeline-exit-code.txt", "Repository test plan failed", "Enforce deterministic verdict", "baselines list", "HIVE_NEEDS_JSON: ${{ toJSON(needs) }}", "HIVE_EXPECTED_REPOSITORY_JOBS", "sudo chown -R root:root .git", "Raw evidence contains a symbolic link",
		"name: Hive setup authorization", "statuses: read", "HIVE_AUTHORIZER_ID", "diff-tree", "--no-renames", "hive/setup-authorized/", "needs.setup-authorization.outputs.authorized", "setup-bootstrap-repository-failure.v2", "Every unauthorized PR remains blocked",
		"HIVE_EXPECTED_UNINSTALL_REF: hive/uninstall-123", "operation: ${{ steps.authorize.outputs.operation }}", "HIVE_SETUP_OPERATION", "Exact out-of-band-authorized Hive uninstall", "always() && needs.setup-authorization.outputs.operation != 'uninstall'"} {
		if !containsString(value, required) {
			t.Fatalf("pull request workflow missing %q", required)
		}
	}
	if containsString(value, "issues: write") || containsString(value, "pull-requests: write") || containsString(value, "statuses: write") {
		t.Fatalf("pull request workflow has an unsafe write lane:\n%s", value)
	}
	if !containsString(value, "pull_request_target:\n    types: [opened, synchronize, reopened]") {
		t.Fatal("pull_request_target must be limited to the base-branch uninstall check publisher lane")
	}
	if !containsString(value, "group: visual-hive-pr-${{ github.event_name }}-${{ github.event.pull_request.number }}") {
		t.Fatal("pull_request and authorization-only pull_request_target runs must not cancel each other")
	}
	if strings.Count(value, "persist-credentials: false") != 6 {
		t.Fatal("pull request workflow must remove credentials from every authorization, target, and fresh-verifier checkout")
	}
	if !containsString(value, "if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation != 'uninstall' }}") {
		t.Fatal("every target-execution job must be explicitly pull_request-only and skipped for managed uninstall")
	}
	if proof, upload := strings.Index(value, "setup-bootstrap-repository-failure.json"), strings.Index(value, "- name: Upload review evidence"); proof < 0 || upload < 0 || proof > upload {
		t.Fatal("setup bootstrap proof must be written before the always-run review artifact upload")
	}
	for _, required := range []string{"HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}", "HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}", `["diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, head]`, "needs: setup-authorization", "if: ${{ always() && github.event_name == 'pull_request' }}"} {
		if !containsString(value, required) {
			t.Fatalf("pull request workflow does not pass untrusted event data through quoted environment variables: missing %q", required)
		}
	}
	if containsString(value, "HIVE_FRESH_SETUP") {
		t.Fatal("pull request workflow must not let an inconsistent installation self-certify through a reduced fresh-setup lane")
	}
	for _, forbidden := range []string{"HIVE_PR_AUTHOR_ASSOCIATION", "HIVE_PR_BODY", "changed.every((file)", "author_association"} {
		if containsString(value, forbidden) {
			t.Fatalf("pull request workflow still trusts self-attested setup input %q", forbidden)
		}
	}
	var document any
	if err := yaml.Unmarshal([]byte(value), &document); err != nil || strings.Contains(value, "%!") || strings.Contains(value, "\t") {
		t.Fatalf("generated pull-request workflow is invalid: %v\n%s", err, value)
	}
	var shellDocument struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(value), &shellDocument); err != nil {
		t.Fatal(err)
	}
	bash := workflowBash(t)
	verdictScript := ""
	for jobName, job := range shellDocument.Jobs {
		for _, step := range job.Steps {
			if strings.TrimSpace(step.Run) == "" {
				continue
			}
			command := exec.Command(bash, "-n")
			command.Stdin = strings.NewReader(step.Run)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("generated shell is invalid in job %s step %q: %v\n%s\n%s", jobName, step.Name, err, output, step.Run)
			}
			if jobName == "visual-hive" && step.Name == "Enforce deterministic verdict" {
				verdictScript = step.Run
			}
		}
	}
	if verdictScript == "" {
		t.Fatal("generated pull-request workflow has no deterministic verdict script")
	}
	runVerdict := func(authorized, operation string, withEvidence bool) error {
		command := exec.Command(bash, "-c", verdictScript)
		command.Dir = t.TempDir()
		if withEvidence {
			writeFixture(t, command.Dir, ".visual-hive/pipeline.json", `{"status":"passed","exitCode":0}`)
			writeFixture(t, command.Dir, ".visual-hive/report.json", `{"status":"passed","summary":{},"results":[]}`)
			writeFixture(t, command.Dir, ".visual-hive/verdict.json", `{"summary":{"visualHiveVerdict":"passed"},"gatingContributions":[]}`)
		}
		command.Env = append(os.Environ(),
			"HIVE_SETUP_AUTHORIZED="+authorized, "HIVE_SETUP_OPERATION="+operation,
			`HIVE_NEEDS_JSON={"visual-hive-execution":{"result":"success"},"repository-test-001":{"result":"success"}}`,
			`HIVE_EXPECTED_REPOSITORY_JOBS=["repository-test-001"]`,
			"HIVE_REPOSITORY=owner/repo", "HIVE_HEAD_SHA="+strings.Repeat("a", 40),
			"HIVE_SETUP_CONTEXT=", "HIVE_SETUP_BINDING_DIGEST=", "HIVE_SETUP_DIFF_DIGEST=",
			"HIVE_TRUSTED_REBUILD_OUTCOME=success",
		)
		return command.Run()
	}
	if err := runVerdict("true", "uninstall", false); err != nil {
		t.Fatalf("exactly authorized uninstall did not satisfy the required visual-hive check: %v", err)
	}
	if err := runVerdict("false", "uninstall", false); err == nil {
		t.Fatal("unauthorized managed uninstall satisfied the required visual-hive check")
	}
	if err := runVerdict("false", "setup", true); err != nil {
		t.Fatalf("ordinary successful PR verdict was changed by the uninstall lane: %v", err)
	}
}

func TestManagedRepositoryConfigExcludesLocalPaths(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "visual-hive.config.yaml", "project:\n  name: proof\ntargets:\n  app:\n    kind: command\ncontracts:\n  - id: app\n    target: app\nintegrations:\n  hive:\n    enabled: false\n    mode: measured\n")
	writeFixture(t, root, ".github/workflows/visual-hive-lifecycle.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-issue-lifecycle.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-trusted-publisher.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-failure-issue.yml", "issues: write")
	writeFixture(t, root, ".github/workflows/visual-hive-hive-handoff.yml", "issues: write")
	config := Config{
		Repository: "owner/repo", DefaultBranch: "main", Coverage: CoverageStandard, Automation: AutomationIssues,
		Provider: "codex", ACMMLevel: 4, VisualHive: true, VisualHiveRepo: "owner/visual-hive",
		VisualHiveRef: "0123456789012345678901234567890123456789", CheckoutDir: `C:\private\checkout`, StateDir: `C:\private\state`,
	}
	if err := writeManagedFiles(root, config, RepositoryInspection{}); err != nil {
		t.Fatal(err)
	}
	if err := markVisualHiveConfigOrigin(root, "generated", profileForCoverage(config.Coverage)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".hive", "integrated.json"))
	if err != nil {
		t.Fatal(err)
	}
	value := string(data)
	if strings.Contains(value, "private") || strings.Contains(value, "checkout_dir") || strings.Contains(value, "state_dir") {
		t.Fatalf("local paths leaked into repository config: %s", value)
	}
	for _, relative := range standaloneVisualHiveWriterWorkflowPaths() {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatalf("standalone writer %s must be removed in integrated mode", relative)
		}
	}
	visualConfig, err := os.ReadFile(filepath.Join(root, "visual-hive.config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(visualConfig, &parsed); err != nil {
		t.Fatal(err)
	}
	hiveIntegration := anyStringMap(anyStringMap(parsed["integrations"])["hive"])
	if hiveIntegration["enabled"] != true || hiveIntegration["mode"] != "measured" {
		t.Fatalf("integrated setup did not enable Hive additively while removing standalone writers: %+v", hiveIntegration)
	}
	prWorkflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "visual-hive-pr.yml"))
	if err != nil || !strings.Contains(string(prWorkflow), visualHivePullRequestProducerCommit) || strings.Contains(string(prWorkflow), config.VisualHiveRef) {
		t.Fatalf("managed pull request workflow is missing or not pinned: %v", err)
	}
}

func TestStandaloneVisualHiveWriterInventoryCoversGeneratedDefaults(t *testing.T) {
	want := []string{
		".github/workflows/visual-hive-lifecycle.yml",
		".github/workflows/visual-hive-issue-lifecycle.yml",
		".github/workflows/visual-hive-trusted-publisher.yml",
		".github/workflows/visual-hive-failure-issue.yml",
		".github/workflows/visual-hive-hive-handoff.yml",
	}
	got := standaloneVisualHiveWriterWorkflowPaths()
	if len(got) != len(want) {
		t.Fatalf("standalone writer inventory = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("standalone writer inventory = %v, want %v", got, want)
		}
	}
	for _, inventory := range [][]string{managedSetupFiles(true), authorizerTransferAbsentFiles(Config{VisualHive: true})} {
		if !contains(inventory, ".github/workflows/visual-hive-failure-issue.yml") || !contains(inventory, ".github/workflows/visual-hive-hive-handoff.yml") {
			t.Fatalf("single-writer lifecycle inventory omitted Visual Hive generated defaults: %v", inventory)
		}
	}
	authorizationJob := setupAuthorizationWorkflowJob(Config{VisualHive: true})
	for _, relative := range want {
		if !strings.Contains(authorizationJob, relative) {
			t.Fatalf("setup authorization workflow omitted standalone writer %q", relative)
		}
	}
}

func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsString(value, target string) bool {
	return len(target) > 0 && strings.Contains(value, target)
}

func hasCommand(commands [][]string, want ...string) bool {
	for _, command := range commands {
		if strings.Join(command, "\x00") == strings.Join(want, "\x00") {
			return true
		}
	}
	return false
}

func workflowBash(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Git", "bin", "bash.exe"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return bash
	}
	t.Skip("bash is required to execute the generated GitHub Actions shell")
	return ""
}

func bashFilesystemPath(value string) string {
	value = filepath.Clean(value)
	volume := filepath.VolumeName(value)
	if len(volume) == 2 && volume[1] == ':' {
		rest := strings.TrimPrefix(filepath.ToSlash(value), filepath.ToSlash(volume))
		return "/" + strings.ToLower(volume[:1]) + rest
	}
	return filepath.ToSlash(value)
}
