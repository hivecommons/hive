package integrated

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	downloadArtifactActionSHA = "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c" // actions/download-artifact v8.0.1
	visualExecutionJobName    = "visual-hive-execution"
	isolatedTargetAccount     = "hive-target"
	isolatedEvidenceAccount   = "hive-evidence"
	isolatedTargetRoot        = "/opt/hive-target"
	isolatedTargetWorkspace   = isolatedTargetRoot + "/workspace"
	isolatedTrustedRoot       = isolatedTargetRoot + "/trusted"
	isolatedTrustedTooling    = isolatedTrustedRoot + "/visual-hive-tooling"
	// Runtime dependencies are sealed tooling, not evidence. Keep them outside
	// .visual-hive so its complete artifact index remains exhaustive.
	isolatedRuntimeRoot       = isolatedTrustedRoot + "/visual-hive-runtime"
	isolatedRuntimeModules    = isolatedRuntimeRoot + "/node_modules"
	isolatedTargetShell       = isolatedTrustedRoot + "/target-shell"
	isolatedTargetBash        = isolatedTrustedRoot + "/target-bash"
	isolatedEvidenceLauncher  = isolatedTrustedRoot + "/run-visual-hive-evidence"
	runnerAttestationPath     = ".visual-hive/proof/pr/runner-execution-attestation.json"
	setupBaselineCaptureJobID = "setup-baseline-capture"
	setupBaselineVerifyJobID  = "setup-baseline-verify"
	// Audited producer with the exclusive proof-only runtime sidecar used by
	// this lane. A later producer must be deliberately reviewed and repinned.
	visualHivePullRequestProducerCommit = hivegithub.VisualHivePullRequestProducerCommit
)

func workflow(config Config) string {
	return isolatedWorkflow(config)
}

func pullRequestWorkflow(config Config) string {
	return isolatedPullRequestWorkflow(config)
}

// isolatedWorkflow renders the production workflow as three independent trust
// zones: one job per repository test, an unprivileged target-execution job,
// and a fresh runner-owned verifier/bundle job. Only the final verifier emits
// artifacts that Hive may consume for issue or repair lifecycle authority.
func isolatedWorkflow(config Config) string {
	repositoryJobs, repositoryNeeds := isolatedRepositoryTestWorkflowJobs(config, "", "inputs.hive_operation == 'production'")
	productionNeeds := appendWorkflowNeed(repositoryNeeds, visualExecutionJobName)
	productionNeeds = strings.Replace(productionNeeds, "    if: ${{ always() }}", "    if: ${{ always() && inputs.hive_operation == 'production' }}", 1)
	jobs := joinWorkflowJobs(
		repositoryJobs,
		isolatedVisualExecutionWorkflowJob(config, false, "inputs.hive_operation == 'production'"),
		productionAggregatorWorkflowJob(config, productionNeeds),
		isolatedSetupBaselineCaptureWorkflowJobs(config),
	)
	return fmt.Sprintf(`name: Hive Visual Hive Production

on:
  workflow_dispatch:
    inputs:
      hive_dispatch_id:
        description: Opaque durable Hive dispatch correlation
        required: true
        type: string
      hive_operation:
        description: Exact Hive workflow lane
        required: true
        default: production
        type: choice
        options:
          - production
          - setup-baseline-capture

run-name: "Hive Visual Hive Production [${{ inputs.hive_dispatch_id }}]"

permissions:
  contents: read
  actions: read

concurrency:
  group: hive-visual-hive-production
  cancel-in-progress: false

jobs:
%s

  # GitHub allows an App-bound context to become required only after that
  # context has completed successfully in the repository within seven days.
  # This job executes no target code and fails actively on stale dispatches.
  visual-hive:
    if: ${{ inputs.hive_operation == 'production' }}
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - name: Reject a non-default production dispatch
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REF: ${{ github.ref }}
          HIVE_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
        run: |
          set -euo pipefail
          test "$HIVE_EVENT_NAME" = "workflow_dispatch"
          test "$HIVE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"
      - uses: actions/checkout@%s
        with:
          ref: ${{ github.event.repository.default_branch }}
          fetch-depth: 1
          persist-credentials: false
      - name: Verify dispatch SHA is the current default head
        shell: bash
        env:
          HIVE_DISPATCH_SHA: ${{ github.sha }}
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_DISPATCH_SHA"
`, jobs, checkoutActionSHA)
}

// isolatedPullRequestWorkflow keeps the protected visual-hive context on a
// fresh verifier job. The target job has read-only GitHub permissions and runs
// every dependency/script/browser process as a separate unprivileged account.
func isolatedPullRequestWorkflow(config Config) string {
	prProducerConfig := config
	prProducerConfig.VisualHiveRef = visualHivePullRequestProducerCommit
	setupAuthorizationJob := setupAuthorizationWorkflowJob(config)
	uninstallCheckJob := uninstallCheckPublisherWorkflowJob()
	repositoryJobs, repositoryNeeds := isolatedRepositoryTestWorkflowJobs(config, "setup-authorization", "github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation != 'uninstall'")
	aggregatorNeeds := appendWorkflowNeeds(repositoryNeeds, "setup-authorization", visualExecutionJobName)
	aggregatorNeeds = strings.Replace(aggregatorNeeds, "    if: ${{ always() }}", "    if: ${{ always() && github.event_name == 'pull_request' }}", 1)
	jobs := joinWorkflowJobs(
		setupAuthorizationJob,
		uninstallCheckJob,
		repositoryJobs,
		isolatedVisualExecutionWorkflowJob(prProducerConfig, true),
		pullRequestAggregatorWorkflowJob(prProducerConfig, aggregatorNeeds),
	)
	return fmt.Sprintf(`name: Visual Hive PR

on:
  pull_request:
  # Base-branch-only uninstall publication. No proposed checkout, dependency,
  # test, or Visual Hive process is ever executed by this event.
  pull_request_target:
    types: [opened, synchronize, reopened]

permissions: {}

concurrency:
  group: visual-hive-pr-${{ github.event_name }}-${{ github.event.pull_request.number }}
  cancel-in-progress: true

jobs:
%s
`, jobs)
}

func isolatedRepositoryTestWorkflowJobs(config Config, prerequisite, condition string) (string, string) {
	var jobs strings.Builder
	jobNames := make([]string, 0, len(config.TestCommands))
	dependencyInstall := isolatedTargetDependencyShell(config.TestCommands, false)
	checkoutRef := "${{ github.sha }}"
	if prerequisite != "" {
		checkoutRef = "${{ github.event.pull_request.head.sha }}"
	}
	jobIndex := 0
	for _, command := range config.TestCommands {
		if len(command) == 0 {
			continue
		}
		jobIndex++
		jobID := fmt.Sprintf("repository-test-%03d", jobIndex)
		jobNames = append(jobNames, jobID)
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		gate := ""
		if prerequisite != "" {
			gate = fmt.Sprintf("    needs: %s\n", prerequisite)
		}
		if condition != "" {
			gate += fmt.Sprintf("    if: ${{ %s }}\n", condition)
		}
		prerequisiteSteps := `      - *hive-repository-test-checkout
      - *hive-repository-test-setup-node
      - *hive-repository-test-setup-python
      - *hive-repository-test-prepare-account
      - *hive-repository-test-provision-tools
      - *hive-repository-test-install-dependencies
      - *hive-repository-test-seal-checkout`
		cleanupStep := `      - *hive-repository-test-cleanup`
		if jobIndex == 1 {
			prerequisiteSteps = fmt.Sprintf(`      - &hive-repository-test-checkout
        uses: actions/checkout@%s
        with:
          ref: %s
          fetch-depth: 1
          persist-credentials: false
      - &hive-repository-test-setup-node
        uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
      - &hive-repository-test-setup-python
        uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - &hive-repository-test-prepare-account
        name: Prepare isolated target account
        shell: bash
        run: |
%s
      - &hive-repository-test-provision-tools
        name: Provision trusted repository test tools
        shell: bash
        run: |
%s
      - &hive-repository-test-install-dependencies
        name: Install repository test dependencies
        shell: bash
        run: |
%s
      - &hive-repository-test-seal-checkout
        name: Stop dependency processes and verify exact tracked source
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s`, checkoutActionSHA, checkoutRef, setupNodeActionSHA, setupPythonActionSHA,
				indentWorkflowShell(prepareIsolatedTargetAccountShell(), 10),
				indentWorkflowShell(trustedRepositoryTestPrerequisitesShell(), 10),
				indentWorkflowShell(dependencyInstall, 10), checkoutRef, indentWorkflowShell(verifyAndSealTargetCheckoutShell(), 10))
			cleanupStep = fmt.Sprintf(`      - &hive-repository-test-cleanup
        name: Terminate isolated target processes and reverify source
        if: always()
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s`, checkoutRef, indentWorkflowShell(verifyImmutableTargetCheckoutShell(), 10))
		}
		fmt.Fprintf(&jobs, `  %s:
    name: Hive repository test %03d
%s    permissions:
      contents: read
    runs-on: ubuntu-latest
    timeout-minutes: 90
    steps:
%s
      - name: Execute exact repository test command
        shell: bash
        run: |
          set -euo pipefail
          %s %s
%s

`, jobID, jobIndex, gate, prerequisiteSteps, isolatedTargetEnvPrefix(), strings.Join(quoted, " "), cleanupStep)
	}
	return strings.TrimSuffix(jobs.String(), "\n"), workflowNeeds(jobNames)
}

func isolatedVisualExecutionWorkflowJob(config Config, pullRequest bool, condition ...string) string {
	gate := ""
	checkoutRef := "${{ github.sha }}"
	rawArtifact := "visual-hive-raw-${{ github.run_id }}-${{ github.run_attempt }}"
	modeArgs := "--mode full --ci --continue-on-error --skip-install"
	captureScope := ""
	targetPipelineEnv := ""
	sealIdentityEnv := `          HIVE_TARGET_HEAD_SHA: ${{ github.sha }}
`
	evidenceLauncherPrefix := isolatedVisualEvidenceLauncherPrefix(false)
	runnerAttestation := ""
	finalEnforcement := `          if [[ "$HIVE_TARGET_PIPELINE_OUTCOME" != "success" && "$HIVE_TARGET_PIPELINE_OUTCOME" != "failure" ]]; then
            echo "Visual pipeline did not reach a terminal runner-owned outcome" >&2
            exit 1
          fi`
	if pullRequest {
		gate = "    needs: setup-authorization\n    if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation != 'uninstall' }}\n"
		checkoutRef = "${{ github.event.pull_request.head.sha }}"
		rawArtifact = "visual-hive-pr-raw-${{ github.run_id }}-${{ github.run_attempt }}"
		// The pinned producer must expose --runtime-sidecar before this generated
		// lane can run. Never omit the flag or synthesize runtime identity in Hive:
		// an actual PR evaluation without the producer-written sidecar fails closed
		// in the fresh runner-owned aggregator.
		modeArgs = "--mode pr --changed-files \"$HIVE_CHANGED_FILES\" --runtime-sidecar .visual-hive/proof/pr/runtime.json --ci --continue-on-error --skip-install"
		targetPipelineEnv = `        env:
          HIVE_TARGET_PULL_REQUEST: ${{ github.event.pull_request.number }}
          HIVE_TARGET_REPOSITORY: ${{ github.repository }}
          HIVE_TARGET_BASE_REF: ${{ github.event.pull_request.base.ref }}
          HIVE_TARGET_HEAD_REF: ${{ github.event.pull_request.head.ref }}
          HIVE_TARGET_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_TARGET_RUN_ID: ${{ github.run_id }}
          HIVE_TARGET_RUN_ATTEMPT: ${{ github.run_attempt }}
          HIVE_TARGET_WORKFLOW: Visual Hive PR
`
		sealIdentityEnv = `          HIVE_TARGET_PULL_REQUEST: ${{ github.event.pull_request.number }}
          HIVE_TARGET_REPOSITORY: ${{ github.repository }}
          HIVE_TARGET_BASE_REF: ${{ github.event.pull_request.base.ref }}
          HIVE_TARGET_HEAD_REF: ${{ github.event.pull_request.head.ref }}
          HIVE_TARGET_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_TARGET_RUN_ID: ${{ github.run_id }}
          HIVE_TARGET_RUN_ATTEMPT: ${{ github.run_attempt }}
          HIVE_TARGET_WORKFLOW: Visual Hive PR
`
		evidenceLauncherPrefix = isolatedVisualEvidenceLauncherPrefix(true)
		runnerAttestation = indentWorkflowShell(runnerExecutionAttestationShell(), 10)
		captureScope = `      - name: Capture immutable pull request scope
        shell: bash
        env:
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_CHANGED_FILES: ${{ runner.temp }}/changed-files.pr.txt
        run: |
          input_dir="/tmp/hive-visual-input-${{ github.run_id }}"
          sudo install -d -o root -g root -m 0755 "$input_dir"
` + indentWorkflowShell(pullRequestChangedFilesCaptureShell(), 10) + `
          sudo install -o root -g root -m 0444 "$HIVE_CHANGED_FILES" "$input_dir/changed-files.pr.txt"
          echo "HIVE_CHANGED_FILES=$input_dir/changed-files.pr.txt" >> "$GITHUB_ENV"
`
	} else if len(condition) > 0 && strings.TrimSpace(condition[0]) != "" {
		gate = fmt.Sprintf("    if: ${{ %s }}\n", strings.TrimSpace(condition[0]))
	}
	return fmt.Sprintf(`  %s:
    name: %s
%s    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@%s
        with:
          ref: %s
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Build exact Visual Hive tooling before target execution
        working-directory: .hive-visual-tooling
        shell: bash
        env:
          HIVE_VISUAL_HIVE_REF: %s
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_VISUAL_HIVE_REF"
          npm ci
          npm run build
          test -z "$(git status --porcelain --untracked-files=no)"
      - name: Seal immutable tooling and prepare isolated target account
        shell: bash
        run: |
          set -euo pipefail
%s
          trusted_tooling=%q
          sudo mv .hive-visual-tooling "$trusted_tooling"
          cli="$trusted_tooling/packages/cli/dist/index.js"
          test -f "$cli"
          cli_sha="$(sha256sum "$cli" | cut -d ' ' -f 1)"
          trusted_node="$(readlink -f "$(command -v node)")"
          tool_cache="$(readlink -f "$RUNNER_TOOL_CACHE")"
          node_runtime="$(dirname "$(dirname "$trusted_node")")"
          test -x "$trusted_node"
          case "$node_runtime" in
            "$tool_cache"/node/*) ;;
            *) echo "setup-node resolved outside the GitHub-hosted tool cache" >&2; exit 1 ;;
          esac
          sudo chown -R root:root "$node_runtime"
          sudo chmod -R a-w "$node_runtime"
          trusted_node_sha="$(sha256sum "$trusted_node" | cut -d ' ' -f 1)"
          echo "VISUAL_HIVE_CLI=$cli" >> "$GITHUB_ENV"
          echo "HIVE_VISUAL_HIVE_CLI_SHA=$cli_sha" >> "$GITHUB_ENV"
          echo "HIVE_TRUSTED_NODE=$trusted_node" >> "$GITHUB_ENV"
          echo "HIVE_TRUSTED_NODE_SHA=$trusted_node_sha" >> "$GITHUB_ENV"
          sudo chown -R root:root "$trusted_tooling"
          sudo chmod -R a-w "$trusted_tooling"
%s
%s      - name: Install target dependencies in isolated account
        shell: bash
        run: |
%s
      - name: Stop dependency processes and verify exact tracked source
        shell: bash
        env:
          HIVE_TARGET_HEAD_SHA: %s
        run: |
%s
      - name: Verify sealed Playwright browser handoff
        shell: bash
        run: |
%s
      - name: Prepare isolated Visual Hive evidence root
        shell: bash
        run: |
%s
      - name: Run target-facing Visual Hive collection
        id: target_visual_pipeline
        continue-on-error: true
        shell: bash
%s        run: |
          set +e
          umask 077
          %s "$HIVE_TRUSTED_NODE" "$VISUAL_HIVE_CLI" pipeline --config visual-hive.config.yaml %s
          pipeline_exit=$?
          set -e
          runner_pipeline_exit="$RUNNER_TEMP/hive-visual-pipeline-exit-${GITHUB_RUN_ID}.txt"
          rm -f -- "$runner_pipeline_exit"
          (umask 077 && printf '%%s\n' "$pipeline_exit" > "$runner_pipeline_exit")
          test -f "$runner_pipeline_exit"
          test ! -L "$runner_pipeline_exit"
          exit "$pipeline_exit"
      - name: Terminate target account and reverify immutable tooling
        id: seal_raw_evidence
        if: always()
        shell: bash
        env:
          HIVE_TARGET_PIPELINE_OUTCOME: ${{ steps.target_visual_pipeline.outcome }}
          HIVE_TARGET_PIPELINE_CONCLUSION: ${{ steps.target_visual_pipeline.conclusion }}
%s        run: |
          set -euo pipefail
          target_workspace="${HIVE_TARGET_WORKSPACE:-}"
          expected_browser_path="`+isolatedTrustedRoot+`/playwright-${GITHUB_RUN_ID}"
          expected_browser_manifest="$RUNNER_TEMP/hive-trusted-playwright-${GITHUB_RUN_ID}.tsv"
          cleanup_trusted_browser() {
            sudo rm -rf -- "$expected_browser_path"
            rm -f -- "$expected_browser_manifest"
          }
          cleanup_target_workspace() {
            if [ -n "$target_workspace" ] && mountpoint -q "$target_workspace"; then
              sudo umount "$target_workspace" || true
            fi
            cleanup_trusted_browser
            if [ -n "${HIVE_EXECUTION_AUTHORITY_KEY:-}" ]; then
              sudo rm -f -- "$HIVE_EXECUTION_AUTHORITY_KEY"
            fi
          }
          trap cleanup_target_workspace EXIT
          test -n "$target_workspace"
          sudo pkill -KILL -u %s 2>/dev/null || true
          sudo pkill -KILL -u %s 2>/dev/null || true
          if pgrep -u %s >/dev/null 2>&1; then
            echo "Isolated target account still owns a live process" >&2
            exit 1
          fi
          if pgrep -u %s >/dev/null 2>&1; then
            echo "Isolated evidence account still owns a live process" >&2
            exit 1
          fi
          test "$(sha256sum "$VISUAL_HIVE_CLI" | cut -d ' ' -f 1)" = "$HIVE_VISUAL_HIVE_CLI_SHA"
          test ! -w "$VISUAL_HIVE_CLI"
          test "$(sha256sum "$HIVE_TRUSTED_NODE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_NODE_SHA"
          sudo -u %s -- test ! -w "$HIVE_TRUSTED_NODE"
          sudo -u %s -- test ! -w "$HIVE_TRUSTED_NODE"
%s
          if [ -n "${HIVE_TRUSTED_BROWSER_EXECUTABLE:-}" ] && [ -n "${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-}" ] && [ -n "${HIVE_TRUSTED_BROWSER_SHA:-}" ]; then
            case "$HIVE_TRUSTED_BROWSER_EXECUTABLE" in
              "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"/*) ;;
              *) echo "Pinned browser escaped its sealed root" >&2; exit 1 ;;
            esac
            test -x "$HIVE_TRUSTED_BROWSER_EXECUTABLE"
            test "$(sha256sum "$HIVE_TRUSTED_BROWSER_EXECUTABLE" | cut -d ' ' -f 1)" = "$HIVE_TRUSTED_BROWSER_SHA"
            sudo -u %s -- test ! -w "$HIVE_TRUSTED_BROWSER_EXECUTABLE"
            sudo -u %s -- test ! -w "$HIVE_TRUSTED_BROWSER_EXECUTABLE"
          else
            echo "Pinned browser preparation did not complete" >&2
          fi
          test "$(git rev-parse HEAD)" = "%s"
          sudo git -c safe.directory="$GITHUB_WORKSPACE" diff --no-ext-diff --no-textconv --exit-code -- .
          sudo umount "$target_workspace"
          cleanup_trusted_browser
          if mountpoint -q "$target_workspace"; then
            echo "Isolated target workspace remained mounted" >&2
            exit 1
          fi
          test ! -e "$expected_browser_path"
          test ! -e "$expected_browser_manifest"
          sudo pkill -KILL -u %s 2>/dev/null || true
          sudo pkill -KILL -u %s 2>/dev/null || true
          if pgrep -u %s >/dev/null 2>&1 || pgrep -u %s >/dev/null 2>&1; then
            echo "An isolated execution principal remained live before evidence authentication" >&2
            exit 1
          fi
%s
          runner_pipeline_exit="$RUNNER_TEMP/hive-visual-pipeline-exit-${GITHUB_RUN_ID}.txt"
          test -f "$runner_pipeline_exit"
          test ! -L "$runner_pipeline_exit"
          pipeline_exit="$(cat "$runner_pipeline_exit")"
          case "$pipeline_exit" in
            0) test "$HIVE_TARGET_PIPELINE_OUTCOME" = "success" ;;
            ''|*[!0-9]*) echo "Visual pipeline exit code is invalid" >&2; exit 1 ;;
            *) test "$HIVE_TARGET_PIPELINE_OUTCOME" = "failure" ;;
          esac
          sudo -u %s -- test ! -e .visual-hive/pipeline-exit-code.txt
          sudo install -o root -g root -m 0444 "$runner_pipeline_exit" .visual-hive/pipeline-exit-code.txt
          rm -f -- "$runner_pipeline_exit"
          runner_outcome="$RUNNER_TEMP/hive-visual-runner-outcome-${GITHUB_RUN_ID}.json"
          RUNNER_OUTCOME_PATH="$runner_outcome" node <<'NODE'
          const fs = require("fs");
          const outcome = process.env.HIVE_TARGET_PIPELINE_OUTCOME;
          const conclusion = process.env.HIVE_TARGET_PIPELINE_CONCLUSION;
          if (!["success", "failure"].includes(outcome) || !["success", "failure"].includes(conclusion)) {
            throw new Error("Visual pipeline lacks a terminal runner-owned outcome receipt");
          }
          fs.writeFileSync(process.env.RUNNER_OUTCOME_PATH, JSON.stringify({
            schemaVersion: "hive.visual-runner-outcome.v1",
            outcome,
            conclusion
          }) + "\n", { flag: "wx", mode: 0o600 });
          NODE
          sudo rm -f -- .visual-hive/hive-runner-outcome.json
          sudo install -o root -g root -m 0444 "$runner_outcome" .visual-hive/hive-runner-outcome.json
          rm -f -- "$runner_outcome"
          sudo -u %s -- test -f .visual-hive/hive-runner-outcome.json
          sudo -u %s -- test ! -L .visual-hive/hive-runner-outcome.json
          sudo -u %s -- test -r .visual-hive/hive-runner-outcome.json
          sudo -u %s -- test ! -w .visual-hive/hive-runner-outcome.json
          test "$(sudo -u %s -- stat -c '%%u:%%g:%%a' .visual-hive/pipeline-exit-code.txt)" = "0:0:444"
          test "$(sudo -u %s -- stat -c '%%u:%%g:%%a' .visual-hive/hive-runner-outcome.json)" = "0:0:444"
          test "$(git rev-parse HEAD)" = "%s"
%s
%s
          trap - EXIT
      - name: Upload isolated raw Visual evidence
        if: ${{ always() && steps.seal_raw_evidence.outcome == 'success' }}
        uses: actions/upload-artifact@%s
        with:
          name: %s
          path: .visual-hive
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Confirm target execution runner outcome
        if: always()
        shell: bash
        env:
          HIVE_TARGET_PIPELINE_OUTCOME: ${{ steps.target_visual_pipeline.outcome }}
        run: |
          set -euo pipefail
%s
`, visualExecutionJobName, visualExecutionJobName, gate, checkoutActionSHA, checkoutRef, checkoutActionSHA,
		config.VisualHiveRepo, config.VisualHiveRef, setupNodeActionSHA, setupPythonActionSHA, config.VisualHiveRef,
		indentWorkflowShell(prepareIsolatedTargetAccountShell(), 10), isolatedTrustedTooling,
		indentWorkflowShell(prepareIsolatedVisualEvidenceRuntimeShell(pullRequest), 10), captureScope,
		indentWorkflowShell(isolatedTargetDependencyShell(config.TestCommands, true), 10), checkoutRef, indentWorkflowShell(verifyAndSealTargetCheckoutShell(), 10),
		indentWorkflowShell(trustedBrowserHandoffVerificationShell(), 10), indentWorkflowShell(prepareIsolatedVisualEvidenceRootShell(), 10), targetPipelineEnv, evidenceLauncherPrefix, modeArgs, sealIdentityEnv,
		isolatedTargetAccount, isolatedEvidenceAccount, isolatedTargetAccount, isolatedEvidenceAccount,
		isolatedTargetAccount, isolatedEvidenceAccount, indentWorkflowShell(trustedBrowserHandoffVerificationShell(), 10),
		isolatedTargetAccount, isolatedEvidenceAccount, "$HIVE_TARGET_HEAD_SHA",
		isolatedTargetAccount, isolatedEvidenceAccount, isolatedTargetAccount, isolatedEvidenceAccount,
		indentWorkflowShell(validateIsolatedVisualEvidenceBeforeSealShell(), 10), isolatedEvidenceAccount,
		isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount,
		"$HIVE_TARGET_HEAD_SHA", runnerAttestation,
		indentWorkflowShell(sealIsolatedVisualEvidenceShell(), 10), uploadArtifactActionSHA, rawArtifact,
		indentWorkflowShell(finalEnforcement, 10))
}

func isolatedSetupBaselineCaptureWorkflowJobs(config Config) string {
	target := isolatedVisualExecutionWorkflowJob(config, false, "inputs.hive_operation == 'setup-baseline-capture'")
	target = strings.ReplaceAll(target, visualExecutionJobName, setupBaselineCaptureJobID)
	target = strings.Replace(target, "name: "+setupBaselineCaptureJobID, "name: "+hivegithub.SetupBaselineCaptureJobDisplayName, 1)
	target = strings.Replace(target, "--mode full --ci --continue-on-error --skip-install", "--mode full --ci --continue-on-error --skip-install --bootstrap-baselines", 1)
	target = strings.ReplaceAll(target, "visual-hive-raw-${{ github.run_id }}-${{ github.run_attempt }}", "setup-baseline-raw-${{ github.run_id }}-${{ github.run_attempt }}")
	verifier := fmt.Sprintf(`  %s:
    name: %s
    needs: %s
    if: ${{ always() && inputs.hive_operation == 'setup-baseline-capture' && needs.%s.result == 'success' }}
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
%s
      - name: Download isolated baseline capture
        uses: actions/download-artifact@%s
        with:
          name: setup-baseline-raw-${{ github.run_id }}-${{ github.run_attempt }}
          path: .visual-hive
      - name: Validate exact Linux baseline candidate set
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_REPOSITORY_ID: ${{ github.event.repository.id }}
          HIVE_CAPTURE_HEAD: ${{ github.sha }}
          HIVE_CAPTURE_REF: ${{ github.ref }}
          HIVE_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}
          HIVE_CAPTURE_CORRELATION: ${{ inputs.hive_dispatch_id }}
          HIVE_WORKFLOW_RUN_ID: ${{ github.run_id }}
          HIVE_WORKFLOW_NAME: Hive Visual Hive Production
          HIVE_WORKFLOW_PATH: .github/workflows/hive-visual-hive.yml
        run: |
          set -euo pipefail
          test "$HIVE_EVENT_NAME" = "workflow_dispatch"
          test "$HIVE_CAPTURE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"
          test "$(git rev-parse HEAD)" = "$HIVE_CAPTURE_HEAD"
          test -f "$VISUAL_HIVE_CLI"
          test ! -w "$VISUAL_HIVE_CLI"
          node <<'NODE'
          const crypto = require("crypto");
          const fs = require("fs");
          const path = require("path");
          const correlation = String(process.env.HIVE_CAPTURE_CORRELATION || "").toLowerCase();
          const head = String(process.env.HIVE_CAPTURE_HEAD || "").toLowerCase();
          const repository = String(process.env.HIVE_REPOSITORY || "");
          const repositoryID = String(process.env.HIVE_REPOSITORY_ID || "");
          const runID = String(process.env.HIVE_WORKFLOW_RUN_ID || "");
          if (!/^[a-f0-9]{64}$/u.test(correlation) || !/^[a-f0-9]{40}$/u.test(head) ||
              !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(repository) || !/^[1-9][0-9]*$/u.test(repositoryID) || !/^[1-9][0-9]*$/u.test(runID)) {
            throw new Error("baseline capture immutable identity is invalid");
          }
          const sourceRoot = path.resolve(".visual-hive/snapshots");
          const outputRoot = path.resolve(process.env.RUNNER_TEMP, "setup-baseline-artifact");
          if (!fs.existsSync(sourceRoot) || !fs.lstatSync(sourceRoot).isDirectory() || fs.lstatSync(sourceRoot).isSymbolicLink()) {
            throw new Error("baseline capture produced no regular snapshots directory");
          }
          const records = [];
          const visit = (directory) => {
            for (const name of fs.readdirSync(directory).sort()) {
              const absolute = path.join(directory, name);
              const stat = fs.lstatSync(absolute);
              if (stat.isSymbolicLink()) throw new Error("baseline candidate contains a symbolic link");
              if (stat.isDirectory()) { visit(absolute); continue; }
              if (!stat.isFile()) throw new Error("baseline candidate is not a regular file");
              const relativeSource = path.relative(process.cwd(), absolute).split(path.sep).join("/");
              if (!/^\.visual-hive\/snapshots\/(?:[^/]+\/)*[^/]+\.png$/u.test(relativeSource) || relativeSource.includes("..")) {
                throw new Error("unexpected baseline candidate path " + relativeSource);
              }
              if (stat.size <= 8 || stat.size > 20 * 1024 * 1024) throw new Error("baseline candidate has invalid size");
              const content = fs.readFileSync(absolute);
              if (!content.subarray(0, 8).equals(Buffer.from([137,80,78,71,13,10,26,10]))) throw new Error("baseline candidate is not PNG");
              const destination = path.join(outputRoot, ...relativeSource.split("/"));
              fs.mkdirSync(path.dirname(destination), { recursive: true });
              fs.copyFileSync(absolute, destination, fs.constants.COPYFILE_EXCL);
              records.push({ path: relativeSource, sha256: crypto.createHash("sha256").update(content).digest("hex"), bytes: stat.size });
            }
          };
          visit(sourceRoot);
          records.sort((a, b) => a.path.localeCompare(b.path));
          const totalBytes = records.reduce((sum, item) => sum + item.bytes, 0);
          if (records.length < 1 || records.length > 200 || totalBytes > 500 * 1024 * 1024) throw new Error("baseline candidate set is empty or exceeds bounds");
          const candidateDigest = crypto.createHash("sha256").update(JSON.stringify(records)).digest("hex");
          const manifest = {
            schema_version: "hive.setup-baseline-artifact.v1", repository, repository_id: repositoryID,
            capture_correlation: correlation, capture_head: head, workflow_run_id: runID,
            workflow_name: process.env.HIVE_WORKFLOW_NAME, workflow_path: process.env.HIVE_WORKFLOW_PATH,
            event: "workflow_dispatch", platform: "linux", runner: "ubuntu-latest",
            candidate_digest: candidateDigest, file_count: records.length, total_bytes: totalBytes, files: records
          };
          fs.mkdirSync(outputRoot, { recursive: true });
          fs.writeFileSync(path.join(outputRoot, "setup-baseline-manifest.json"), JSON.stringify(manifest, null, 2) + "\n", { flag: "wx" });
          NODE
      - name: Upload correlation-bound setup baseline artifact
        uses: actions/upload-artifact@%s
        with:
          name: hive-setup-baselines-${{ inputs.hive_dispatch_id }}
          path: ${{ runner.temp }}/setup-baseline-artifact
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, setupBaselineVerifyJobID, hivegithub.SetupBaselineVerifyJobDisplayName, setupBaselineCaptureJobID, setupBaselineCaptureJobID,
		runnerOwnedVerifierSetupSteps(config, "${{ github.sha }}", ""), downloadArtifactActionSHA, uploadArtifactActionSHA)
	return target + "\n" + verifier
}

func productionAggregatorWorkflowJob(config Config, needs string) string {
	expectedJSON, _ := json.Marshal(isolatedRepositoryJobIDs(config))
	return fmt.Sprintf(`  visual-hive-production:
%s
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - name: Verify isolated runner prerequisites before trusted publication
        shell: bash
        env:
          HIVE_NEEDS_JSON: ${{ toJSON(needs) }}
          HIVE_EXPECTED_REPOSITORY_JOBS: '%s'
        run: |
%s
%s
      - name: Download isolated raw Visual evidence
        uses: actions/download-artifact@%s
        with:
          name: visual-hive-raw-${{ github.run_id }}-${{ github.run_attempt }}
          path: .visual-hive
      - name: Rebuild and validate runner-owned lifecycle evidence
        shell: bash
        run: |
%s
      - name: Stage content-addressed evidence root
        shell: bash
        run: |
          set -euo pipefail
          evidence_stage="$RUNNER_TEMP/hive-visual-hive-evidence-${GITHUB_RUN_ID}"
          test ! -e "$evidence_stage"
          install -d -m 0700 "$evidence_stage/.visual-hive"
          if find .visual-hive -type l -print -quit | grep -q .; then
            echo "Runner-owned evidence contains a symbolic link" >&2
            exit 1
          fi
          while IFS= read -r -d '' source; do
            relative="${source#.visual-hive/}"
            case "$relative" in
              bundles/*) continue ;;
            esac
            target="$evidence_stage/.visual-hive/$relative"
            install -D -m 0444 -- "$source" "$target"
            cmp -- "$source" "$target"
          done < <(find .visual-hive -type f -print0 | sort -z)
          test -f "$evidence_stage/.visual-hive/artifacts-index.json"
          find "$evidence_stage" -depth -type d -exec chmod 0555 -- {} +
      - name: Upload independently verifiable evidence
        id: evidence
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-evidence-${{ github.run_id }}
          path: ${{ runner.temp }}/hive-visual-hive-evidence-${{ github.run_id }}
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Build Hive lifecycle bundle bound to evidence artifact
        shell: bash
        env:
          VISUAL_HIVE_WORKFLOW_ARTIFACT_ID: ${{ steps.evidence.outputs.artifact-id }}
        run: |
          set -euo pipefail
          args=()
          while IFS= read -r contract; do
            if [ -n "$contract" ]; then args+=(--evaluated-contract "$contract"); fi
          done < .visual-hive/evaluated-contracts.txt
          authority_args=()
          if [ "$(tr -d '\r\n' < .visual-hive/authoritative-resolution.txt)" = "true" ]; then
            authority_args+=(--authoritative-for-resolution)
          fi
          env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL node "$VISUAL_HIVE_CLI" hive bundle --config visual-hive.config.yaml --issues .visual-hive/issues.json --acmm-request %d --scan-scope full "${authority_args[@]}" "${args[@]}"
      - name: Upload trusted Hive bundle
        id: bundle
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-bundle-${{ github.run_id }}
          path: .visual-hive/bundles
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, needs, string(expectedJSON), indentWorkflowShell(productionPrerequisiteShell(), 10), runnerOwnedVerifierSetupSteps(config, "${{ github.sha }}", ""), downloadArtifactActionSHA,
		indentWorkflowShell(runnerOwnedEvidenceRebuildShell(false), 10), uploadArtifactActionSHA, config.ACMMLevel, uploadArtifactActionSHA)
}

func pullRequestAggregatorWorkflowJob(config Config, needs string) string {
	expectedJobs := isolatedRepositoryJobIDs(config)
	expectedJSON, _ := json.Marshal(expectedJobs)
	return fmt.Sprintf(`  visual-hive:
%s
    permissions:
      contents: read
      actions: read
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
%s
      - name: Download isolated raw Visual evidence
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        uses: actions/download-artifact@%s
        with:
          name: visual-hive-pr-raw-${{ github.run_id }}-${{ github.run_attempt }}
          path: .visual-hive
      - name: Capture runner-owned pull request scope
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        shell: bash
        env:
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_CHANGED_FILES: .visual-hive/changed-files.pr.txt
        run: |
%s
      - name: Rebuild and validate runner-owned review evidence
        if: ${{ needs.setup-authorization.outputs.operation != 'uninstall' }}
        id: trusted_rebuild
        continue-on-error: true
        shell: bash
        run: |
%s
      - name: Enforce deterministic verdict
        if: always()
        shell: bash
        env:
          HIVE_NEEDS_JSON: ${{ toJSON(needs) }}
          HIVE_EXPECTED_REPOSITORY_JOBS: '%s'
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_SETUP_AUTHORIZED: ${{ needs.setup-authorization.outputs.authorized }}
          HIVE_SETUP_CONTEXT: ${{ needs.setup-authorization.outputs.context }}
          HIVE_SETUP_BINDING_DIGEST: ${{ needs.setup-authorization.outputs.binding_digest }}
          HIVE_SETUP_DIFF_DIGEST: ${{ needs.setup-authorization.outputs.diff_digest }}
          HIVE_SETUP_OPERATION: ${{ needs.setup-authorization.outputs.operation }}
          HIVE_TRUSTED_REBUILD_OUTCOME: ${{ steps.trusted_rebuild.outcome }}
        run: |
%s
      - name: Stage immutable legacy pull request review evidence
        if: ${{ always() && needs.setup-authorization.outputs.operation != 'uninstall' }}
        shell: bash
        run: |
          set -euo pipefail
          review_stage="$RUNNER_TEMP/hive-visual-hive-pr-review-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
          test ! -e "$review_stage"
          test -d .visual-hive
          test ! -L .visual-hive
          if find .visual-hive -type l -print -quit | grep -q .; then
            echo "Legacy PR review evidence contains a symbolic link" >&2
            exit 1
          fi
          if find .visual-hive ! -type d ! -type f -print -quit | grep -q .; then
            echo "Legacy PR review evidence contains a non-regular entry" >&2
            exit 1
          fi
          install -d -m 0700 "$review_stage/.visual-hive"
          while IFS= read -r -d '' source; do
            relative="${source#.visual-hive/}"
            target="$review_stage/.visual-hive/$relative"
            install -D -m 0444 -- "$source" "$target"
            cmp -- "$source" "$target"
          done < <(find .visual-hive -type f -print0 | sort -z)
          find "$review_stage" -depth -type d -exec chmod 0555 -- {} +
      - name: Prepare admissible pull request source binding
        if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == '' }}
        id: pr_source
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_SETUP_OPERATION: ${{ needs.setup-authorization.outputs.operation }}
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_PULL_REQUEST_NUMBER: ${{ github.event.pull_request.number }}
          HIVE_BASE_REPOSITORY: ${{ github.event.pull_request.base.repo.full_name }}
          HIVE_BASE_REPOSITORY_ID: ${{ github.event.pull_request.base.repo.id }}
          HIVE_BASE_REF: ${{ github.event.pull_request.base.ref }}
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_HEAD_REPOSITORY: ${{ github.event.pull_request.head.repo.full_name }}
          HIVE_HEAD_REPOSITORY_ID: ${{ github.event.pull_request.head.repo.id }}
          HIVE_HEAD_REF: ${{ github.event.pull_request.head.ref }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_WORKFLOW_RUN_ID: ${{ github.run_id }}
          HIVE_WORKFLOW_RUN_ATTEMPT: ${{ github.run_attempt }}
          HIVE_WORKFLOW_NAME: Visual Hive PR
          HIVE_WORKFLOW_PATH: .github/workflows/visual-hive-pr.yml
          HIVE_PRODUCER_COMMIT: %s
          HIVE_SOURCE_ARTIFACT_NAME: visual-hive-pr-source-${{ github.run_id }}-${{ github.run_attempt }}
          HIVE_BUNDLE_ARTIFACT_NAME: visual-hive-pr-bundle-${{ github.run_id }}-${{ github.run_attempt }}
        run: |
%s
      - name: Stage content-addressed pull request source
        if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == '' && steps.pr_source.outputs.admissible == 'true' }}
        shell: bash
        run: |
          set -euo pipefail
          source_stage="$RUNNER_TEMP/hive-visual-hive-pr-source-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
          test ! -e "$source_stage"
          test -f .visual-hive/proof/pr/runtime.json
          test -f .visual-hive/proof/pr/source-binding.json
          test -f .visual-hive/artifacts-index.json
          install -d -m 0700 "$source_stage/.visual-hive"
          if find .visual-hive -type l -print -quit | grep -q .; then
            echo "Runner-owned PR source contains a symbolic link" >&2
            exit 1
          fi
          if find .visual-hive ! -type d ! -type f -print -quit | grep -q .; then
            echo "Runner-owned PR source contains a non-regular entry" >&2
            exit 1
          fi
          while IFS= read -r -d '' source; do
            relative="${source#.visual-hive/}"
            case "$relative" in
              bundles/*) continue ;;
            esac
            target="$source_stage/.visual-hive/$relative"
            install -D -m 0444 -- "$source" "$target"
            cmp -- "$source" "$target"
          done < <(find .visual-hive -type f -print0 | sort -z)
          find "$source_stage" -depth -type d -exec chmod 0555 -- {} +
      - name: Upload independently verifiable pull request source
        if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == '' && steps.pr_source.outputs.admissible == 'true' }}
        id: pr_source_artifact
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-pr-source-${{ github.run_id }}-${{ github.run_attempt }}
          path: ${{ runner.temp }}/hive-visual-hive-pr-source-${{ github.run_id }}-${{ github.run_attempt }}
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Build non-authoritative pull request bundle
        if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == '' && steps.pr_source.outputs.admissible == 'true' }}
        shell: bash
        env:
          HIVE_HEAD_REF: ${{ github.event.pull_request.head.ref }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          VISUAL_HIVE_WORKFLOW_ARTIFACT_ID: ${{ steps.pr_source_artifact.outputs.artifact-id }}
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_HEAD_SHA"
          case "$HIVE_HEAD_REF" in refs/pull/*|refs/merge/*) echo "Merge refs cannot produce PR bundles" >&2; exit 1 ;; esac
          test -n "$VISUAL_HIVE_WORKFLOW_ARTIFACT_ID"
          contract_args=()
          while IFS= read -r contract; do
            if [ -n "$contract" ]; then contract_args+=(--evaluated-contract "$contract"); fi
          done < .visual-hive/evaluated-contracts.txt
          file_args=()
          while IFS= read -r file; do
            if [ -n "$file" ]; then file_args+=(--evaluated-file "$file"); fi
          done < .visual-hive/changed-files.pr.txt
          env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL -u NODE_OPTIONS -u BASH_ENV -u ENV \
            GITHUB_SHA="$HIVE_HEAD_SHA" GITHUB_REF="refs/heads/$HIVE_HEAD_REF" GITHUB_EVENT_NAME=pull_request VISUAL_HIVE_SOURCE_CONCLUSION=success VISUAL_HIVE_WORKFLOW_ARTIFACT_ID="$VISUAL_HIVE_WORKFLOW_ARTIFACT_ID" \
            node "$VISUAL_HIVE_CLI" hive bundle --config visual-hive.config.yaml --issues .visual-hive/proof/pr/issues.present.json --acmm-request %d --scan-scope changed-files "${contract_args[@]}" "${file_args[@]}"
      - name: Upload pull request bundle
        if: ${{ github.event_name == 'pull_request' && needs.setup-authorization.outputs.operation == '' && steps.pr_source.outputs.admissible == 'true' }}
        id: pr_bundle
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-pr-bundle-${{ github.run_id }}-${{ github.run_attempt }}
          path: .visual-hive/bundles
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
      - name: Upload review evidence
        if: ${{ always() && needs.setup-authorization.outputs.operation != 'uninstall' }}
        uses: actions/upload-artifact@%s
        with:
          name: visual-hive-pr
          path: ${{ runner.temp }}/hive-visual-hive-pr-review-${{ github.run_id }}-${{ github.run_attempt }}/.visual-hive
          if-no-files-found: error
          include-hidden-files: true
          retention-days: 14
`, needs, runnerOwnedVerifierSetupSteps(config, "${{ github.event.pull_request.head.sha }}", "needs.setup-authorization.outputs.operation != 'uninstall'"), downloadArtifactActionSHA,
		indentWorkflowShell(pullRequestChangedFilesCaptureShell(), 10), indentWorkflowShell(runnerOwnedEvidenceRebuildShell(true), 10), string(expectedJSON),
		indentWorkflowShell(pullRequestEnforcementShell(), 10), config.VisualHiveRef, indentWorkflowShell(pullRequestSourceAdmissibilityShell(), 10),
		uploadArtifactActionSHA, config.ACMMLevel, uploadArtifactActionSHA, uploadArtifactActionSHA)
}

// pullRequestChangedFilesCaptureShell obtains Git's NUL-delimited path bytes
// and writes the one canonical newline-delimited form accepted by both the
// producer and the runner-owned verifier. Paths that cannot be represented
// without quoting or trimming fail closed.
func pullRequestChangedFilesCaptureShell() string {
	return `set -euo pipefail
[[ "$HIVE_BASE_SHA" =~ ^[a-f0-9]{40}$ ]]
[[ "$HIVE_HEAD_SHA" =~ ^[a-f0-9]{40}$ ]]
test -n "$HIVE_CHANGED_FILES"
rm -f -- "$HIVE_CHANGED_FILES"
node <<'NODE'
const fs = require("fs");
const path = require("path");
const { spawnSync } = require("child_process");
const fail = (message) => { throw new Error(message); };
const base = String(process.env.HIVE_BASE_SHA || "");
const head = String(process.env.HIVE_HEAD_SHA || "");
const output = String(process.env.HIVE_CHANGED_FILES || "");
if (!/^[a-f0-9]{40}$/u.test(base) || !/^[a-f0-9]{40}$/u.test(head) || !output) fail("Exact changed-file capture identity is missing");
const result = spawnSync("git", ["diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", base, head], {
  encoding: null,
  env: { ...process.env, GIT_EXTERNAL_DIFF: "", GIT_CONFIG_COUNT: "0" },
  maxBuffer: 16777216
});
if (result.error || result.status !== 0 || !Buffer.isBuffer(result.stdout) || result.stdout.length > 16777216) fail("NUL-delimited changed-file scan failed");
const decoder = new TextDecoder("utf-8", { fatal: true });
const files = [];
let start = 0;
for (let index = 0; index < result.stdout.length; index += 1) {
  if (result.stdout[index] !== 0) continue;
  const value = decoder.decode(result.stdout.subarray(start, index));
  const parts = value.split("/");
  if (!value || value.startsWith("/") || value.includes("\\") || /[\u0000-\u001f\u007f]/u.test(value) || parts.some((part) => !part || part === "." || part === ".." || part.trim() !== part)) {
    fail("Changed-file path cannot be represented exactly in the Visual Hive scope");
  }
  files.push(value);
  start = index + 1;
}
if (start !== result.stdout.length) fail("Changed-file scan was not NUL terminated");
if (new Set(files).size !== files.length) fail("Changed-file scan contains duplicate paths");
files.sort((left, right) => Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8")));
const serialized = files.length ? files.join("\n") + "\n" : "";
fs.mkdirSync(path.dirname(path.resolve(output)), { recursive: true, mode: 0o700 });
fs.writeFileSync(output, serialized, { flag: "wx", mode: 0o600 });
const stat = fs.lstatSync(output);
if (!stat.isFile() || stat.isSymbolicLink() || stat.nlink !== 1 || stat.size !== Buffer.byteLength(serialized) || fs.readFileSync(output, "utf8") !== serialized) fail("Canonical changed-file scope changed while it was written");
NODE`
}

func isolatedRepositoryJobIDs(config Config) []string {
	result := make([]string, 0, len(config.TestCommands))
	for _, command := range config.TestCommands {
		if len(command) > 0 {
			result = append(result, fmt.Sprintf("repository-test-%03d", len(result)+1))
		}
	}
	return result
}

func productionPrerequisiteShell() string {
	return `set -euo pipefail
node <<'NODE'
const needs = JSON.parse(process.env.HIVE_NEEDS_JSON || "{}");
const expectedRepositoryJobs = JSON.parse(process.env.HIVE_EXPECTED_REPOSITORY_JOBS || "[]");
const expected = [...expectedRepositoryJobs, "visual-hive-execution"].sort();
const actual = Object.keys(needs).sort();
if (JSON.stringify(actual) !== JSON.stringify(expected)) {
  throw new Error("production runner prerequisite topology does not match the installed plan");
}
if (needs["visual-hive-execution"]?.result !== "success") {
  throw new Error("isolated Visual Hive execution did not complete successfully");
}
for (const name of expectedRepositoryJobs) {
  if (!needs[name] || !["success", "failure"].includes(needs[name].result)) {
    throw new Error("repository test job lacks a terminal runner result: " + name);
  }
}
NODE`
}

func runnerOwnedVerifierSetupSteps(config Config, targetRef, condition string) string {
	conditionLine := ""
	if strings.TrimSpace(condition) != "" {
		conditionLine = fmt.Sprintf("        if: ${{ %s }}\n", condition)
	}
	return fmt.Sprintf(`      - uses: actions/checkout@%s
%s        with:
          ref: %s
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/checkout@%s
%s        with:
          repository: %s
          ref: %s
          path: .hive-visual-tooling
          persist-credentials: false
      - uses: actions/setup-node@%s
%s        with:
          node-version: 22.23.1
          cache: npm
          cache-dependency-path: .hive-visual-tooling/package-lock.json
      - name: Rebuild exact immutable Visual Hive CLI on fresh runner
%s        working-directory: .hive-visual-tooling
        shell: bash
        env:
          HIVE_VISUAL_HIVE_REF: %s
        run: |
          set -euo pipefail
          test "$(git rev-parse HEAD)" = "$HIVE_VISUAL_HIVE_REF"
          npm ci
          npm run build
          test -z "$(git status --porcelain --untracked-files=no)"
          release_dir="$RUNNER_TEMP/visual-hive-release-${GITHUB_RUN_ID}"
          rm -rf -- "$release_dir"
          GITHUB_SHA="$HIVE_VISUAL_HIVE_REF" node scripts/build-release-bundle.mjs --output "$release_dir"
          test -f "$release_dir/release-manifest.json"
          echo "HIVE_VISUAL_HIVE_RELEASE_DIR=$release_dir" >> "$GITHUB_ENV"
      - name: Seal reverified release outside target checkout
%s        shell: bash
        env:
          HIVE_VISUAL_HIVE_REF: %s
        run: |
          set -euo pipefail
          case "$HIVE_VISUAL_HIVE_RELEASE_DIR" in
            "$RUNNER_TEMP"/visual-hive-release-*) ;;
            *) echo "Visual Hive release path escaped the runner temp root" >&2; exit 1 ;;
          esac
          trusted_tooling="$RUNNER_TEMP/visual-hive-tooling"
          test ! -e "$trusted_tooling"
          mv "$HIVE_VISUAL_HIVE_RELEASE_DIR" "$trusted_tooling"
          rm -rf -- .hive-visual-tooling
          cli="$trusted_tooling/visual-hive.mjs"
          test -f "$cli"
          node - "$trusted_tooling/release-manifest.json" "$HIVE_VISUAL_HIVE_REF" <<'NODE'
          const fs = require("fs");
          const [manifestPath, expectedCommit] = process.argv.slice(2);
          const manifest = JSON.parse(fs.readFileSync(manifestPath, "utf8"));
          if (manifest.schemaVersion !== "visual-hive.release.v1" || manifest.name !== "visual-hive" ||
              manifest.gitCommit !== expectedCommit || manifest.release !== true || manifest.clean !== true ||
              manifest.entrypoint !== "visual-hive.mjs") {
            throw new Error("rebuilt Visual Hive release identity is incomplete or mismatched");
          }
          NODE
          sudo chown -R root:root "$trusted_tooling"
          sudo chmod -R a-w "$trusted_tooling"
          echo "VISUAL_HIVE_CLI=$cli" >> "$GITHUB_ENV"
`, checkoutActionSHA, conditionLine, targetRef, checkoutActionSHA, conditionLine, config.VisualHiveRepo, config.VisualHiveRef,
		setupNodeActionSHA, conditionLine, conditionLine, config.VisualHiveRef, conditionLine, config.VisualHiveRef)
}

func runnerOwnedEvaluationScopeScript() string {
	return `const fs = require("fs");
 const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
 const runnerOutcome = readJSON(".visual-hive/hive-runner-outcome.json");
 if (runnerOutcome.schemaVersion !== "hive.visual-runner-outcome.v1" || !["success", "failure"].includes(runnerOutcome.outcome) ||
     !["success", "failure"].includes(runnerOutcome.conclusion)) {
   throw new Error("runner-owned Visual pipeline outcome receipt is malformed");
 }
 const pipeline = readJSON(".visual-hive/pipeline.json");
const report = readJSON(".visual-hive/report.json");
if (!Number.isInteger(pipeline.exitCode) || !["passed", "failed", "blocked"].includes(pipeline.status)) {
  throw new Error("pipeline report lacks a terminal deterministic status");
}
fs.writeFileSync(".visual-hive/pipeline-exit-code.txt", String(pipeline.exitCode) + "\n");

 const planPath = ".visual-hive/plan.json";
 const plan = fs.existsSync(planPath) ? readJSON(planPath) : {};
const planRows = Array.isArray(plan.items) ? plan.items : [];
const planExcluded = Array.isArray(plan.excluded) ? plan.excluded : [];
const reportSelected = Array.isArray(report.selectedContracts) ? report.selectedContracts : [];
const reportExcluded = Array.isArray(report.excludedContracts) ? report.excludedContracts : [];
const results = Array.isArray(report.results) ? report.results : [];
const normalizedID = (value) => typeof value === "string" && value.trim() ? value.trim() : null;
const rowID = (row) => row && typeof row === "object" ? normalizedID(row.contractId || row.id) : null;
const sorted = (values) => [...values].sort();
const unique = (values) => new Set(values).size === values.length;
const sameIDs = (left, right) => JSON.stringify(sorted(left)) === JSON.stringify(sorted(right));
const plannedIDs = planRows.map(rowID);
const excludedPlanIDs = planExcluded.map(rowID);
const selectedIDs = reportSelected.map(normalizedID);
const excludedReportIDs = reportExcluded.map(rowID);
const resultIDs = results.map((result) => result && typeof result === "object" ? normalizedID(result.contractId) : null);
const identityComplete = Boolean(planPath) && Array.isArray(plan.items) && Array.isArray(plan.excluded) &&
  Array.isArray(report.selectedContracts) && Array.isArray(report.excludedContracts) && Array.isArray(report.results) &&
  [...plannedIDs, ...excludedPlanIDs, ...selectedIDs, ...excludedReportIDs, ...resultIDs].every(Boolean) &&
  results.every((result) => result && typeof result === "object" && normalizedID(result.contractId) && ["passed", "failed", "created", "skipped"].includes(result.status));
if (!identityComplete || !unique(plannedIDs) || !unique(excludedPlanIDs) || !unique(selectedIDs) || !unique(excludedReportIDs) || !unique(resultIDs) ||
    !sameIDs(plannedIDs, selectedIDs) || !sameIDs(excludedPlanIDs, excludedReportIDs) || !sameIDs(selectedIDs, resultIDs) ||
    selectedIDs.some((contractID) => excludedPlanIDs.includes(contractID))) {
  throw new Error("plan and report lack an exact one-to-one contract evaluation binding");
}
 const passedIDs = runnerOutcome.outcome === "success"
   ? sorted(results.filter((result) => result.status === "passed").map((result) => normalizedID(result.contractId)))
   : [];
fs.writeFileSync(".visual-hive/evaluated-plan-contracts.txt", passedIDs.length ? passedIDs.join("\n") + "\n" : "");
fs.writeFileSync(".visual-hive/evaluated-contracts.txt", passedIDs.length ? passedIDs.join("\n") + "\n" : "");
 const authoritative = runnerOutcome.outcome === "success" && plannedIDs.length > 0 && passedIDs.length === plannedIDs.length && pipeline.mode === "full" && plan.mode === "full" && report.mode === "full" &&
   pipeline.status === "passed" && pipeline.exitCode === 0 && report.status === "passed";
 fs.writeFileSync(".visual-hive/authoritative-resolution.txt", authoritative ? "true\n" : "false\n");`
}

func runnerOwnedPlanRegenerationAndEvaluationShell(pullRequest bool) string {
	planCommand := `"${safe_env[@]}" node "$VISUAL_HIVE_CLI" plan --config visual-hive.config.yaml --mode full --output .visual-hive/plan.json`
	if pullRequest {
		planCommand = `"${safe_env[@]}" node "$VISUAL_HIVE_CLI" plan --config visual-hive.config.yaml --mode pr --changed-files .visual-hive/changed-files.pr.txt --output .visual-hive/plan.json`
	}
	return `rm -f .visual-hive/plan.json .visual-hive/plan.full.json
safe_env=(env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL -u NODE_OPTIONS -u BASH_ENV -u ENV)
` + planCommand + `
test -f .visual-hive/plan.json
test ! -e .visual-hive/plan.full.json
node <<'NODE'
` + runnerOwnedEvaluationScopeScript() + `
NODE`
}

// runnerOwnedResolutionScopeReceiptScript admits repository-level resolution
// scopes only from artifacts freshly regenerated by the immutable CLI in the
// verifier job. The rebuild removes the target-authored copies first, so a
// successful validation is also a current-run source binding.
func runnerOwnedResolutionScopeReceiptScript() string {
	return `const fs = require("fs");
const readJSON = (file) => JSON.parse(fs.readFileSync(file, "utf8"));
const fail = (message) => { throw new Error(message); };
const scopes = fs.readFileSync(".visual-hive/evaluated-contracts.txt", "utf8").split(/\r?\n/).map((value) => value.trim()).filter(Boolean);

const layers = readJSON(".visual-hive/testing-layers.json");
const layerStatuses = new Set(["covered", "partial", "missing", "not_applicable", "unknown"]);
if (layers.schemaVersion !== 1 || !Array.isArray(layers.layers) || layers.layers.length !== 12) fail("runner-owned testing-layer receipt is incomplete");
const layerIDs = layers.layers.map((layer) => layer && Number.isInteger(layer.id) ? layer.id : -1);
if (new Set(layerIDs).size !== 12 || layerIDs.some((id) => id < 0 || id > 11) ||
    layers.layers.some((layer) => !layerStatuses.has(layer.status) || !Array.isArray(layer.evidence) || !Array.isArray(layer.gaps) || !Array.isArray(layer.skippedReasons))) {
  fail("runner-owned testing-layer receipt is malformed");
}
const resolvedLayerStatuses = new Set(["covered", "not_applicable"]);
for (const layer of layers.layers) {
  if (resolvedLayerStatuses.has(layer.status) && layer.gaps.length === 0 && layer.skippedReasons.length === 0) {
    scopes.push("testing-layer:" + layer.id);
  }
}

const workflows = readJSON(".visual-hive/workflows.json");
if (workflows.schemaVersion !== 1 || !workflows.summary || typeof workflows.summary !== "object" ||
    !Array.isArray(workflows.workflows) || workflows.workflows.length === 0 || !Array.isArray(workflows.findings) ||
    workflows.workflows.some((workflow) => !workflow || typeof workflow.path !== "string" || !workflow.path.trim())) {
  fail("runner-owned workflow-safety receipt is malformed");
}
const workflowPaths = workflows.workflows.map((workflow) => workflow.path.trim());
if (new Set(workflowPaths).size !== workflowPaths.length) fail("runner-owned workflow-safety receipt contains duplicate paths");
if (workflows.findings.length === 0) scopes.push("workflow-safety");

const providers = readJSON(".visual-hive/provider-results.json");
const evidence = readJSON(".visual-hive/evidence-packet.json");
const report = readJSON(".visual-hive/report.json");
if (providers.schemaVersion !== 1 || !Array.isArray(providers.providers) || providers.providers.length === 0 || !Array.isArray(evidence.providers)) {
  fail("runner-owned provider-governance receipt is incomplete");
}
const providerRows = providers.providers.map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  resultID: provider && provider.result && typeof provider.result.providerId === "string" ? provider.result.providerId.trim() : "",
  status: provider && provider.result && typeof provider.result.status === "string" ? provider.result.status : "",
  uploadStatus: provider && provider.result && provider.result.upload && typeof provider.result.upload.status === "string" ? provider.result.upload.status : ""
}));
const evidenceRows = evidence.providers.map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  status: provider && typeof provider.status === "string" ? provider.status : "",
  uploadStatus: provider && provider.upload && typeof provider.upload.status === "string" ? provider.upload.status : ""
}));
const reportRows = (Array.isArray(report.providerResults) ? report.providerResults : []).map((provider) => ({
  id: provider && typeof provider.providerId === "string" ? provider.providerId.trim() : "",
  status: provider && typeof provider.status === "string" ? provider.status : "",
  uploadStatus: provider && provider.upload && typeof provider.upload.status === "string" ? provider.upload.status : ""
}));
const providerStatuses = new Set(["passed", "failed", "skipped", "missing_credentials", "mock"]);
const providerUploadStatuses = new Set(["uploaded", "skipped", "blocked", "missing_credentials", "failed", "dry_run"]);
const providerReceipt = (row) => row.id + "\u0000" + row.status + "\u0000" + row.uploadStatus;
if (providerRows.some((row) => !row.id || row.id !== row.resultID || !providerStatuses.has(row.status) || (row.uploadStatus && !providerUploadStatuses.has(row.uploadStatus))) ||
    new Set(providerRows.map((row) => row.id)).size !== providerRows.length ||
    reportRows.some((row) => !row.id || !providerStatuses.has(row.status) || (row.uploadStatus && !providerUploadStatuses.has(row.uploadStatus))) ||
    new Set(reportRows.map((row) => row.id)).size !== reportRows.length ||
    JSON.stringify([...reportRows, ...providerRows].map(providerReceipt).sort()) !== JSON.stringify(evidenceRows.map(providerReceipt).sort())) {
  fail("runner-owned provider-governance receipt does not match rebuilt evidence");
}
const issueProducingProviderStatuses = new Set(["failed", "missing_credentials", "blocked"]);
if (evidenceRows.every((row) => !issueProducingProviderStatuses.has(row.status) && !issueProducingProviderStatuses.has(row.uploadStatus))) {
  scopes.push("provider-governance");
}

const evaluated = [...new Set(scopes)].sort();
fs.writeFileSync(".visual-hive/evaluated-contracts.txt", evaluated.join("\n") + "\n");`
}

func runnerOwnedEvidenceRebuildShell(pullRequest bool) string {
	return `set -euo pipefail
runner_workspace="$(readlink -f "$GITHUB_WORKSPACE")"
review_workspace="` + isolatedTargetWorkspace + `"
test "$runner_workspace" = "$GITHUB_WORKSPACE"
test ! -e "` + isolatedTargetRoot + `"
sudo install -d -o root -g root -m 0755 "` + isolatedTargetRoot + `" "$review_workspace"
sudo mount --bind "$runner_workspace" "$review_workspace"
cleanup_review_workspace() {
  cd "$runner_workspace" || true
  if mountpoint -q "$review_workspace"; then
    sudo umount "$review_workspace" || true
  fi
}
trap cleanup_review_workspace EXIT
mountpoint -q "$review_workspace"
test "$(stat -Lc '%d:%i' "$review_workspace")" = "$(stat -Lc '%d:%i' "$runner_workspace")"
cd "$review_workspace"
test -f "$VISUAL_HIVE_CLI"
test ! -w "$VISUAL_HIVE_CLI"
test -d .visual-hive
if find .visual-hive -type l -print -quit | grep -q .; then
  echo "Raw evidence contains a symbolic link" >&2
  exit 1
fi
artifact_files="$(find .visual-hive -type f | wc -l)"
artifact_bytes="$(du -sb .visual-hive | cut -f 1)"
test "$artifact_files" -le 5000
test "$artifact_bytes" -le 1073741824
 rm -f .visual-hive/evidence-packet.json .visual-hive/evidence-summary.md .visual-hive/verdict.json .visual-hive/verdict.md .visual-hive/issues.json .visual-hive/issues.md .visual-hive/baselines.json .visual-hive/artifacts-index.json .visual-hive/capability-parity.json .visual-hive/workflows.json .visual-hive/provider-results.json .visual-hive/testing-layers.json .visual-hive/testing-layers.md
 ` + runnerOwnedPlanRegenerationAndEvaluationShell(pullRequest) + `
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" workflows --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" providers list --config visual-hive.config.yaml --mock-results --format json
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" evidence --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" layers --config visual-hive.config.yaml --format json
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" verdict --config visual-hive.config.yaml
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" issues --config visual-hive.config.yaml --write
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" baselines list --config visual-hive.config.yaml --write
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" hive integration-smoke --config visual-hive.config.yaml --mode measured
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" capabilities --config visual-hive.config.yaml
node <<'NODE'
` + runnerOwnedResolutionScopeReceiptScript() + `
NODE
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" artifacts --config visual-hive.config.yaml --complete
cleanup_review_workspace
trap - EXIT
if mountpoint -q "$review_workspace"; then
  echo "Runner-owned review workspace remained mounted" >&2
  exit 1
fi`
}

func pullRequestEnforcementShell() string {
	return `set -euo pipefail
if [ "$HIVE_SETUP_OPERATION" = "uninstall" ]; then
  test "$HIVE_SETUP_AUTHORIZED" = "true"
  echo "Exact out-of-band-authorized Hive uninstall; all managed files are absent at this immutable head."
  exit 0
fi
node <<'NODE'
const fs = require("fs");
          const needs = JSON.parse(process.env.HIVE_NEEDS_JSON || "{}");
          const expected = JSON.parse(process.env.HIVE_EXPECTED_REPOSITORY_JOBS || "[]");
          const actualRepositoryJobs = Object.keys(needs).filter((name) => name.startsWith("repository-test-")).sort();
          if (JSON.stringify(actualRepositoryJobs) !== JSON.stringify(expected)) {
            throw new Error("runner-owned repository job topology does not match the installed plan");
          }
          if (!needs["visual-hive-execution"] || !["success", "failure"].includes(needs["visual-hive-execution"].result)) {
            throw new Error("isolated target execution lacks a terminal runner result");
          }
          for (const name of expected) {
            if (!needs[name] || !["success", "failure"].includes(needs[name].result)) {
              throw new Error("repository test job lacks a terminal runner result: " + name);
            }
          }
          const repositoryFailed = expected.some((name) => needs[name].result === "failure");
          const pipeline = JSON.parse(fs.readFileSync(".visual-hive/pipeline.json", "utf8"));
          const report = JSON.parse(fs.readFileSync(".visual-hive/report.json", "utf8"));
          const verdict = JSON.parse(fs.readFileSync(".visual-hive/verdict.json", "utf8"));
          const readiness = fs.existsSync(".visual-hive/readiness.json") ? JSON.parse(fs.readFileSync(".visual-hive/readiness.json", "utf8")) : {};
          const visualVerdict = verdict?.summary?.visualHiveVerdict;
          const summary = report.summary || {};
          const verdictSummary = verdict.summary || {};
          const contributions = Array.isArray(verdict.gatingContributions) ? verdict.gatingContributions : [];
          const screenshots = (Array.isArray(report.results) ? report.results : []).flatMap((result) => Array.isArray(result.screenshotAssertions) ? result.screenshotAssertions : []);
          const blocking = contributions.filter((item) => item && item.gating === true && item.status !== "passed");
          const visualPassed = needs["visual-hive-execution"].result === "success" && process.env.HIVE_TRUSTED_REBUILD_OUTCOME === "success" && pipeline.exitCode === 0 && ["passed", "warning"].includes(visualVerdict) && Array.isArray(verdict.gatingContributions) && blocking.length === 0;
          const missing = screenshots.filter((item) => item && item.status === "missing_baseline");
          const readinessBlocked = (Array.isArray(readiness.gates) ? readiness.gates : []).filter((gate) => gate && gate.status === "blocked");
          const exclusiveMissingBaselineReadiness = readiness.status === "blocked" && readinessBlocked.some((gate) => gate.id === "baselines:missing-baseline") &&
            readinessBlocked.every((gate) => ["deterministic:status", "baselines:missing-baseline"].includes(gate.id));
          const exclusiveMissingBaseline = ["failed", "blocked"].includes(report.status) && verdictSummary.visualHiveVerdict === "blocked" &&
            Array.isArray(verdictSummary.failedBecause) && verdictSummary.failedBecause.length === 0 &&
            Number(summary.missingBaselines) > 0 && Number(summary.missingBaselines) === missing.length &&
            Number(summary.visualDiffs || 0) === 0 && Number(summary.consoleErrors || 0) === 0 && Number(summary.pageErrors || 0) === 0 && Number(summary.flowStepsFailed || 0) === 0 &&
            exclusiveMissingBaselineReadiness && blocking.some((item) => item.kind === "missing_baseline") && blocking.every((item) => item.status === "blocked" && ["deterministic_run", "contract_result", "missing_baseline", "readiness_gate"].includes(item.kind)) &&
            missing.every((item) => item.contractId && (item.screenshotName || item.name) && item.baselinePath && item.actualPath);
          if (!repositoryFailed && process.env.HIVE_SETUP_OPERATION === "setup" && process.env.HIVE_SETUP_AUTHORIZED === "true" && exclusiveMissingBaseline) {
            const proof = {
              schemaVersion: "hive.setup-baseline-required.v1", repository: process.env.HIVE_REPOSITORY,
              headSha: process.env.HIVE_HEAD_SHA, missingBaselines: missing.length,
              authorizationContext: process.env.HIVE_SETUP_CONTEXT, bindingDigest: process.env.HIVE_SETUP_BINDING_DIGEST,
              diffDigest: process.env.HIVE_SETUP_DIFF_DIGEST,
              disposition: "exact_setup_authorized_missing_baselines_only; hosted_post_merge_capture_required"
            };
            fs.writeFileSync(".visual-hive/setup-baseline-required.json", JSON.stringify(proof, null, 2) + "\n");
            console.log("::warning::Exact setup is blocked only by absent initial baselines. Merge may proceed; Hive must capture and review Linux baselines before production activation or lifecycle writes.");
            process.exit(0);
          }
          if (repositoryFailed && visualPassed && process.env.HIVE_SETUP_AUTHORIZED === "true") {
            const proof = {
              schemaVersion: "hive.setup-bootstrap-repository-failure.v2",
              repository: process.env.HIVE_REPOSITORY,
              headSha: process.env.HIVE_HEAD_SHA,
              repositoryOutcome: "failure",
              visualOutcome: "success",
              authorizationContext: process.env.HIVE_SETUP_CONTEXT,
              bindingDigest: process.env.HIVE_SETUP_BINDING_DIGEST,
              diffDigest: process.env.HIVE_SETUP_DIFF_DIGEST,
              disposition: "out_of_band_exact_setup_authorization; production Hive repairs the runner-owned repository-test failure"
            };
            fs.writeFileSync(".visual-hive/setup-bootstrap-repository-failure.json", JSON.stringify(proof, null, 2) + "\n");
            console.log("::warning::The runner-owned repository test plan is already failing. This exact out-of-band-authorized setup PR may install Hive; production Hive will open and repair the failure. Every unauthorized PR remains blocked.");
            process.exit(0);
          }
          if (repositoryFailed) throw new Error("Repository test plan failed");
          if (!visualPassed) throw new Error("Isolated deterministic Visual Hive verdict failed");
NODE`
}

func pullRequestSourceAdmissibilityShell() string {
	return `set -euo pipefail
test "$HIVE_EVENT_NAME" = "pull_request"
test -z "$HIVE_SETUP_OPERATION"
test "$(git rev-parse HEAD)" = "$HIVE_HEAD_SHA"
test -f "$VISUAL_HIVE_CLI"
test ! -w "$VISUAL_HIVE_CLI"
test -d .visual-hive
test ! -L .visual-hive
if [ -e .visual-hive/proof ] || [ -L .visual-hive/proof ]; then
  test -d .visual-hive/proof
  test ! -L .visual-hive/proof
else
  install -d -m 0700 .visual-hive/proof
fi
if [ -e .visual-hive/proof/pr ] || [ -L .visual-hive/proof/pr ]; then
  test -d .visual-hive/proof/pr
  test ! -L .visual-hive/proof/pr
else
  install -d -m 0700 .visual-hive/proof/pr
fi
rm -rf .visual-hive/proof/pr/inputs .visual-hive/proof/pr/baselines
rm -f .visual-hive/proof/pr/source-binding.json .visual-hive/proof/pr/issues.present.json
` + pullRequestBaseWorkflowSnapshotShell() + `
admissibility_path="$RUNNER_TEMP/hive-pr-admissibility-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.txt"
rm -f -- "$admissibility_path"
export HIVE_PR_ADMISSIBILITY_PATH="$admissibility_path"
node <<'NODE'
` + pullRequestSourceBindingScript() + `
NODE
test -f "$admissibility_path"
test ! -L "$admissibility_path"
admissible="$(tr -d '\r\n' < "$admissibility_path")"
rm -f -- "$admissibility_path"
case "$admissible" in true|false) ;; *) echo "Invalid PR source admissibility result" >&2; exit 1 ;; esac
if [ "$admissible" = "false" ]; then
  test ! -e .visual-hive/proof/pr/source-binding.json
  printf 'admissible=false\n' >> "$GITHUB_OUTPUT"
  exit 0
fi
test -f .visual-hive/proof/pr/source-binding.json
test -f .visual-hive/proof/pr/runtime.json
node <<'NODE'
` + pullRequestPresentIssuesScript() + `
NODE
test -f .visual-hive/proof/pr/issues.present.json
chmod 0444 .visual-hive/proof/pr/source-binding.json .visual-hive/proof/pr/issues.present.json
find .visual-hive/proof/pr/inputs .visual-hive/proof/pr/baselines -type f -exec chmod 0444 -- {} +
# The source binding is part of the source artifact, so the producer's complete
# index must be regenerated only after the real runtime and runner-owned receipt
# are both fixed. Never manufacture a runtime sidecar in this verifier.
rm -f .visual-hive/artifacts-index.json
safe_env=(env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY -u ACTIONS_ID_TOKEN_REQUEST_TOKEN -u ACTIONS_ID_TOKEN_REQUEST_URL -u NODE_OPTIONS -u BASH_ENV -u ENV)
"${safe_env[@]}" node "$VISUAL_HIVE_CLI" artifacts --config visual-hive.config.yaml --complete
test -f .visual-hive/artifacts-index.json
printf 'admissible=true\n' >> "$GITHUB_OUTPUT"`
}

// pullRequestBaseWorkflowSnapshotShell freezes the authorizing workflow
// definition from the exact base commit. The checked-out pull request head is
// deliberately not an authority for the definition that admits its own source
// packet.
func pullRequestBaseWorkflowSnapshotShell() string {
	return `test "$HIVE_WORKFLOW_PATH" = ".github/workflows/visual-hive-pr.yml"
[[ "$HIVE_BASE_SHA" =~ ^[a-f0-9]{40}$ ]]
workflow_snapshot=.visual-hive/proof/pr/inputs/visual-hive-pr.yml
install -d -m 0700 .visual-hive/proof/pr/inputs
workflow_object="$HIVE_BASE_SHA:.github/workflows/visual-hive-pr.yml"
workflow_bytes="$(git cat-file -s "$workflow_object")"
case "$workflow_bytes" in ''|*[!0-9]*) echo "Invalid base workflow size" >&2; exit 1 ;; esac
test "$workflow_bytes" -gt 0
test "$workflow_bytes" -le 1048576
umask 077
git show "$HIVE_BASE_SHA:.github/workflows/visual-hive-pr.yml" > "$workflow_snapshot"
test -f "$workflow_snapshot"
test ! -L "$workflow_snapshot"
test "$(stat -c %h "$workflow_snapshot")" = "1"
snapshot_bytes="$(wc -c < "$workflow_snapshot" | tr -d '[:space:]')"
test "$snapshot_bytes" = "$workflow_bytes"
base_workflow_sha256="$(git show "$HIVE_BASE_SHA:.github/workflows/visual-hive-pr.yml" | sha256sum | cut -d ' ' -f 1)"
snapshot_sha256="$(sha256sum "$workflow_snapshot" | cut -d ' ' -f 1)"
[[ "$base_workflow_sha256" =~ ^[a-f0-9]{64}$ ]]
test "$snapshot_sha256" = "$base_workflow_sha256"
export HIVE_BASE_WORKFLOW_SHA256="$base_workflow_sha256"`
}

// pullRequestPresentIssuesScript preserves the runner-owned issue report
// metadata while removing every absence-like lifecycle candidate. A changed-file
// PR scan can report present findings, but it can never claim that a target issue
// was resolved or otherwise absent.
func pullRequestPresentIssuesScript() string {
	return `const fs = require("fs");
const path = require("path");
const fail = (message) => { throw new Error(message); };
const root = fs.realpathSync(process.cwd());
const sourceRelative = ".visual-hive/issues.json";
const outputRelative = ".visual-hive/proof/pr/issues.present.json";
const ordinaryFile = (relative, label) => {
  const absolute = path.resolve(root, ...relative.split("/"));
  const escaped = path.relative(root, absolute);
  if (!escaped || escaped.startsWith(".." + path.sep) || path.isAbsolute(escaped)) fail(label + " escaped the repository root");
  let current = root;
  for (const part of relative.split("/")) {
    current = path.join(current, part);
    if (!fs.existsSync(current)) fail(label + " is missing");
    if (fs.lstatSync(current).isSymbolicLink()) fail(label + " traverses a symbolic link");
  }
  const stat = fs.lstatSync(absolute);
  if (!stat.isFile() || stat.nlink !== 1 || stat.size > 16777216) fail(label + " is not one bounded ordinary file");
  return absolute;
};
const source = ordinaryFile(sourceRelative, "runner-owned issue report");
const report = JSON.parse(fs.readFileSync(source, "utf8"));
if (!report || typeof report !== "object" || Array.isArray(report) || report.schemaVersion !== "visual-hive.issues.v1" || !Array.isArray(report.issues)) fail("Runner-owned issue report is malformed");
const knownStatuses = new Set(["open_candidate", "update_candidate", "resolved_candidate", "suppressed", "blocked"]);
for (const [index, issue] of report.issues.entries()) {
  if (!issue || typeof issue !== "object" || Array.isArray(issue) || typeof issue.status !== "string" || !knownStatuses.has(issue.status) || typeof issue.issueKind !== "string" || !issue.issueKind || typeof issue.severity !== "string" || !issue.severity) fail("Runner-owned issue report has an unknown or malformed status at index " + index);
}
const presentIssues = report.issues.filter((issue) => issue.status === "open_candidate" || issue.status === "update_candidate");
if (presentIssues.some((issue) => issue.status !== "open_candidate" && issue.status !== "update_candidate")) fail("PR issue filter retained a non-present lifecycle candidate");
const byKind = {};
const bySeverity = {};
for (const issue of presentIssues) {
  byKind[issue.issueKind] = (byKind[issue.issueKind] || 0) + 1;
  bySeverity[issue.severity] = (bySeverity[issue.severity] || 0) + 1;
}
const filtered = {
  ...report,
  summary: {
    total: presentIssues.length,
    openCandidates: presentIssues.filter((issue) => issue.status === "open_candidate").length,
    updateCandidates: presentIssues.filter((issue) => issue.status === "update_candidate").length,
    resolvedCandidates: 0,
    suppressed: 0,
    blocked: 0,
    byKind,
    bySeverity
  },
  issues: presentIssues
};
const output = path.resolve(root, ...outputRelative.split("/"));
fs.mkdirSync(path.dirname(output), { recursive: true, mode: 0o700 });
const serialized = JSON.stringify(filtered, null, 2) + "\n";
fs.writeFileSync(output, serialized, { flag: "wx", mode: 0o600 });
const written = ordinaryFile(outputRelative, "present-only PR issue report");
if (!fs.readFileSync(written).equals(Buffer.from(serialized))) fail("Present-only PR issue report changed while it was written");`
}

func pullRequestSourceBindingScript() string {
	return `const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

const fail = (message) => { throw new Error(message); };
const root = fs.realpathSync(process.cwd());
const hex40 = /^[a-f0-9]{40}$/u;
const hex64 = /^[a-f0-9]{64}$/u;
const positiveInteger = /^[1-9][0-9]*$/u;
const repositoryName = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u;
const requiredEnvironment = (name) => {
  const value = String(process.env[name] || "").trim();
  if (!value || value.includes("\0")) fail("Missing or invalid " + name);
  return value;
};
const sha256 = (value) => crypto.createHash("sha256").update(value).digest("hex");
const compareUTF8 = (left, right) => Buffer.compare(Buffer.from(left, "utf8"), Buffer.from(right, "utf8"));
// encoding/json escapes these code points even with otherwise valid UTF-8.
// Match its exact bytes so the Go verifier and the workflow hash one canonical
// stable-runtime representation.
const goJSON = (value) => JSON.stringify(value)
  .replaceAll("<", "\\u003c")
  .replaceAll(">", "\\u003e")
  .replaceAll("&", "\\u0026")
  .replaceAll("\u2028", "\\u2028")
  .replaceAll("\u2029", "\\u2029");
const normalizeRelative = (value, label) => {
  const normalized = String(value || "");
  if (!normalized || normalized.includes("\\") || normalized.startsWith("/") || /^[A-Za-z]:\//u.test(normalized) || /[\u0000-\u001f\u007f]/u.test(normalized) || normalized.split("/").some((part) => !part || part === "." || part === ".." || part.trim() !== part)) {
    fail(label + " is not a safe repository-relative path");
  }
  return normalized;
};
const regularFile = (relative, label) => {
  const normalized = normalizeRelative(relative, label);
  let current = root;
  for (const part of normalized.split("/")) {
    current = path.join(current, part);
    if (!fs.existsSync(current)) fail(label + " is missing");
    const stat = fs.lstatSync(current);
    if (stat.isSymbolicLink()) fail(label + " traverses a symbolic link");
  }
  const stat = fs.lstatSync(current);
  if (!stat.isFile() || stat.nlink !== 1) fail(label + " is not one ordinary file");
  const escaped = path.relative(root, fs.realpathSync(current));
  if (!escaped || escaped.startsWith(".." + path.sep) || path.isAbsolute(escaped)) fail(label + " escaped the repository root");
  return { path: normalized, absolute: current, bytes: stat.size, sha256: sha256(fs.readFileSync(current)) };
};
const copySealed = (sourceRelative, destinationRelative, label) => {
  const source = regularFile(sourceRelative, label + " source");
  const destination = normalizeRelative(destinationRelative, label + " destination");
  const absolute = path.resolve(root, ...destination.split("/"));
  const escaped = path.relative(root, absolute);
  if (!escaped || escaped.startsWith(".." + path.sep) || path.isAbsolute(escaped)) fail(label + " destination escaped the repository root");
  fs.mkdirSync(path.dirname(absolute), { recursive: true, mode: 0o700 });
  fs.copyFileSync(source.absolute, absolute, fs.constants.COPYFILE_EXCL);
  const copied = regularFile(destination, label + " snapshot");
  if (source.bytes !== copied.bytes || source.sha256 !== copied.sha256 || !fs.readFileSync(source.absolute).equals(fs.readFileSync(copied.absolute))) fail(label + " changed while it was snapshotted");
  return { path: copied.path, sha256: copied.sha256, bytes: copied.bytes };
};
const readJSON = (relative, label) => {
  const identity = regularFile(relative, label);
  return { identity, value: JSON.parse(fs.readFileSync(identity.absolute, "utf8")) };
};
const indexedFile = (relative, label, index) => {
  const normalized = normalizeRelative(relative, label);
  if (!normalized.startsWith(".visual-hive/")) fail(label + " is outside the indexed evidence root");
  const identity = regularFile(normalized, label);
  const entries = index.artifacts.filter((entry) => entry && typeof entry === "object" && !Array.isArray(entry) && entry.path === normalized);
  if (entries.length !== 1 || entries[0].bytes !== identity.bytes || entries[0].sha256 !== identity.sha256) fail(label + " does not match one complete artifact-index entry");
  return { path: identity.path, sha256: identity.sha256, bytes: identity.bytes };
};
const sortedUnique = (values, label) => {
  if (!Array.isArray(values) || values.some((value) => typeof value !== "string" || !value || value.trim() !== value || /[\u0000-\u001f\u007f]/u.test(value))) fail(label + " is invalid");
  const normalized = [...values];
  if (new Set(normalized).size !== normalized.length) fail(label + " contains duplicates");
  return [...normalized].sort(compareUTF8);
};
const readLines = (relative, label) => {
  const identity = regularFile(relative, label);
  const values = fs.readFileSync(identity.absolute, "utf8").split(/\r?\n/u);
  if (values.length && values[values.length - 1] === "") values.pop();
  if (values.some((value) => !value || value.trim() !== value || /[\u0000-\u001f\u007f]/u.test(value))) fail(label + " contains a non-canonical line identity");
  return { identity, values: sortedUnique(values, label) };
};
const same = (left, right) => JSON.stringify(left) === JSON.stringify(right);
// Keep this exactly aligned with Visual Hive's PlaywrightExecutionBinding.
// Stable browser/environment identity is a separate root-sealed source receipt.
const bindingFields = ["bindingMacSha256", "generatedConfigSha256", "generatedSpecSha256", "nonceSha256", "payloadSha256"];
const executionBinding = (value, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || !same(Object.keys(value).sort(compareUTF8), bindingFields)) fail(label + " is incomplete");
  const result = {};
  for (const field of bindingFields) {
    const digest = String(value[field] || "");
    if (!hex64.test(digest)) fail(label + " has an invalid " + field);
    result[field] = digest;
  }
  return result;
};
const pair = (value, label) => {
  const contractId = value && typeof value.contractId === "string" ? value.contractId.trim() : "";
  const targetId = value && typeof value.targetId === "string" ? value.targetId.trim() : "";
  if (!contractId || !targetId || contractId.includes("\0") || targetId.includes("\0")) fail(label + " lacks a target-bound contract identity");
  return { contractId, targetId };
};
const pairKey = (value) => value.contractId + "\0" + value.targetId;
const writeAdmissibility = (value) => {
  const runnerTemp = fs.realpathSync(requiredEnvironment("RUNNER_TEMP"));
  const output = path.resolve(requiredEnvironment("HIVE_PR_ADMISSIBILITY_PATH"));
  const relative = path.relative(runnerTemp, output);
  if (!relative || relative.startsWith(".." + path.sep) || path.isAbsolute(relative)) fail("PR admissibility output escaped runner temp");
  fs.writeFileSync(output, value ? "true\n" : "false\n", { flag: "wx", mode: 0o600 });
};

const eventName = requiredEnvironment("HIVE_EVENT_NAME");
const repository = requiredEnvironment("HIVE_REPOSITORY");
const pullRequestNumber = requiredEnvironment("HIVE_PULL_REQUEST_NUMBER");
const baseRepository = requiredEnvironment("HIVE_BASE_REPOSITORY");
const baseRepositoryID = requiredEnvironment("HIVE_BASE_REPOSITORY_ID");
const baseRef = requiredEnvironment("HIVE_BASE_REF");
const baseSHA = requiredEnvironment("HIVE_BASE_SHA").toLowerCase();
const headRepository = requiredEnvironment("HIVE_HEAD_REPOSITORY");
const headRepositoryID = requiredEnvironment("HIVE_HEAD_REPOSITORY_ID");
const headRef = requiredEnvironment("HIVE_HEAD_REF");
const headSHA = requiredEnvironment("HIVE_HEAD_SHA").toLowerCase();
const workflowRunID = requiredEnvironment("HIVE_WORKFLOW_RUN_ID");
const workflowRunAttempt = requiredEnvironment("HIVE_WORKFLOW_RUN_ATTEMPT");
const workflowName = requiredEnvironment("HIVE_WORKFLOW_NAME");
const workflowPath = requiredEnvironment("HIVE_WORKFLOW_PATH");
const baseWorkflowSHA256 = requiredEnvironment("HIVE_BASE_WORKFLOW_SHA256").toLowerCase();
const producerCommit = requiredEnvironment("HIVE_PRODUCER_COMMIT").toLowerCase();
const sourceArtifactName = requiredEnvironment("HIVE_SOURCE_ARTIFACT_NAME");
const bundleArtifactName = requiredEnvironment("HIVE_BUNDLE_ARTIFACT_NAME");
if (eventName !== "pull_request" || workflowName !== "Visual Hive PR" || workflowPath !== ".github/workflows/visual-hive-pr.yml") fail("PR workflow identity is not exact");
if (!repositoryName.test(repository) || !repositoryName.test(baseRepository) || !repositoryName.test(headRepository) || repository.toLowerCase() !== baseRepository.toLowerCase()) fail("PR repository identity is invalid");
if (![pullRequestNumber, baseRepositoryID, headRepositoryID, workflowRunID, workflowRunAttempt].every((value) => positiveInteger.test(value))) fail("PR numeric identity is invalid");
if (!hex40.test(baseSHA) || !hex40.test(headSHA) || !hex40.test(producerCommit) || !hex64.test(baseWorkflowSHA256)) fail("PR immutable commit identity is invalid");
if (producerCommit !== "` + visualHivePullRequestProducerCommit + `") fail("PR lane requires the exact audited Visual Hive runtime-sidecar producer");
if ([baseRef, headRef].some((value) => !value || value.startsWith("refs/pull/") || value.startsWith("refs/merge/") || /[\u0000-\u001f\u007f]/u.test(value))) fail("PR branch identity is invalid or merge-derived");
if (sourceArtifactName !== ` + "`visual-hive-pr-source-${workflowRunID}-${workflowRunAttempt}`" + ` || bundleArtifactName !== ` + "`visual-hive-pr-bundle-${workflowRunID}-${workflowRunAttempt}`" + `) fail("PR artifact names are not attempt-bound");

regularFile("visual-hive.config.yaml", "Visual Hive config");
const workflowSnapshot = regularFile(".visual-hive/proof/pr/inputs/visual-hive-pr.yml", "base-side PR workflow definition");
if (workflowSnapshot.sha256 !== baseWorkflowSHA256 || workflowSnapshot.bytes > 1048576) fail("Base-side PR workflow definition identity drifted");
const workflowIdentity = { path: workflowSnapshot.path, sha256: workflowSnapshot.sha256, bytes: workflowSnapshot.bytes };
const planDocument = readJSON(".visual-hive/plan.json", "PR plan");
const reportDocument = readJSON(".visual-hive/report.json", "PR report");
const changed = readLines(".visual-hive/changed-files.pr.txt", "changed-file scope");
const planContracts = readLines(".visual-hive/evaluated-plan-contracts.txt", "evaluated plan contracts");
const evaluationScopes = readLines(".visual-hive/evaluated-contracts.txt", "full evaluation scopes");
const plan = planDocument.value;
const report = reportDocument.value;
if (plan.schemaVersion !== 1 || plan.mode !== "pr" || !Array.isArray(plan.items) || !Array.isArray(plan.excluded) || !Array.isArray(plan.changedFiles) || !Array.isArray(plan.effectiveChangedFiles) || !Array.isArray(plan.ignoredChangedFiles)) fail("Runner-owned PR plan is malformed");
if (report.schemaVersion !== 2 || report.mode !== "pr" || report.status !== "passed" || !Array.isArray(report.selectedContracts) || !Array.isArray(report.results) || !Array.isArray(report.changedFiles)) fail("Runner-owned PR report is malformed");
const reportRepository = report.repository;
if (!reportRepository || typeof reportRepository !== "object" || Array.isArray(reportRepository) || reportRepository.provider !== "github-actions" ||
    String(reportRepository.repository || "").toLowerCase() !== repository.toLowerCase() || reportRepository.branch !== headRef || reportRepository.baseBranch !== baseRef ||
    String(reportRepository.commitSha || "").toLowerCase() !== headSHA || String(reportRepository.runId || "") !== workflowRunID ||
    String(reportRepository.runAttempt || "") !== workflowRunAttempt || reportRepository.workflow !== workflowName ||
    (reportRepository.pullRequestNumber !== undefined && Number(reportRepository.pullRequestNumber) !== Number(pullRequestNumber))) fail("Runner-owned PR report lacks the exact allowlisted GitHub execution identity");
const planChangedOrder = plan.changedFiles.map((value) => normalizeRelative(value, "planned changed file"));
const planChanged = sortedUnique(planChangedOrder, "planned changed files");
const effectiveChanged = plan.effectiveChangedFiles.map((value) => normalizeRelative(value, "effective changed file"));
sortedUnique(effectiveChanged, "effective changed files");
const ignoredChanged = plan.ignoredChangedFiles.map((value, index) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || typeof value.file !== "string" || typeof value.pattern !== "string" || typeof value.reason !== "string" ||
      !value.pattern || value.pattern.trim() !== value.pattern || !value.reason || value.reason.trim() !== value.reason || /[\u0000-\u001f\u007f]/u.test(value.pattern) || /[\u0000-\u001f\u007f]/u.test(value.reason)) fail("Ignored changed file " + index + " is malformed");
  return { file: normalizeRelative(value.file, "ignored changed file"), pattern: value.pattern, reason: value.reason };
});
const reportChanged = sortedUnique(report.changedFiles.map((value) => normalizeRelative(value, "reported changed file")), "reported changed files");
const exactChanged = changed.values.map((value) => normalizeRelative(value, "changed file")).sort(compareUTF8);
if (!same(planChanged, exactChanged) || !same(reportChanged, exactChanged)) fail("PR plan/report changed-file scope does not match the runner-owned diff");
const ignoredFiles = ignoredChanged.map((value) => value.file);
if (new Set(ignoredFiles).size !== ignoredFiles.length || ignoredFiles.some((value) => !exactChanged.includes(value))) fail("Ignored changed-file inventory is duplicated or outside the runner-owned diff");
const ignoredSet = new Set(ignoredFiles);
const expectedIgnored = planChangedOrder.filter((value) => ignoredSet.has(value));
const expectedEffective = planChangedOrder.filter((value) => !ignoredSet.has(value));
if (!same(ignoredFiles, expectedIgnored) || !same(effectiveChanged, expectedEffective)) fail("Effective and ignored changed files do not exactly partition the runner-owned diff");
const plannedPairs = plan.items.map((value, index) => pair(value, "plan item " + index)).sort((left, right) => compareUTF8(pairKey(left), pairKey(right)));
const resultPairs = report.results.map((value, index) => pair(value, "report result " + index)).sort((left, right) => compareUTF8(pairKey(left), pairKey(right)));
if (new Set(plannedPairs.map(pairKey)).size !== plannedPairs.length || new Set(resultPairs.map(pairKey)).size !== resultPairs.length) fail("PR target-contract binding contains duplicates");
const selectedContracts = sortedUnique(report.selectedContracts, "selected contracts");
const plannedContracts = sortedUnique(plannedPairs.map((value) => value.contractId), "planned contracts");
const resultContracts = sortedUnique(resultPairs.map((value) => value.contractId), "result contracts");
if (plannedPairs.length === 0) {
  if (selectedContracts.length || resultPairs.length || planContracts.values.length || report.executionBinding !== undefined) fail("No-op PR plan contains execution claims");
  writeAdmissibility(false);
  process.exit(0);
}
if (!same(plannedPairs, resultPairs) || !same(plannedContracts, selectedContracts) || !same(plannedContracts, resultContracts) || !same(plannedContracts, planContracts.values)) fail("PR plan, target results, and evaluated plan contracts are not exactly bound");
const planContractSet = new Set(planContracts.values);
const allowedLifecycleScope = (scope) => scope === "workflow-safety" || scope === "provider-governance" || /^testing-layer:(?:[0-9]|1[01])$/u.test(scope);
if (planContracts.values.some((contract) => !evaluationScopes.values.includes(contract)) || evaluationScopes.values.some((scope) => !planContractSet.has(scope) && !allowedLifecycleScope(scope))) fail("Full evaluation scopes do not exactly extend the executed plan contracts with deterministic lifecycle scopes");
if (report.results.some((value) => value.status !== "passed")) fail("Admissible PR source contains a non-passing target contract");

const runtimeDocument = readJSON(".visual-hive/proof/pr/runtime.json", "producer runtime sidecar");
const runtime = runtimeDocument.value;
if (runtime.schemaVersion !== "visual-hive.playwright-runtime.v1" || !runtime.capturedAt || Number.isNaN(Date.parse(runtime.capturedAt))) fail("Producer runtime sidecar is malformed");
if (!runtime.browser || typeof runtime.browser.name !== "string" || !runtime.browser.name.trim() || typeof runtime.browser.version !== "string" || !runtime.browser.version.trim()) fail("Producer runtime sidecar lacks browser identity");
const environment = runtime.environment;
if (!environment || ["os", "architecture", "nodeVersion", "playwrightVersion", "locale", "timezone", "userAgent"].some((field) => typeof environment[field] !== "string" || !environment[field].trim()) || !Number.isFinite(environment.deviceScaleFactor) || !Array.isArray(environment.fonts) || environment.fonts.length === 0) fail("Producer runtime sidecar lacks execution environment identity");
if (environment.fonts.some((font) => !font || typeof font.name !== "string" || !font.name.trim() || typeof font.available !== "boolean")) fail("Producer runtime sidecar has malformed font identity");
const runtimeBinding = executionBinding(runtime.executionBinding, "runtime execution binding");
const reportBinding = executionBinding(report.executionBinding, "report execution binding");
if (!same(runtimeBinding, reportBinding)) fail("Producer runtime sidecar is not bound to the exact report execution");
const artifactIndexDocument = readJSON(".visual-hive/artifacts-index.json", "complete artifact index");
const artifactIndex = artifactIndexDocument.value;
if (artifactIndex.schemaVersion !== 1 || artifactIndex.contentAddressed !== true || artifactIndex.complete !== true || !Array.isArray(artifactIndex.artifacts)) fail("Generated execution files lack a complete artifact index");
const generatedSpecPath = normalizeRelative(report.generatedSpecPath, "generated Playwright spec");
if (!generatedSpecPath.startsWith(".visual-hive/generated/")) fail("Generated Playwright spec is outside the canonical generated directory");
const generatedConfigPath = path.posix.join(path.posix.dirname(generatedSpecPath), "visual-hive.generated.config.cjs");
const generatedSpecIdentity = indexedFile(generatedSpecPath, "generated Playwright spec", artifactIndex);
const generatedConfigIdentity = indexedFile(generatedConfigPath, "generated Playwright config", artifactIndex);
if (generatedSpecIdentity.sha256 !== runtimeBinding.generatedSpecSha256 || generatedConfigIdentity.sha256 !== runtimeBinding.generatedConfigSha256) fail("Generated spec/config bytes do not match the runtime execution binding");
const publicIdentity = (value) => ({ path: value.path, sha256: value.sha256, bytes: value.bytes });
const exactIdentity = (value, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || !same(Object.keys(value).sort(compareUTF8), ["bytes", "path", "sha256"]) ||
      normalizeRelative(value.path, label) !== value.path || !hex64.test(String(value.sha256 || "")) || !Number.isSafeInteger(value.bytes) || value.bytes < 0) fail(label + " is malformed");
  return { path: value.path, sha256: value.sha256, bytes: value.bytes };
};
const decodeCanonicalBase64 = (value, label) => {
  if (typeof value !== "string" || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/u.test(value)) fail(label + " is not canonical base64");
  const decoded = Buffer.from(value, "base64");
  if (decoded.toString("base64") !== value) fail(label + " is not canonical base64");
  return decoded;
};
const attestationDocument = readJSON("` + runnerAttestationPath + `", "runner execution attestation");
const attestationIdentity = indexedFile("` + runnerAttestationPath + `", "runner execution attestation", artifactIndex);
if (!same(publicIdentity(attestationDocument.identity), attestationIdentity)) fail("Runner execution attestation is not exactly indexed");
const attestation = attestationDocument.value;
const attestationFields = ["algorithm", "keyBase64", "keyCommitmentSha256", "macSha256", "payloadBase64", "payloadSha256", "schemaVersion"];
if (!attestation || typeof attestation !== "object" || Array.isArray(attestation) || !same(Object.keys(attestation).sort(compareUTF8), attestationFields)) fail("Runner execution attestation has unexpected fields");
if (attestation.schemaVersion !== "hive.visual-hive-runner-attestation.v1" || attestation.algorithm !== "HMAC-SHA256") fail("Runner execution attestation protocol is unsupported");
const authorityKey = decodeCanonicalBase64(attestation.keyBase64, "runner execution authority key");
const authenticatedPayloadBytes = decodeCanonicalBase64(attestation.payloadBase64, "runner execution payload");
if (authorityKey.length !== 32 || !hex64.test(String(attestation.keyCommitmentSha256 || "")) || sha256(authorityKey) !== attestation.keyCommitmentSha256) fail("Runner execution authority key does not match its commitment");
if (!hex64.test(String(attestation.payloadSha256 || "")) || sha256(authenticatedPayloadBytes) !== attestation.payloadSha256) fail("Runner execution payload does not match its digest");
if (!hex64.test(String(attestation.macSha256 || ""))) fail("Runner execution attestation MAC is malformed");
const expectedMac = crypto.createHmac("sha256", authorityKey).update(authenticatedPayloadBytes).digest();
const providedMac = Buffer.from(attestation.macSha256, "hex");
if (providedMac.length !== expectedMac.length || !crypto.timingSafeEqual(providedMac, expectedMac)) fail("Runner execution attestation HMAC verification failed");
const authenticatedPayload = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(authenticatedPayloadBytes));
const payloadFields = ["baseRef", "executionBinding", "generatedConfig", "generatedSpec", "headRef", "headSha", "pipelineExit", "producerCommit", "pullRequest", "report", "repository", "runAttempt", "runId", "runnerOutcome", "runtime", "schemaVersion", "workflow"];
if (!authenticatedPayload || typeof authenticatedPayload !== "object" || Array.isArray(authenticatedPayload) || !same(Object.keys(authenticatedPayload).sort(compareUTF8), payloadFields) ||
    !authenticatedPayloadBytes.equals(Buffer.from(JSON.stringify(authenticatedPayload), "utf8"))) fail("Runner execution payload is not the exact canonical object");
if (authenticatedPayload.schemaVersion !== "hive.visual-hive-runner-execution.v1" || authenticatedPayload.repository !== repository ||
    authenticatedPayload.pullRequest !== Number(pullRequestNumber) || authenticatedPayload.baseRef !== baseRef || authenticatedPayload.headRef !== headRef ||
    authenticatedPayload.headSha !== headSHA || authenticatedPayload.runId !== workflowRunID || authenticatedPayload.runAttempt !== workflowRunAttempt ||
    authenticatedPayload.workflow !== workflowName || authenticatedPayload.producerCommit !== producerCommit || !same(executionBinding(authenticatedPayload.executionBinding, "authenticated execution binding"), runtimeBinding)) fail("Runner execution payload is not bound to this exact PR workflow execution");
if (!same(exactIdentity(authenticatedPayload.report, "authenticated report identity"), publicIdentity(reportDocument.identity)) ||
    !same(exactIdentity(authenticatedPayload.runtime, "authenticated runtime identity"), publicIdentity(runtimeDocument.identity)) ||
    !same(exactIdentity(authenticatedPayload.generatedSpec, "authenticated generated spec identity"), generatedSpecIdentity) ||
    !same(exactIdentity(authenticatedPayload.generatedConfig, "authenticated generated config identity"), generatedConfigIdentity)) fail("Runner execution payload does not authenticate the exact execution artifacts");
const pipelineExitIdentity = indexedFile(".visual-hive/pipeline-exit-code.txt", "runner pipeline exit receipt", artifactIndex);
const runnerOutcomeDocument = readJSON(".visual-hive/hive-runner-outcome.json", "runner outcome receipt");
const runnerOutcomeIdentity = indexedFile(".visual-hive/hive-runner-outcome.json", "runner outcome receipt", artifactIndex);
if (!same(exactIdentity(authenticatedPayload.pipelineExit, "authenticated pipeline exit identity"), pipelineExitIdentity) ||
    !same(exactIdentity(authenticatedPayload.runnerOutcome, "authenticated runner outcome identity"), runnerOutcomeIdentity)) fail("Runner execution payload does not authenticate runner-owned outcome receipts");
if (fs.readFileSync(path.join(root, ".visual-hive/pipeline-exit-code.txt"), "utf8") !== "0\n") fail("Authenticated Visual Hive execution did not exit successfully");
if (!runnerOutcomeDocument.value || !same(Object.keys(runnerOutcomeDocument.value).sort(compareUTF8), ["conclusion", "outcome", "schemaVersion"]) ||
    runnerOutcomeDocument.value.schemaVersion !== "hive.visual-runner-outcome.v1" || runnerOutcomeDocument.value.outcome !== "success" || runnerOutcomeDocument.value.conclusion !== "success") fail("Authenticated Visual Hive runner outcome is not successful");
const stableRuntime = {
  browser: { name: runtime.browser.name, version: runtime.browser.version },
  environment: {
    os: environment.os,
    architecture: environment.architecture,
    nodeVersion: environment.nodeVersion,
    playwrightVersion: environment.playwrightVersion,
    locale: environment.locale,
    timezone: environment.timezone,
    userAgent: environment.userAgent,
    deviceScaleFactor: environment.deviceScaleFactor,
    fonts: environment.fonts.map((font) => ({ name: font.name, available: font.available }))
  }
};
const stableRuntimeSHA256 = sha256(goJSON(stableRuntime));

const configIdentity = copySealed("visual-hive.config.yaml", ".visual-hive/proof/pr/inputs/visual-hive.config.yaml", "Visual Hive config");
const baselineBySource = new Map();
for (const result of report.results) {
  const assertions = Array.isArray(result.screenshotAssertions) ? result.screenshotAssertions : [];
  for (const assertion of assertions) {
    const rawBaselinePath = assertion && typeof assertion.baselinePath === "string" ? assertion.baselinePath : "";
    const baselinePath = rawBaselinePath.trim();
    if (!baselinePath || rawBaselinePath !== baselinePath || assertion.status !== "passed") fail("Passing screenshot assertion lacks an exact passing baseline identity");
    const normalized = normalizeRelative(baselinePath, "screenshot baseline");
    if (!normalized.toLowerCase().endsWith(".png")) fail("Screenshot baseline is not a PNG");
    if (baselineBySource.has(normalized)) fail("Screenshot baseline identity is duplicated");
    baselineBySource.set(normalized, null);
  }
}
if (baselineBySource.size === 0 || baselineBySource.size > 200) fail("Admissible PR source has an invalid baseline inventory size");
const baselineIdentities = [];
let baselineBytes = 0;
const pngSignature = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
for (const source of [...baselineBySource.keys()].sort(compareUTF8)) {
  const baseline = regularFile(source, "screenshot baseline");
  if (baseline.bytes <= pngSignature.length || baseline.bytes > 20 * 1024 * 1024 || !fs.readFileSync(baseline.absolute).subarray(0, pngSignature.length).equals(pngSignature)) fail("Screenshot baseline is not a bounded exact PNG");
  baselineBytes += baseline.bytes;
  if (baselineBytes > 500 * 1024 * 1024) fail("Screenshot baseline inventory exceeds its byte bound");
  baselineIdentities.push(copySealed(source, ".visual-hive/proof/pr/baselines/" + source, "screenshot baseline"));
}
baselineIdentities.sort((left, right) => compareUTF8(left.path, right.path));
const baselineRecords = baselineIdentities.map((identity) => identity.path + "\0" + identity.sha256 + "\0" + identity.bytes);
const binding = {
  schemaVersion: "hive.visual-hive-pr-source.v2",
  repository,
  repositoryId: baseRepositoryID,
  pullRequest: Number(pullRequestNumber),
  base: { repository: baseRepository, repositoryId: baseRepositoryID, ref: baseRef, sha: baseSHA },
  head: { repository: headRepository, repositoryId: headRepositoryID, ref: headRef, sha: headSHA },
  workflow: {
    name: workflowName,
    path: workflowPath,
    event: eventName,
    runId: workflowRunID,
    runAttempt: workflowRunAttempt,
    definition: workflowIdentity,
    producerCommit,
    sourceArtifactName,
    bundleArtifactName
  },
  plan: { path: planDocument.identity.path, sha256: planDocument.identity.sha256, bytes: planDocument.identity.bytes },
  report: { path: reportDocument.identity.path, sha256: reportDocument.identity.sha256, bytes: reportDocument.identity.bytes },
  config: configIdentity,
  changedFiles: { path: changed.identity.path, sha256: changed.identity.sha256, bytes: changed.identity.bytes },
  planContracts: { path: planContracts.identity.path, sha256: planContracts.identity.sha256, bytes: planContracts.identity.bytes },
  evaluationScopes: { path: evaluationScopes.identity.path, sha256: evaluationScopes.identity.sha256, bytes: evaluationScopes.identity.bytes },
  runnerAttestation: attestationIdentity,
  runtime: {
    receipt: { path: runtimeDocument.identity.path, sha256: runtimeDocument.identity.sha256, bytes: runtimeDocument.identity.bytes },
    generatedSpec: generatedSpecIdentity,
    generatedConfig: generatedConfigIdentity,
    stableSha256: stableRuntimeSHA256,
    executionBindingSha256: sha256(JSON.stringify(runtimeBinding))
  },
  baselines: { sha256: sha256(baselineRecords.join("\n")), files: baselineIdentities }
};
fs.writeFileSync(".visual-hive/proof/pr/source-binding.json", JSON.stringify(binding, null, 2) + "\n", { flag: "wx", mode: 0o600 });
writeAdmissibility(true);`
}

func prepareIsolatedTargetAccountShell() string {
	return fmt.Sprintf(`set -euo pipefail
for reserved_account in %s %s; do
  if id -u "$reserved_account" >/dev/null 2>&1; then
    echo "Reserved isolation account already exists: $reserved_account" >&2
    exit 1
  fi
done
sudo useradd --create-home --shell /bin/bash %s
sudo useradd --create-home --shell /usr/sbin/nologin %s
sudo install -d -o %s -g %s -m 0755 /home/%s/.local/bin /home/%s/.cache
sudo install -d -o %s -g %s -m 0700 /home/%s/.cache /home/%s/.cache/tmp
sudo chmod 0700 /home/%s
python_bin="$(command -v python)"
(cd / && sudo -u %s -- env -i HOME=/home/%s PATH="$PATH" "$python_bin" -I -m venv /home/%s/.venv)
target_root=%q
target_workspace=%q
trusted_root=%q
test ! -e "$target_root"
sudo install -d -o root -g root -m 0755 "$target_root" "$target_workspace" "$trusted_root"
test -d "$GITHUB_WORKSPACE"
test ! -L "$GITHUB_WORKSPACE"
test "$(readlink -f "$GITHUB_WORKSPACE")" = "$GITHUB_WORKSPACE"
sudo chown -R %s:%s "$GITHUB_WORKSPACE"
test -d .git
test ! -L .git
test -f visual-hive.config.yaml
test ! -L visual-hive.config.yaml
sudo chown -R root:root .git
sudo chmod -R a-w .git
sudo chown root:root visual-hive.config.yaml
sudo chmod 0444 visual-hive.config.yaml
sudo chmod 1777 "$GITHUB_WORKSPACE"
sudo mount --bind "$GITHUB_WORKSPACE" "$target_workspace"
mountpoint -q "$target_workspace"
test "$(stat -Lc '%%d:%%i' "$target_workspace")" = "$(stat -Lc '%%d:%%i' "$GITHUB_WORKSPACE")"
echo "HIVE_TARGET_WORKSPACE=$target_workspace" >> "$GITHUB_ENV"
`, isolatedTargetAccount, isolatedEvidenceAccount, isolatedTargetAccount, isolatedEvidenceAccount,
		isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount,
		isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount, isolatedEvidenceAccount,
		isolatedTargetAccount, isolatedTargetAccount, isolatedTargetAccount,
		isolatedTargetRoot, isolatedTargetWorkspace, isolatedTrustedRoot,
		isolatedTargetAccount, isolatedTargetAccount)
}

func trustedRepositoryTestPrerequisitesShell() string {
	return `set -euo pipefail
sudo apt-get update -qq
sudo apt-get install -y -qq --no-install-recommends ripgrep
rg_path="$(readlink -f /usr/bin/rg)"
test "$rg_path" = "/usr/bin/rg"
test -x "$rg_path"
test "$(dpkg-query -S "$rg_path")" = "ripgrep: /usr/bin/rg"
test "$(stat -Lc '%u:%g' "$rg_path")" = "0:0"
if sudo -u ` + isolatedTargetAccount + ` -- test -w "$rg_path"; then
  echo "System ripgrep executable is writable by the isolated target account" >&2
  exit 1
fi
rg_version="$($rg_path --version | sed -n '1p')"
case "$rg_version" in
  'ripgrep '[0-9]*) ;;
  *) echo "System ripgrep reported an invalid version: $rg_version" >&2; exit 1 ;;
esac
target_rg="$(` + isolatedTargetEnvPrefix() + ` bash --noprofile --norc -c 'command -v rg')"
test "$(readlink -f "$target_rg")" = "$rg_path"
probe_output="$(` + isolatedTargetEnvPrefix() + ` bash --noprofile --norc -c '
set -euo pipefail
probe_root="$HOME/.cache/tmp/hive-rg-package-manager-probe"
rm -rf -- "$probe_root"
mkdir -p "$probe_root"
printf "%s\n" "{\"scripts\":{\"hive-rg-probe\":\"command -v rg && rg --version\"}}" > "$probe_root/package.json"
npm --prefix "$probe_root" run --silent hive-rg-probe
rm -rf -- "$probe_root"
')"
test "$(printf '%s\n' "$probe_output" | sed -n '1p')" = "$rg_path"
if ! printf '%s\n' "$probe_output" | grep -Fqx "$rg_version"; then
  echo "Ordinary target npm execution did not report the provisioned system ripgrep version" >&2
  exit 1
fi
`
}

// prepareIsolatedVisualEvidenceRuntimeShell installs the runner-owned
// cross-principal launcher. Visual Hive and Playwright run as hive-evidence,
// while every shell:true lifecycle command is delegated down to hive-target in
// a private mount namespace. The target never receives the PR attestation key.
func prepareIsolatedVisualEvidenceRuntimeShell(pullRequest bool) string {
	authoritySetup := ""
	if pullRequest {
		authoritySetup = `
execution_key="$RUNNER_TEMP/hive-visual-execution-authority-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.key"
test ! -e "$execution_key"
sudo install -o root -g root -m 0400 /dev/null "$execution_key"
sudo dd if=/dev/urandom of="$execution_key" bs=32 count=1 status=none
test "$(sudo stat -c '%u:%g:%a:%s' "$execution_key")" = "0:0:400:32"
if sudo -u ` + isolatedTargetAccount + ` -- test -r "$execution_key" || sudo -u ` + isolatedEvidenceAccount + ` -- test -r "$execution_key"; then
  echo "Execution authority key is readable outside the runner root principal" >&2
  exit 1
fi
execution_key_commitment="$(sudo sha256sum "$execution_key" | cut -d ' ' -f 1)"
echo "HIVE_EXECUTION_AUTHORITY_KEY=$execution_key" >> "$GITHUB_ENV"
echo "HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT=$execution_key_commitment" >> "$GITHUB_ENV"
`
	}
	return `set -euo pipefail
target_bash=` + isolatedTargetBash + `
target_shell=` + isolatedTargetShell + `
evidence_launcher=` + isolatedEvidenceLauncher + `
sudo install -o root -g root -m 0555 "$(readlink -f /bin/bash)" "$target_bash"
sudo install -d -o root -g root -m 0555 ` + isolatedRuntimeRoot + ` ` + isolatedRuntimeModules + `
test "$(stat -c '%u:%g:%a' ` + isolatedRuntimeModules + `)" = "0:0:555"
target_shell_source="$RUNNER_TEMP/hive-visual-target-shell-${GITHUB_RUN_ID}"
evidence_launcher_source="$RUNNER_TEMP/hive-visual-evidence-launcher-${GITHUB_RUN_ID}"
rm -f -- "$target_shell_source" "$evidence_launcher_source"
umask 077
cat > "$target_shell_source" <<'HIVE_TARGET_SHELL'
#!` + isolatedTargetBash + `
set -euo pipefail
if [ "$#" -lt 2 ] || [ "$1" != "-c" ]; then
  echo "Visual Hive target shell only accepts one -c lifecycle command" >&2
  exit 126
fi
target_command="$2"
shift 2
test -n "${HIVE_TARGET_WORKSPACE:-}"
test -n "${PLAYWRIGHT_BROWSERS_PATH:-}"
caller_uid="$(id -u)"
target_uid="$(id -u ` + isolatedTargetAccount + `)"
evidence_uid="$(id -u ` + isolatedEvidenceAccount + `)"
if [ "$caller_uid" = "$target_uid" ]; then
  exec ` + isolatedTargetBash + ` --noprofile --norc -c "$target_command" hive-visual-target-child-shell "$@"
fi
if [ "$caller_uid" != "$evidence_uid" ]; then
  echo "Visual Hive target shell was invoked by an unexpected principal" >&2
  exit 126
fi
target_environment=(
  "HOME=/home/` + isolatedTargetAccount + `"
  "PATH=/home/` + isolatedTargetAccount + `/.local/bin:/home/` + isolatedTargetAccount + `/.venv/bin:/usr/local/bin:/usr/bin:/bin"
  "LANG=C.UTF-8"
  "CI=true"
  "PLAYWRIGHT_BROWSERS_PATH=$PLAYWRIGHT_BROWSERS_PATH"
  "HIVE_TARGET_WORKSPACE=$HIVE_TARGET_WORKSPACE"
  "HIVE_TARGET_PROCESS=1"
)
for name in GITHUB_ACTIONS GITHUB_REPOSITORY GITHUB_HEAD_REF GITHUB_BASE_REF GITHUB_SHA GITHUB_EVENT_NAME GITHUB_REF GITHUB_REF_NAME GITHUB_RUN_ID GITHUB_RUN_ATTEMPT GITHUB_WORKFLOW; do
  if [[ -v "$name" ]]; then
    target_environment+=("$name=${!name}")
  fi
done
exec /usr/bin/sudo -n -u ` + isolatedTargetAccount + ` -- /usr/bin/env -i -C "$HIVE_TARGET_WORKSPACE" "${target_environment[@]}" ` + isolatedTargetBash + ` --noprofile --norc -c "$target_command" hive-visual-target-shell "$@"
HIVE_TARGET_SHELL
cat > "$evidence_launcher_source" <<'HIVE_EVIDENCE_LAUNCHER'
#!` + isolatedTargetBash + `
set -euo pipefail
umask 077
test "$(id -u)" = "0"
test "$#" -ge 1
for name in HIVE_TARGET_WORKSPACE HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH; do
  test -n "${!name:-}"
done
evidence_root="$HIVE_TARGET_WORKSPACE/.visual-hive"
runtime_modules=` + isolatedRuntimeModules + `
test "$(stat -c '%u:%g:%a' "$evidence_root")" = "$(id -u ` + isolatedEvidenceAccount + `):$(id -g ` + isolatedEvidenceAccount + `):700"
test "$(stat -c '%u:%g:%a' "$runtime_modules")" = "0:0:555"
case "$runtime_modules" in
  "$evidence_root"|"$evidence_root"/*) echo "Visual Hive runtime modules entered the evidence root" >&2; exit 1 ;;
esac
if find "$runtime_modules" -mindepth 1 -print -quit | grep -q .; then
  echo "Protected runtime module mountpoint is not empty" >&2
  exit 1
fi
mount --make-rprivate /
mount --bind ` + isolatedTrustedTooling + `/node_modules "$runtime_modules"
mount -o remount,bind,ro "$runtime_modules"
system_shell="$(readlink -f /bin/sh)"
case "$system_shell" in /usr/bin/*|/bin/*) ;; *) echo "System shell resolved outside an OS-owned path" >&2; exit 1 ;; esac
mount --bind ` + isolatedTargetShell + ` "$system_shell"
mount -o remount,bind,ro "$system_shell"
evidence_environment=(
  "HOME=/home/` + isolatedEvidenceAccount + `"
  "TMPDIR=/home/` + isolatedEvidenceAccount + `/.cache/tmp"
  "PATH=$runtime_modules/.bin:/usr/local/bin:/usr/bin:/bin"
  "NODE_PATH=$runtime_modules"
  "LANG=C.UTF-8"
  "CI=true"
  "PLAYWRIGHT_BROWSERS_PATH=$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"
  "HIVE_TARGET_WORKSPACE=$HIVE_TARGET_WORKSPACE"
)
for name in GITHUB_ACTIONS GITHUB_REPOSITORY GITHUB_HEAD_REF GITHUB_BASE_REF GITHUB_SHA GITHUB_EVENT_NAME GITHUB_REF GITHUB_REF_NAME GITHUB_RUN_ID GITHUB_RUN_ATTEMPT GITHUB_WORKFLOW; do
  if [[ -v "$name" ]]; then
    evidence_environment+=("$name=${!name}")
  fi
done
exec /usr/bin/sudo -n -u ` + isolatedEvidenceAccount + ` -- /usr/bin/env -i -C "$HIVE_TARGET_WORKSPACE" "${evidence_environment[@]}" ` + isolatedTargetBash + ` --noprofile --norc -c 'umask 077; exec "$@"' hive-visual-evidence "$@"
HIVE_EVIDENCE_LAUNCHER
sudo install -o root -g root -m 0555 "$target_shell_source" "$target_shell"
sudo install -o root -g root -m 0555 "$evidence_launcher_source" "$evidence_launcher"
rm -f -- "$target_shell_source" "$evidence_launcher_source"
sudoers_source="$RUNNER_TEMP/hive-visual-target-shell-sudoers-${GITHUB_RUN_ID}"
rm -f -- "$sudoers_source"
cat > "$sudoers_source" <<'HIVE_TARGET_SUDOERS'
` + isolatedEvidenceAccount + ` ALL=(` + isolatedTargetAccount + `) NOPASSWD: /usr/bin/env
Defaults:` + isolatedEvidenceAccount + ` !requiretty
HIVE_TARGET_SUDOERS
chmod 0440 "$sudoers_source"
sudo visudo -cf "$sudoers_source"
sudo install -o root -g root -m 0440 "$sudoers_source" /etc/sudoers.d/hive-visual-target-shell
rm -f -- "$sudoers_source"
sudo -u ` + isolatedEvidenceAccount + ` -- test ! -w "$target_shell"
sudo -u ` + isolatedTargetAccount + ` -- test ! -w "$target_shell"
sudo -u ` + isolatedEvidenceAccount + ` -- test ! -w "$evidence_launcher"
` + authoritySetup
}

func prepareIsolatedVisualEvidenceRootShell() string {
	return `set -euo pipefail
test -n "${HIVE_TARGET_WORKSPACE:-}"
evidence_root="$HIVE_TARGET_WORKSPACE/.visual-hive"
sudo git -c safe.directory="$HIVE_TARGET_WORKSPACE" -C "$HIVE_TARGET_WORKSPACE" clean -ffdx -- .visual-hive
if [ -L "$evidence_root" ]; then
  echo "Tracked Visual Hive evidence root is a symbolic link" >&2
  exit 1
fi
if [ -e "$evidence_root" ] && [ ! -d "$evidence_root" ]; then
  echo "Tracked Visual Hive evidence root is not a directory" >&2
  exit 1
fi
if [ -d "$evidence_root" ] && sudo find "$evidence_root" -type l -print -quit | grep -q .; then
  echo "Tracked Visual Hive evidence contains a symbolic link" >&2
  exit 1
fi
if [ -d "$evidence_root" ] && sudo find "$evidence_root" ! -type d ! -type f -print -quit | grep -q .; then
  echo "Tracked Visual Hive evidence contains a non-regular entry" >&2
  exit 1
fi
if [ -e "$evidence_root/node_modules" ] || [ -L "$evidence_root/node_modules" ]; then
  echo "Tracked Visual Hive evidence contains a dependency tree" >&2
  exit 1
fi
sudo install -d -o ` + isolatedEvidenceAccount + ` -g ` + isolatedEvidenceAccount + ` -m 0700 "$evidence_root"
sudo chown -R ` + isolatedEvidenceAccount + `:` + isolatedEvidenceAccount + ` "$evidence_root"
sudo find "$evidence_root" -type d -exec chmod 0700 {} +
sudo find "$evidence_root" -type f -exec chmod 0600 {} +
test "$(stat -c '%u:%g:%a' "$evidence_root")" = "$(id -u ` + isolatedEvidenceAccount + `):$(id -g ` + isolatedEvidenceAccount + `):700"
if sudo -u ` + isolatedTargetAccount + ` -- test -r "$evidence_root" || sudo -u ` + isolatedTargetAccount + ` -- test -w "$evidence_root" || sudo -u ` + isolatedTargetAccount + ` -- test -x "$evidence_root"; then
  echo "Target principal can access the protected evidence root" >&2
  exit 1
fi
sudo -u ` + isolatedEvidenceAccount + ` -- test -w "$evidence_root" -a -x "$evidence_root"
`
}

func validateIsolatedVisualEvidenceBeforeSealShell() string {
	return `evidence_root="$(readlink -f .visual-hive)"
workspace_root="$(readlink -f "$GITHUB_WORKSPACE")"
if [ "$evidence_root" != "$workspace_root/.visual-hive" ]; then
  echo "Raw evidence escaped the runner workspace" >&2
  exit 1
fi
test "$(stat -c '%u:%g:%a' "$evidence_root")" = "$(id -u ` + isolatedEvidenceAccount + `):$(id -g ` + isolatedEvidenceAccount + `):700"
if sudo -u ` + isolatedTargetAccount + ` -- test -r "$evidence_root" || sudo -u ` + isolatedTargetAccount + ` -- test -w "$evidence_root" || sudo -u ` + isolatedTargetAccount + ` -- test -x "$evidence_root"; then
  echo "Target principal retained access to protected evidence" >&2
  exit 1
fi
python_bin="$(readlink -f "$(command -v python)")"
sudo env HIVE_EVIDENCE_ROOT="$evidence_root" HIVE_EXPECTED_UID="$(id -u ` + isolatedEvidenceAccount + `)" HIVE_EXPECTED_GID="$(id -g ` + isolatedEvidenceAccount + `)" "$python_bin" -I - <<'PY'
import os
import stat

root = os.path.realpath(os.environ["HIVE_EVIDENCE_ROOT"])
expected_uid = int(os.environ["HIVE_EXPECTED_UID"])
expected_gid = int(os.environ["HIVE_EXPECTED_GID"])
root_stat = os.lstat(root)
if not stat.S_ISDIR(root_stat.st_mode) or root_stat.st_uid != expected_uid or root_stat.st_gid != expected_gid or stat.S_IMODE(root_stat.st_mode) != 0o700:
    raise SystemExit("protected evidence root lacks the exact evidence owner and mode")
device = root_stat.st_dev
files = 0
total = 0
pending = [root]
while pending:
    current = pending.pop()
    with os.scandir(current) as entries:
        for entry in entries:
            item = entry.stat(follow_symlinks=False)
            if item.st_dev != device:
                raise SystemExit("protected evidence contains a nested mount")
            if item.st_uid != expected_uid or item.st_gid != expected_gid:
                raise SystemExit("protected evidence contains an entry from another principal")
            if item.st_mode & 0o7077:
                raise SystemExit("protected evidence contains group, other, or special permission bits")
            if stat.S_ISLNK(item.st_mode):
                raise SystemExit("protected evidence contains a symbolic link")
            if stat.S_ISDIR(item.st_mode):
                pending.append(entry.path)
                continue
            if not stat.S_ISREG(item.st_mode):
                raise SystemExit("protected evidence contains a non-regular file")
            if item.st_nlink != 1:
                raise SystemExit("protected evidence contains a hard-linked file")
            files += 1
            total += item.st_size
            if files > 5000 or total > 1073741824:
                raise SystemExit("protected evidence exceeds its bounded upload limits")
PY`
}

func runnerExecutionAttestationShell() string {
	return `attestation_temp="$RUNNER_TEMP/hive-visual-runner-attestation-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.json"
rm -f -- "$attestation_temp"
test -n "${HIVE_EXECUTION_AUTHORITY_KEY:-}"
test -n "${HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT:-}"
test "$(sudo stat -c '%u:%g:%a:%s' "$HIVE_EXECUTION_AUTHORITY_KEY")" = "0:0:400:32"
if sudo -u ` + isolatedTargetAccount + ` -- test -r "$HIVE_EXECUTION_AUTHORITY_KEY" || sudo -u ` + isolatedEvidenceAccount + ` -- test -r "$HIVE_EXECUTION_AUTHORITY_KEY"; then
  echo "Execution authority key became readable before evidence authentication" >&2
  exit 1
fi
sudo -- env -i -C "$GITHUB_WORKSPACE" \
  HIVE_EXECUTION_AUTHORITY_KEY="$HIVE_EXECUTION_AUTHORITY_KEY" \
  HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT="$HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT" \
  HIVE_RUNNER_ATTESTATION_OUTPUT="$attestation_temp" \
  HIVE_TARGET_REPOSITORY="$HIVE_TARGET_REPOSITORY" \
  HIVE_TARGET_PULL_REQUEST="$HIVE_TARGET_PULL_REQUEST" \
  HIVE_TARGET_BASE_REF="$HIVE_TARGET_BASE_REF" \
  HIVE_TARGET_HEAD_REF="$HIVE_TARGET_HEAD_REF" \
  HIVE_TARGET_HEAD_SHA="$HIVE_TARGET_HEAD_SHA" \
  HIVE_TARGET_RUN_ID="$HIVE_TARGET_RUN_ID" \
  HIVE_TARGET_RUN_ATTEMPT="$HIVE_TARGET_RUN_ATTEMPT" \
  HIVE_TARGET_WORKFLOW="$HIVE_TARGET_WORKFLOW" \
  HIVE_PRODUCER_COMMIT="` + visualHivePullRequestProducerCommit + `" \
  "$HIVE_TRUSTED_NODE" - <<'NODE'
` + runnerExecutionAttestationScript() + `
NODE
if [ -e "$attestation_temp" ]; then
  test -f "$attestation_temp"
  test ! -L "$attestation_temp"
  test "$(sudo stat -c '%u:%g:%a' "$attestation_temp")" = "0:0:600"
  test ! -e ` + runnerAttestationPath + `
  sudo install -D -o root -g root -m 0444 "$attestation_temp" ` + runnerAttestationPath + `
fi
sudo rm -f -- "$attestation_temp" "$HIVE_EXECUTION_AUTHORITY_KEY"
`
}

func runnerExecutionAttestationScript() string {
	return `const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

const fail = (message) => { throw new Error(message); };
const root = fs.realpathSync(process.cwd());
const hex40 = /^[a-f0-9]{40}$/u;
const hex64 = /^[a-f0-9]{64}$/u;
const positiveInteger = /^[1-9][0-9]*$/u;
const exactKeys = (value, fields, label) => {
  if (!value || typeof value !== "object" || Array.isArray(value) || JSON.stringify(Object.keys(value).sort()) !== JSON.stringify([...fields].sort())) fail(label + " has unexpected fields");
};
const requiredEnvironment = (name) => {
  const value = String(process.env[name] || "").trim();
  if (!value || value.includes("\0")) fail("Missing or invalid " + name);
  return value;
};
const sha256 = (value) => crypto.createHash("sha256").update(value).digest("hex");
const normalizeRelative = (value, label) => {
  const normalized = String(value || "");
  if (!normalized || normalized.includes("\\") || normalized.startsWith("/") || /^[A-Za-z]:\//u.test(normalized) || /[\u0000-\u001f\u007f]/u.test(normalized) || normalized.split("/").some((part) => !part || part === "." || part === ".." || part.trim() !== part)) fail(label + " is not a safe repository-relative path");
  return normalized;
};
const ordinaryFile = (relative, label) => {
  const normalized = normalizeRelative(relative, label);
  let current = root;
  for (const part of normalized.split("/")) {
    current = path.join(current, part);
    if (!fs.existsSync(current)) fail(label + " is missing");
    if (fs.lstatSync(current).isSymbolicLink()) fail(label + " traverses a symbolic link");
  }
  const stat = fs.lstatSync(current);
  if (!stat.isFile() || stat.nlink !== 1) fail(label + " is not one ordinary file");
  const escaped = path.relative(root, fs.realpathSync(current));
  if (!escaped || escaped.startsWith(".." + path.sep) || path.isAbsolute(escaped)) fail(label + " escaped the repository root");
  return { path: normalized, sha256: sha256(fs.readFileSync(current)), bytes: stat.size };
};
const readJSON = (relative, label) => {
  const identity = ordinaryFile(relative, label);
  return { identity, value: JSON.parse(fs.readFileSync(path.join(root, ...identity.path.split("/")), "utf8")) };
};
const same = (left, right) => JSON.stringify(left) === JSON.stringify(right);
const bindingFields = ["bindingMacSha256", "generatedConfigSha256", "generatedSpecSha256", "nonceSha256", "payloadSha256"];
const executionBinding = (value, label) => {
  exactKeys(value, bindingFields, label);
  const result = {};
  for (const field of bindingFields) {
    const digest = String(value[field] || "");
    if (!hex64.test(digest)) fail(label + " has an invalid " + field);
    result[field] = digest;
  }
  return result;
};

const reportDocument = readJSON(".visual-hive/report.json", "Visual Hive report");
const report = reportDocument.value;
if (report.executionBinding === undefined) process.exit(0);
const runtimeDocument = readJSON(".visual-hive/proof/pr/runtime.json", "Visual Hive runtime sidecar");
const reportBinding = executionBinding(report.executionBinding, "report execution binding");
const runtimeBinding = executionBinding(runtimeDocument.value.executionBinding, "runtime execution binding");
if (!same(reportBinding, runtimeBinding)) fail("Report and runtime execution bindings differ before runner authentication");
const generatedSpecPath = normalizeRelative(report.generatedSpecPath, "generated Playwright spec");
if (!generatedSpecPath.startsWith(".visual-hive/generated/")) fail("Generated Playwright spec escaped its canonical evidence directory");
const generatedConfigPath = path.posix.join(path.posix.dirname(generatedSpecPath), "visual-hive.generated.config.cjs");
const generatedSpec = ordinaryFile(generatedSpecPath, "generated Playwright spec");
const generatedConfig = ordinaryFile(generatedConfigPath, "generated Playwright config");
if (generatedSpec.sha256 !== reportBinding.generatedSpecSha256 || generatedConfig.sha256 !== reportBinding.generatedConfigSha256) fail("Generated execution bytes differ from the producer execution binding");
const pipelineExit = ordinaryFile(".visual-hive/pipeline-exit-code.txt", "runner pipeline exit receipt");
const pipelineExitText = fs.readFileSync(path.join(root, ...pipelineExit.path.split("/")), "utf8");
if (!/^(?:0|[1-9][0-9]{0,2})\n$/u.test(pipelineExitText) || Number(pipelineExitText.trim()) > 255) fail("Runner pipeline exit receipt is malformed");
const runnerOutcomeDocument = readJSON(".visual-hive/hive-runner-outcome.json", "runner outcome receipt");
exactKeys(runnerOutcomeDocument.value, ["schemaVersion", "outcome", "conclusion"], "runner outcome receipt");
if (runnerOutcomeDocument.value.schemaVersion !== "hive.visual-runner-outcome.v1" || !["success", "failure"].includes(runnerOutcomeDocument.value.outcome) || !["success", "failure"].includes(runnerOutcomeDocument.value.conclusion)) fail("Runner outcome receipt is malformed");

const repository = requiredEnvironment("HIVE_TARGET_REPOSITORY");
const pullRequest = requiredEnvironment("HIVE_TARGET_PULL_REQUEST");
const baseRef = requiredEnvironment("HIVE_TARGET_BASE_REF");
const headRef = requiredEnvironment("HIVE_TARGET_HEAD_REF");
const headSha = requiredEnvironment("HIVE_TARGET_HEAD_SHA").toLowerCase();
const runId = requiredEnvironment("HIVE_TARGET_RUN_ID");
const runAttempt = requiredEnvironment("HIVE_TARGET_RUN_ATTEMPT");
const workflow = requiredEnvironment("HIVE_TARGET_WORKFLOW");
const producerCommit = requiredEnvironment("HIVE_PRODUCER_COMMIT").toLowerCase();
if (![pullRequest, runId, runAttempt].every((value) => positiveInteger.test(value)) || !hex40.test(headSha) || !hex40.test(producerCommit)) fail("Runner execution identity is malformed");
const payload = {
  schemaVersion: "hive.visual-hive-runner-execution.v1",
  repository,
  pullRequest: Number(pullRequest),
  baseRef,
  headRef,
  headSha,
  runId,
  runAttempt,
  workflow,
  producerCommit,
  executionBinding: reportBinding,
  report: reportDocument.identity,
  runtime: runtimeDocument.identity,
  generatedSpec,
  generatedConfig,
  pipelineExit,
  runnerOutcome: runnerOutcomeDocument.identity
};
const payloadBytes = Buffer.from(JSON.stringify(payload), "utf8");
const keyPath = requiredEnvironment("HIVE_EXECUTION_AUTHORITY_KEY");
const key = fs.readFileSync(keyPath);
if (key.length !== 32) fail("Execution authority key is not exactly 32 bytes");
const keyCommitmentSha256 = requiredEnvironment("HIVE_EXECUTION_AUTHORITY_KEY_COMMITMENT").toLowerCase();
if (!hex64.test(keyCommitmentSha256) || sha256(key) !== keyCommitmentSha256) fail("Execution authority key does not match its pre-execution commitment");
const attestation = {
  schemaVersion: "hive.visual-hive-runner-attestation.v1",
  algorithm: "HMAC-SHA256",
  keyCommitmentSha256,
  keyBase64: key.toString("base64"),
  payloadSha256: sha256(payloadBytes),
  payloadBase64: payloadBytes.toString("base64"),
  macSha256: crypto.createHmac("sha256", key).update(payloadBytes).digest("hex")
};
const output = requiredEnvironment("HIVE_RUNNER_ATTESTATION_OUTPUT");
fs.writeFileSync(output, JSON.stringify(attestation, null, 2) + "\n", { flag: "wx", mode: 0o600 });`
}

func sealIsolatedVisualEvidenceShell() string {
	return dedentWorkflowShell(`          evidence_root="$(readlink -f .visual-hive)"
          workspace_root="$(readlink -f "$GITHUB_WORKSPACE")"
          if [ "$evidence_root" != "$workspace_root/.visual-hive" ]; then
            echo "Raw target evidence escaped the runner workspace" >&2
            exit 1
          fi
          test -d "$evidence_root"
          test ! -L "$evidence_root"
          sudo chown root:root "$evidence_root"
          sudo chmod 0555 "$evidence_root"
          python_bin="$(readlink -f "$(command -v python)")"
          test -x "$python_bin"
          sudo env HIVE_EVIDENCE_ROOT="$evidence_root" HIVE_WORKSPACE_ROOT="$workspace_root" "$python_bin" -I - <<'PY'
          import os
          import stat

          root = os.path.realpath(os.environ["HIVE_EVIDENCE_ROOT"])
          workspace = os.path.realpath(os.environ["HIVE_WORKSPACE_ROOT"])
          if root != os.path.join(workspace, ".visual-hive"):
              raise SystemExit("raw evidence root is not the exact workspace child")
          root_stat = os.lstat(root)
          if not stat.S_ISDIR(root_stat.st_mode):
              raise SystemExit("raw evidence root is not a directory")
          device = root_stat.st_dev
          files = 0
          total = 0
          pending = [root]
          while pending:
              current = pending.pop()
              with os.scandir(current) as entries:
                  for entry in entries:
                      item = entry.stat(follow_symlinks=False)
                      if item.st_dev != device:
                          raise SystemExit("raw evidence contains a nested mount")
                      if stat.S_ISLNK(item.st_mode):
                          raise SystemExit("raw evidence contains a symbolic link")
                      if stat.S_ISDIR(item.st_mode):
                          pending.append(entry.path)
                          continue
                      if not stat.S_ISREG(item.st_mode):
                          raise SystemExit("raw evidence contains a non-regular file")
                      if item.st_nlink != 1:
                          raise SystemExit("raw evidence contains a hard-linked file")
                      files += 1
                      total += item.st_size
                      if files > 5000 or total > 1073741824:
                          raise SystemExit("raw evidence exceeds its bounded upload limits")
          PY
          sudo find "$evidence_root" -xdev -type f -exec chown root:root -- {} +
          sudo find "$evidence_root" -xdev -type f -exec chmod 0444 -- {} +
          sudo find "$evidence_root" -xdev -depth -type d -exec chown root:root -- {} +
          sudo find "$evidence_root" -xdev -depth -type d -exec chmod 0555 -- {} +
          env HIVE_EVIDENCE_ROOT="$evidence_root" "$python_bin" -I - <<'PY'
          import os
          import stat

          root = os.path.realpath(os.environ["HIVE_EVIDENCE_ROOT"])
          pending = [root]
          while pending:
              current = pending.pop()
              current_stat = os.lstat(current)
              if current_stat.st_uid != 0 or current_stat.st_gid != 0 or current_stat.st_mode & 0o222:
                  raise SystemExit("sealed evidence directory is not root-owned and read-only")
              with os.scandir(current) as entries:
                  for entry in entries:
                      item = entry.stat(follow_symlinks=False)
                      if item.st_uid != 0 or item.st_gid != 0 or item.st_mode & 0o222:
                          raise SystemExit("sealed evidence entry is not root-owned and read-only")
                      if stat.S_ISDIR(item.st_mode):
                          pending.append(entry.path)
                      elif stat.S_ISREG(item.st_mode):
                          with open(entry.path, "rb") as handle:
                              while handle.read(1024 * 1024):
                                  pass
                      else:
                          raise SystemExit("sealed evidence contains an invalid entry")
          PY`)
}

func verifyAndSealTargetCheckoutShell() string {
	return fmt.Sprintf(`set -euo pipefail
sudo pkill -KILL -u %s 2>/dev/null || true
test "$(git rev-parse HEAD)" = "$HIVE_TARGET_HEAD_SHA"
sudo git -c safe.directory="$GITHUB_WORKSPACE" diff --no-ext-diff --no-textconv --exit-code -- .
while IFS= read -r -d '' tracked; do
  if [ -L "$tracked" ]; then
    sudo chown -h root:root -- "$tracked"
  else
    sudo chown root:root -- "$tracked"
    sudo chmod a-w -- "$tracked"
  fi
  directory="$(dirname -- "$tracked")"
  while :; do
    sudo chown root:root -- "$directory"
    sudo chmod 1777 -- "$directory"
    if [ "$directory" = "." ]; then
      break
    fi
    directory="$(dirname -- "$directory")"
  done
done < <(git ls-files -z)
`, isolatedTargetAccount)
}

func verifyImmutableTargetCheckoutShell() string {
	return fmt.Sprintf(`set -euo pipefail
target_workspace="${HIVE_TARGET_WORKSPACE:-}"
cleanup_target_workspace() {
  if [ -n "$target_workspace" ] && mountpoint -q "$target_workspace"; then
    sudo umount "$target_workspace" || true
  fi
}
trap cleanup_target_workspace EXIT
test -n "$target_workspace"
sudo pkill -KILL -u %s 2>/dev/null || true
test "$(git rev-parse HEAD)" = "$HIVE_TARGET_HEAD_SHA"
git diff --no-ext-diff --no-textconv --exit-code -- .
sudo umount "$target_workspace"
trap - EXIT
if mountpoint -q "$target_workspace"; then
  echo "Isolated target workspace remained mounted" >&2
  exit 1
fi
`, isolatedTargetAccount)
}

func isolatedTargetDependencyShell(commands [][]string, includeTrustedBrowser bool) string {
	packageScopes, err := selectedPackageDependencyScopes(commands)
	if err != nil {
		return fmt.Sprintf("set -euo pipefail\necho %s >&2\nexit 1\n", shellQuote("Hive rejected selected package dependency scope: "+err.Error()))
	}
	targetInstall := strings.ReplaceAll(dedentWorkflowShell(targetPackageInstallShell()), "corepack enable", "corepack enable --install-directory /home/hive-target/.local/bin")
	pythonInstall := selectedPythonDependencyInstallShell(packageScopes)
	trustedBrowserBefore := ""
	trustedBrowserAfter := ""
	targetEnvironment := isolatedTargetEnvPrefix()
	if includeTrustedBrowser {
		trustedBrowserBefore = `tooling_playwright="` + isolatedTrustedTooling + `/node_modules/@playwright/test/cli.js"
trusted_browser_path="` + isolatedTrustedRoot + `/playwright-${GITHUB_RUN_ID}"
target_browser_staging="` + isolatedTargetRoot + `/playwright-staging-${GITHUB_RUN_ID}"
test ! -e "$trusted_browser_path"
test ! -e "$target_browser_staging"
sudo install -d -o root -g root -m 0755 "$trusted_browser_path"
sudo install -d -o ` + isolatedTargetAccount + ` -g ` + isolatedTargetAccount + ` -m 0700 "$target_browser_staging"
browser_provisioning_complete=0
cleanup_browser_provisioning() {
  sudo rm -rf -- "$target_browser_staging"
  if [ "$browser_provisioning_complete" -ne 1 ]; then
    sudo rm -rf -- "$trusted_browser_path"
  fi
}
trap cleanup_browser_provisioning EXIT
sudo env PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" "$tooling_playwright" install --with-deps chromium
trusted_browser_executable="$(PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path" "$HIVE_TRUSTED_NODE" -e 'const { chromium } = require(process.argv[1]); process.stdout.write(chromium.executablePath())' "` + isolatedTrustedTooling + `/node_modules/playwright")"
trusted_browser_executable="$(readlink -f "$trusted_browser_executable")"
trusted_browser_path="$(readlink -f "$trusted_browser_path")"
case "$trusted_browser_executable" in
  "$trusted_browser_path"/*) ;;
  *) echo "Pinned browser resolved outside its dedicated root" >&2; exit 1 ;;
esac
if [ ! -x "$trusted_browser_executable" ]; then
  echo "Pinned Visual Hive Playwright browser executable is missing after install: $trusted_browser_executable" >&2
  exit 1
fi
`
		targetEnvironment = strings.Replace(targetEnvironment, "PLAYWRIGHT_BROWSERS_PATH=/home/"+isolatedTargetAccount+"/.cache/ms-playwright", `PLAYWRIGHT_BROWSERS_PATH="$target_browser_staging"`, 1)
		trustedBrowserAfter = `sudo pkill -KILL -u ` + isolatedTargetAccount + ` 2>/dev/null || true
test -d "$target_browser_staging"
if sudo find "$target_browser_staging" -type l -print -quit | grep -q .; then
  echo "Target Playwright browser staging contains a symbolic link" >&2
  exit 1
fi
if sudo find "$target_browser_staging" ! -type d ! -type f -print -quit | grep -q .; then
  echo "Target Playwright browser staging contains a non-regular entry" >&2
  exit 1
fi
test "$(sudo find "$target_browser_staging" -type f | wc -l)" -le 10000
test "$(sudo du -sb "$target_browser_staging" | cut -f 1)" -le 2147483648
sudo cp -a --update=none "$target_browser_staging"/. "$trusted_browser_path"/
sudo rm -rf -- "$target_browser_staging"
test ! -e "$target_browser_staging"
sudo chown -R root:root "$trusted_browser_path"
sudo find "$trusted_browser_path" -type d -exec chmod a+rx,a-w {} +
sudo find "$trusted_browser_path" -type f -exec chmod a+r,a-w {} +
if ! sudo -u ` + isolatedTargetAccount + ` -- test -r "$trusted_browser_path" -a -x "$trusted_browser_path"; then
  echo "Sealed Playwright browser root is not readable and traversable by the isolated target account: $trusted_browser_path" >&2
  exit 1
fi
if sudo -u ` + isolatedTargetAccount + ` -- test -w "$trusted_browser_path"; then
  echo "Sealed Playwright browser root remains writable by the isolated target account: $trusted_browser_path" >&2
  exit 1
fi
HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH="$trusted_browser_path"

trusted_browser_manifest="$RUNNER_TEMP/hive-trusted-playwright-${GITHUB_RUN_ID}.tsv"
test ! -e "$trusted_browser_manifest"
touch "$trusted_browser_manifest"
chmod 0600 "$trusted_browser_manifest"
record_trusted_browser() {
  scope="$1"
  runtime="$2"
  executable="$3"
  case "$scope$runtime$executable" in
    *$'\n'*|*$'\t'*) echo "Playwright runtime identity contains unsupported whitespace" >&2; exit 1 ;;
  esac
  executable="$(readlink -f "$executable")"
  case "$executable" in
    "$trusted_browser_path"/*) ;;
    *) echo "Playwright runtime $runtime resolved outside the sealed browser root: $executable" >&2; exit 1 ;;
  esac
  if [ ! -x "$executable" ]; then
    echo "Playwright browser provisioning incomplete for $runtime: expected executable is missing at $executable (PLAYWRIGHT_BROWSERS_PATH=$trusted_browser_path)" >&2
    exit 1
  fi
  digest="$(sha256sum "$executable" | cut -d ' ' -f 1)"
  sudo -u ` + isolatedTargetAccount + ` -- test ! -w "$executable"
  printf '%s\t%s\t%s\t%s\n' "$scope" "$runtime" "$executable" "$digest" >> "$trusted_browser_manifest"
}
record_trusted_browser visual-hive "$tooling_playwright" "$trusted_browser_executable"
while IFS= read -r -d '' playwright_cli; do
  target_browser_executable="$(` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); process.stdout.write(runtime.chromium.executablePath())' "$playwright_cli")"
  record_trusted_browser target "$playwright_cli" "$target_browser_executable"
done < <(find "$HIVE_TARGET_WORKSPACE" -path '*/node_modules/@playwright/test/cli.js' -not -path '*/.git/*' -print0 | sort -z)
test "$(wc -l < "$trusted_browser_manifest")" -ge 1
chmod 0400 "$trusted_browser_manifest"
browser_provisioning_complete=1
trap - EXIT
echo "HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH=$trusted_browser_path" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_EXECUTABLE=$trusted_browser_executable" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_SHA=$(sha256sum "$trusted_browser_executable" | cut -d ' ' -f 1)" >> "$GITHUB_ENV"
echo "HIVE_TRUSTED_BROWSER_MANIFEST=$trusted_browser_manifest" >> "$GITHUB_ENV"
`
	}
	return fmt.Sprintf(`set -euo pipefail
%s
%s bash --noprofile --norc -euo pipefail <<'HIVE_TARGET_DEPENDENCIES'
test -n "${HIVE_TARGET_WORKSPACE:-}"
test "$(pwd -P)" = "$HIVE_TARGET_WORKSPACE"
%s
%s
while IFS= read -r -d '' playwright_cli; do
  node "$playwright_cli" install chromium
done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)
HIVE_TARGET_DEPENDENCIES
%s
`, trustedBrowserBefore, targetEnvironment, targetInstall, pythonInstall, trustedBrowserAfter)
}

func selectedPackageDependencyScopes(commands [][]string) ([]string, error) {
	selected := map[string]bool{}
	for _, command := range commands {
		packageScope, _, _, ok := packageScriptCommandParts(command)
		if !ok {
			if selectedRootPythonCommand(command) {
				selected["."] = true
			}
			continue
		}
		normalized, err := normalizeSelectedPackageScope(packageScope)
		if err != nil {
			return nil, err
		}
		selected[normalized] = true
	}
	scopes := make([]string, 0, len(selected))
	for scope := range selected {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes, nil
}

func selectedRootPythonCommand(command []string) bool {
	if len(command) == 0 {
		return false
	}
	executable := strings.ToLower(strings.ReplaceAll(command[0], `\`, "/"))
	if index := strings.LastIndex(executable, "/"); index >= 0 {
		executable = executable[index+1:]
	}
	executable = strings.TrimSuffix(executable, ".exe")
	return executable == "python" || executable == "python3" || strings.HasPrefix(executable, "python3.") || executable == "pytest" || executable == "py.test"
}

func normalizeSelectedPackageScope(scope string) (string, error) {
	if scope == "." {
		return scope, nil
	}
	if scope == "" || strings.TrimSpace(scope) != scope || strings.ContainsAny(scope, "\x00\r\n\t") {
		return "", fmt.Errorf("package scope %q is empty or contains unsupported whitespace", scope)
	}
	normalized := strings.ReplaceAll(scope, `\`, "/")
	if strings.HasPrefix(normalized, "/") || (len(normalized) >= 2 && normalized[1] == ':' && ((normalized[0] >= 'a' && normalized[0] <= 'z') || (normalized[0] >= 'A' && normalized[0] <= 'Z'))) {
		return "", fmt.Errorf("package scope %q must be repository-relative", scope)
	}
	if strings.HasPrefix(normalized, "-") {
		return "", fmt.Errorf("package scope %q cannot be interpreted safely as a package installer argument", scope)
	}
	segments := strings.Split(normalized, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("package scope %q contains an unsafe path segment", scope)
		}
	}
	return strings.Join(segments, "/"), nil
}

func selectedPythonDependencyInstallShell(packageScopes []string) string {
	lines := []string{
		`install_python_scope() {`,
		`  local scope="$1"`,
		`  local pyproject`,
		`  local requirements`,
		`  if [ "$scope" = "." ]; then`,
		`    pyproject="pyproject.toml"`,
		`    requirements="requirements.txt"`,
		`  else`,
		`    pyproject="$scope/pyproject.toml"`,
		`    requirements="$scope/requirements.txt"`,
		`  fi`,
		`  if [ -f "$pyproject" ]; then`,
		`    python -m pip install "$scope"`,
		`  elif [ -f "$requirements" ]; then`,
		`    python -m pip install -r "$requirements"`,
		`  fi`,
		`}`,
	}
	for _, scope := range packageScopes {
		lines = append(lines, "install_python_scope "+shellQuote(scope))
	}
	return strings.Join(lines, "\n")
}

func trustedBrowserHandoffVerificationShell() string {
	return `set -euo pipefail
expected_browser_path="` + isolatedTrustedRoot + `/playwright-${GITHUB_RUN_ID}"
if [ "${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-}" != "$expected_browser_path" ]; then
  echo "Trusted Playwright browser path is missing or changed: expected $expected_browser_path, got ${HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH:-<unset>}" >&2
  exit 1
fi
if [ ! -d "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" ] || [ -w "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" ]; then
  echo "Trusted Playwright browser directory is absent or writable: $HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" >&2
  exit 1
fi
if [ ! -f "${HIVE_TRUSTED_BROWSER_MANIFEST:-}" ]; then
  echo "Trusted Playwright browser manifest is missing; provisioning did not complete" >&2
  exit 1
fi
manifest_rows=0
while IFS="$(printf '\t')" read -r scope runtime executable digest; do
  test -n "$scope" && test -n "$runtime" && test -n "$executable" && test -n "$digest"
  case "$executable" in
    "$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"/*) ;;
    *) echo "Trusted Playwright manifest executable escaped its sealed root: $executable" >&2; exit 1 ;;
  esac
  if [ ! -x "$executable" ]; then
    echo "Trusted Playwright browser executable is missing before Visual Hive execution: $executable (runtime=$runtime, PLAYWRIGHT_BROWSERS_PATH=$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH)" >&2
    exit 1
  fi
  test "$(sha256sum "$executable" | cut -d ' ' -f 1)" = "$digest"
  sudo -u ` + isolatedTargetAccount + ` -- test ! -w "$executable"
  manifest_rows=$((manifest_rows + 1))
done < "$HIVE_TRUSTED_BROWSER_MANIFEST"
test "$manifest_rows" -ge 1
while IFS= read -r -d '' playwright_cli; do
  expected_executable="$(` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); process.stdout.write(runtime.chromium.executablePath())' "$playwright_cli")"
  expected_executable="$(readlink -f "$expected_executable")"
  if ! grep -Fq "$(printf 'target\t%s\t%s\t' "$playwright_cli" "$expected_executable")" "$HIVE_TRUSTED_BROWSER_MANIFEST"; then
    echo "Target Playwright runtime lacks a sealed executable binding: $playwright_cli expects $expected_executable" >&2
    exit 1
  fi
  if ! timeout --signal=KILL 60s ` + isolatedVisualTargetEnvPrefix() + ` "$HIVE_TRUSTED_NODE" -e 'const path = require("node:path"); const cli = process.argv[1]; const runtime = require(require.resolve("playwright", { paths: [path.dirname(cli)] })); (async () => { const browser = await runtime.chromium.launch({ headless: true }); await browser.close(); })().catch(error => { console.error(error); process.exit(1); });' "$playwright_cli"; then
    echo "Target Playwright runtime could not launch its exact sealed headless browser: $playwright_cli (PLAYWRIGHT_BROWSERS_PATH=$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH)" >&2
    exit 1
  fi
done < <(find "$HIVE_TARGET_WORKSPACE" -path "$HIVE_TARGET_WORKSPACE/.visual-hive" -prune -o -path '*/node_modules/@playwright/test/cli.js' -not -path '*/.git/*' -print0 | sort -z)
`
}

func isolatedTargetEnvPrefix() string {
	return "sudo -u " + isolatedTargetAccount + ` -- env -i -C "$HIVE_TARGET_WORKSPACE" HOME=/home/` + isolatedTargetAccount + ` PATH="/home/` + isolatedTargetAccount + `/.local/bin:/home/` + isolatedTargetAccount + `/.venv/bin:$PATH" LANG=C.UTF-8 CI=true PLAYWRIGHT_BROWSERS_PATH=/home/` + isolatedTargetAccount + `/.cache/ms-playwright HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE" HIVE_TARGET_PROCESS=1`
}

func isolatedVisualTargetEnvPrefix() string {
	return "sudo -u " + isolatedTargetAccount + ` -- env -i -C "$HIVE_TARGET_WORKSPACE" HOME=/home/` + isolatedTargetAccount + ` PATH="/home/` + isolatedTargetAccount + `/.local/bin:/home/` + isolatedTargetAccount + `/.venv/bin:$PATH" LANG=C.UTF-8 CI=true PLAYWRIGHT_BROWSERS_PATH="$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH" HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE" HIVE_TARGET_PROCESS=1`
}

func isolatedVisualPullRequestTargetEnvPrefix() string {
	return isolatedVisualTargetEnvPrefix() + ` GITHUB_ACTIONS=true GITHUB_REPOSITORY="$HIVE_TARGET_REPOSITORY" GITHUB_HEAD_REF="$HIVE_TARGET_HEAD_REF" GITHUB_BASE_REF="$HIVE_TARGET_BASE_REF" GITHUB_SHA="$HIVE_TARGET_HEAD_SHA" GITHUB_EVENT_NAME=pull_request GITHUB_REF="refs/heads/$HIVE_TARGET_HEAD_REF" GITHUB_REF_NAME="$HIVE_TARGET_HEAD_REF" GITHUB_RUN_ID="$HIVE_TARGET_RUN_ID" GITHUB_RUN_ATTEMPT="$HIVE_TARGET_RUN_ATTEMPT" GITHUB_WORKFLOW="$HIVE_TARGET_WORKFLOW"`
}

func isolatedVisualEvidenceLauncherPrefix(pullRequest bool) string {
	prefix := `sudo -- env -i HIVE_TARGET_WORKSPACE="$HIVE_TARGET_WORKSPACE" HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH="$HIVE_TRUSTED_PLAYWRIGHT_BROWSERS_PATH"`
	if pullRequest {
		prefix += ` GITHUB_ACTIONS=true GITHUB_REPOSITORY="$HIVE_TARGET_REPOSITORY" GITHUB_HEAD_REF="$HIVE_TARGET_HEAD_REF" GITHUB_BASE_REF="$HIVE_TARGET_BASE_REF" GITHUB_SHA="$HIVE_TARGET_HEAD_SHA" GITHUB_EVENT_NAME=pull_request GITHUB_REF="refs/heads/$HIVE_TARGET_HEAD_REF" GITHUB_REF_NAME="$HIVE_TARGET_HEAD_REF" GITHUB_RUN_ID="$HIVE_TARGET_RUN_ID" GITHUB_RUN_ATTEMPT="$HIVE_TARGET_RUN_ATTEMPT" GITHUB_WORKFLOW="$HIVE_TARGET_WORKFLOW"`
	} else {
		prefix += ` GITHUB_ACTIONS=true GITHUB_REPOSITORY="$GITHUB_REPOSITORY" GITHUB_HEAD_REF="$GITHUB_HEAD_REF" GITHUB_BASE_REF="$GITHUB_BASE_REF" GITHUB_SHA="$GITHUB_SHA" GITHUB_EVENT_NAME="$GITHUB_EVENT_NAME" GITHUB_REF="$GITHUB_REF" GITHUB_REF_NAME="$GITHUB_REF_NAME" GITHUB_RUN_ID="$GITHUB_RUN_ID" GITHUB_RUN_ATTEMPT="$GITHUB_RUN_ATTEMPT" GITHUB_WORKFLOW="$GITHUB_WORKFLOW"`
	}
	return prefix + ` /usr/bin/unshare --mount --fork ` + isolatedEvidenceLauncher
}

func workflowNeeds(jobNames []string) string {
	if len(jobNames) == 0 {
		return "    if: ${{ always() }}"
	}
	return "    needs:\n" + workflowNeedLines(jobNames) + "\n    if: ${{ always() }}"
}

func appendWorkflowNeed(needs, jobName string) string {
	return appendWorkflowNeeds(needs, jobName)
}

func appendWorkflowNeeds(needs string, jobNames ...string) string {
	if strings.HasPrefix(needs, "    needs:\n") {
		insert := workflowNeedLines(jobNames) + "\n"
		return strings.Replace(needs, "    needs:\n", "    needs:\n"+insert, 1)
	}
	return "    needs:\n" + workflowNeedLines(jobNames) + "\n" + needs
}

func workflowNeedLines(jobNames []string) string {
	lines := make([]string, 0, len(jobNames))
	seen := map[string]bool{}
	for _, name := range jobNames {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		lines = append(lines, "      - "+name)
	}
	return strings.Join(lines, "\n")
}

func joinWorkflowJobs(jobs ...string) string {
	joined := make([]string, 0, len(jobs))
	for _, job := range jobs {
		job = strings.Trim(job, "\n")
		if job != "" {
			joined = append(joined, job)
		}
	}
	return strings.Join(joined, "\n\n")
}

func dedentWorkflowShell(value string) string {
	lines := strings.Split(strings.Trim(value, "\n"), "\n")
	for index := range lines {
		lines[index] = strings.TrimPrefix(lines[index], "          ")
	}
	return strings.Join(lines, "\n")
}

func indentWorkflowShell(value string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.Trim(value, "\n"), "\n")
	for index := range lines {
		if lines[index] != "" {
			lines[index] = prefix + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}
