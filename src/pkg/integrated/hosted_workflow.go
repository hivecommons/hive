package integrated

import (
	"fmt"
	"regexp"
	"strings"
)

const HostedControllerWorkflowPath = ".github/workflows/hive-controller.yml"
const HostedControllerProtocol = 1

var (
	hostedRepositoryPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,38}/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	hostedRepositoryIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	hostedBranchPattern       = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,125}[A-Za-z0-9])?$`)
	hostedReleaseTagPattern   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`)
	hostedCommitPattern       = regexp.MustCompile(`^[a-f0-9]{40}$`)
	hostedDigestPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	hostedSecretPattern       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	hostedCronPattern         = regexp.MustCompile(`^(?:\*|\*/(?:[1-9]|[1-5][0-9])|[0-5]?[0-9]) (?:\*|\*/(?:[1-9]|1[0-9]|2[0-3])|[01]?[0-9]|2[0-3]) (?:\*|\*/(?:[1-9]|[12][0-9]|3[01])|[1-9]|[12][0-9]|3[01]) (?:\*|\*/(?:[1-9]|1[0-2])|[1-9]|1[0-2]) (?:\*|\*/[1-6]|[0-6])$`)
)

// HostedWorkflowConfig binds a generated controller to one repository, one
// durable state branch, and one immutable integrated distribution. It is kept
// separate from Config so adding the hosted controller cannot silently change
// the existing local setup contract.
type HostedWorkflowConfig struct {
	Repository                 string
	RepositoryID               string
	DefaultBranch              string
	ScheduleCron               string
	StateBranch                string
	StateKeySecret             string
	ReleaseRepository          string
	ReleaseTag                 string
	HiveCommit                 string
	VisualHiveCommit           string
	DistributionManifestSHA256 string
	ProviderRequired           bool
	PreviousRelease            *HostedReleaseIdentity
}

// GenerateHostedControllerWorkflow renders the contents installed at
// HostedControllerWorkflowPath. The workflow never executes target repository
// code: it only runs the attested Hive distribution on GitHub-hosted runners.
func GenerateHostedControllerWorkflow(config HostedWorkflowConfig) (string, error) {
	if err := validateHostedWorkflowConfig(config); err != nil {
		return "", err
	}

	common := hostedControllerJobSteps(config)
	return fmt.Sprintf(`name: Hive Hosted Controller
run-name: Hive ${{ inputs.operation || 'cycle' }} ${{ inputs.request_id || github.run_id }}

on:
  schedule:
    - cron: %q
  workflow_dispatch:
    inputs:
      request_id:
        description: Unique idempotency key for this controller request
        required: true
        type: string
      operation:
        description: Hosted Hive controller operation
        required: true
        default: cycle
        type: choice
        options:
          - cycle
          - status
          - doctor
          - pause
          - resume
          - approve-baseline-plan
          - approve-baseline-apply
          - approve-merge-plan
          - approve-merge-apply
          - retry-repair
          - recover-dispatch-plan
          - recover-dispatch-apply
      request_payload:
        description: Base64url content-bound hosted operator request
        required: false
        type: string
      request_sha256:
        description: SHA-256 of the exact hosted operator request bytes
        required: false
        type: string

permissions: {}

concurrency:
  group: hive-controller-%s
  cancel-in-progress: false

jobs:
  cycle:
    if: ${{ github.event_name == 'schedule' || inputs.operation == 'cycle' }}
    runs-on: ubuntu-latest
    timeout-minutes: 60
    permissions:
      actions: write
      contents: write
      issues: write
      pull-requests: write
      checks: read
      statuses: write
    steps:
%s

  control:
    if: ${{ github.event_name == 'workflow_dispatch' && (inputs.operation == 'pause' || inputs.operation == 'resume' || startsWith(inputs.operation, 'approve-') || inputs.operation == 'retry-repair' || startsWith(inputs.operation, 'recover-dispatch-')) }}
    runs-on: ubuntu-latest
    timeout-minutes: 20
    permissions:
      actions: read
      contents: write
      issues: write
      pull-requests: write
      checks: read
      statuses: read
    steps:
%s

  status:
    if: ${{ github.event_name == 'workflow_dispatch' && (inputs.operation == 'status' || inputs.operation == 'doctor') }}
    runs-on: ubuntu-latest
    timeout-minutes: 20
    permissions:
      actions: read
      contents: read
      issues: read
      pull-requests: read
      checks: read
      statuses: read
    steps:
%s
`, config.ScheduleCron, config.RepositoryID, common, common, common), nil
}

func validateHostedWorkflowConfig(config HostedWorkflowConfig) error {
	for name, value := range map[string]string{
		"repository":         config.Repository,
		"release repository": config.ReleaseRepository,
	} {
		if !hostedRepositoryPattern.MatchString(value) {
			return fmt.Errorf("hosted workflow %s must be one strict owner/name pair", name)
		}
		parts := strings.Split(value, "/")
		if len(parts) != 2 || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.HasSuffix(parts[0], ".") || strings.HasSuffix(parts[1], ".") {
			return fmt.Errorf("hosted workflow %s contains an unsafe path segment", name)
		}
	}
	if strings.EqualFold(config.ReleaseRepository, "kubestellar/hive") {
		return fmt.Errorf("hosted workflow release repository must be the maintained Hive fork, not kubestellar/hive")
	}
	if !hostedRepositoryIDPattern.MatchString(config.RepositoryID) {
		return fmt.Errorf("hosted workflow repository ID must be a positive decimal GitHub repository ID")
	}
	for name, value := range map[string]string{"default branch": config.DefaultBranch, "state branch": config.StateBranch} {
		if !hostedBranchPattern.MatchString(value) || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.Contains(value, "@{") || strings.HasSuffix(value, ".lock") {
			return fmt.Errorf("hosted workflow %s is not a strict Git ref path", name)
		}
	}
	if config.DefaultBranch == config.StateBranch {
		return fmt.Errorf("hosted workflow state branch must be separate from the default branch")
	}
	if !hostedCronPattern.MatchString(config.ScheduleCron) {
		return fmt.Errorf("hosted workflow cron must be a strict five-field numeric, wildcard, or step cadence")
	}
	if !hostedReleaseTagPattern.MatchString(config.ReleaseTag) {
		return fmt.Errorf("hosted workflow release tag must be an immutable integrated release tag")
	}
	if !hostedCommitPattern.MatchString(config.HiveCommit) || !hostedCommitPattern.MatchString(config.VisualHiveCommit) {
		return fmt.Errorf("hosted workflow Hive and Visual Hive commits must be lowercase 40-character commit SHAs")
	}
	if !hostedDigestPattern.MatchString(config.DistributionManifestSHA256) {
		return fmt.Errorf("hosted workflow distribution manifest digest must be a lowercase SHA-256")
	}
	if config.PreviousRelease != nil {
		previous := *config.PreviousRelease
		if !hostedReleaseTagPattern.MatchString(previous.Version) || !hostedCommitPattern.MatchString(previous.HiveCommit) ||
			!hostedCommitPattern.MatchString(previous.VisualHiveCommit) || !hostedDigestPattern.MatchString(previous.DistributionManifestSHA256) ||
			previous.HostedControllerProtocol != HostedControllerProtocol {
			return fmt.Errorf("hosted workflow previous release identity must be a complete immutable integrated release")
		}
		current := HostedReleaseIdentity{
			Version: config.ReleaseTag, HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveCommit,
			DistributionManifestSHA256: config.DistributionManifestSHA256, HostedControllerProtocol: HostedControllerProtocol,
		}
		if equalHostedReleaseIdentity(previous, current) {
			return fmt.Errorf("hosted workflow previous release identity must differ from the current release")
		}
	}
	if !hostedSecretPattern.MatchString(config.StateKeySecret) {
		return fmt.Errorf("hosted workflow state key secret must be an uppercase GitHub Actions secret name")
	}
	return nil
}

func hostedControllerJobSteps(config HostedWorkflowConfig) string {
	previousVersion, previousHive, previousVisual, previousManifest, previousProtocol := previousHostedReleaseValues(config.PreviousRelease)
	providerRequired := "false"
	if config.ProviderRequired {
		providerRequired = "true"
	}
	return fmt.Sprintf(`      - name: Validate controller invocation
        shell: bash
        env:
          HIVE_EVENT_NAME: ${{ github.event_name }}
          HIVE_REF: ${{ github.ref }}
          HIVE_REPOSITORY_CONTEXT: ${{ github.repository }}
          HIVE_REPOSITORY_ID_CONTEXT: ${{ github.repository_id }}
          HIVE_DEFAULT_BRANCH_CONTEXT: ${{ github.event.repository.default_branch }}
          HIVE_WORKFLOW_REF: ${{ github.workflow_ref }}
        run: |
          set -euo pipefail
          case "$HIVE_EVENT_NAME" in schedule|workflow_dispatch) ;; *) exit 1 ;; esac
          test "$HIVE_REF" = "refs/heads/%s"
          test "$HIVE_REPOSITORY_CONTEXT" = "%s"
          test "$HIVE_REPOSITORY_ID_CONTEXT" = "%s"
          test "$HIVE_DEFAULT_BRANCH_CONTEXT" = "%s"
          test "$HIVE_WORKFLOW_REF" = "%s/%s@refs/heads/%s"
      - name: Checkout default branch for Hive-controlled Git operations
        uses: actions/checkout@%s
        with:
          ref: ${{ github.sha }}
          fetch-depth: 1
          persist-credentials: false
          path: target-metadata
      - name: Checkout durable controller state
        uses: actions/checkout@%s
        with:
          ref: %s
          fetch-depth: 2
          persist-credentials: true
          path: hive-controller-state
      - name: Install exact attested Hive release
        shell: bash
        env:
          GH_TOKEN: ${{ github.token }}
          HIVE_RELEASE_REPOSITORY: %s
          HIVE_RELEASE_TAG: %s
          HIVE_EXPECTED_COMMIT: %s
          HIVE_EXPECTED_VISUAL_COMMIT: %s
          HIVE_EXPECTED_MANIFEST_SHA256: %s
        run: |
          set -euo pipefail
          umask 077
          controller_home="$RUNNER_TEMP/hive-controller-home-${GITHUB_JOB}-${GITHUB_RUN_ID}"
          release_dir="$RUNNER_TEMP/hive-controller-release-${GITHUB_JOB}-${GITHUB_RUN_ID}"
          install_dir="$RUNNER_TEMP/install-hive-${GITHUB_JOB}-${GITHUB_RUN_ID}"
          mkdir -m 0700 -- "$controller_home" "$release_dir"
          export HOME="$controller_home"
          before_commit="$(gh api "repos/$HIVE_RELEASE_REPOSITORY/commits/$HIVE_RELEASE_TAG" --jq .sha)"
          test "$before_commit" = "$HIVE_EXPECTED_COMMIT"
          gh release download "$HIVE_RELEASE_TAG" --repo "$HIVE_RELEASE_REPOSITORY" \
            --pattern install-integrated.sh --pattern install-integrated.sh.sha256 --dir "$release_dir"
          for subject in install-integrated.sh install-integrated.sh.sha256; do
            test -f "$release_dir/$subject"
            test ! -L "$release_dir/$subject"
            gh attestation verify "$release_dir/$subject" \
              --repo "$HIVE_RELEASE_REPOSITORY" \
              --signer-workflow "$HIVE_RELEASE_REPOSITORY/.github/workflows/integrated-release.yml" \
              --source-ref "refs/tags/$HIVE_RELEASE_TAG" \
              --source-digest "$HIVE_EXPECTED_COMMIT" \
              --signer-digest "$HIVE_EXPECTED_COMMIT" \
              --deny-self-hosted-runners >/dev/null
          done
          (cd "$release_dir" && sha256sum --check install-integrated.sh.sha256)
          chmod 0500 "$release_dir/install-integrated.sh"
          sh "$release_dir/install-integrated.sh" --repo "$HIVE_RELEASE_REPOSITORY" \
            --version "$HIVE_RELEASE_TAG" --install-dir "$install_dir"
          after_commit="$(gh api "repos/$HIVE_RELEASE_REPOSITORY/commits/$HIVE_RELEASE_TAG" --jq .sha)"
          test "$after_commit" = "$before_commit"
          test "$(sha256sum "$install_dir/distribution-manifest.json" | cut -d ' ' -f 1)" = "$HIVE_EXPECTED_MANIFEST_SHA256"
          "$install_dir/runtime/node" - "$install_dir/distribution-manifest.json" \
            "$HIVE_RELEASE_TAG" "$HIVE_EXPECTED_COMMIT" "$HIVE_EXPECTED_VISUAL_COMMIT" <<'NODE'
          const fs = require('node:fs');
          const [manifestPath, releaseTag, hiveCommit, visualCommit] = process.argv.slice(2);
          const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
          if (manifest.schema_version !== 'hive.integrated-distribution.v1' ||
              manifest.hive_version !== releaseTag || manifest.hive_commit !== hiveCommit ||
              manifest.visual_hive_commit !== visualCommit || manifest.hosted_controller_protocol !== 1 || manifest.os !== 'linux' ||
              manifest.architecture !== 'amd64') process.exit(1);
          NODE
          "$install_dir/hive" --version | grep -F "$HIVE_RELEASE_TAG" >/dev/null
          "$install_dir/hive" --version | grep -F "$HIVE_EXPECTED_COMMIT" >/dev/null
          printf 'HIVE_HOSTED_BIN=%%s\n' "$install_dir/hive" >> "$GITHUB_ENV"
          printf 'HIVE_HOSTED_INSTALL=%%s\n' "$install_dir" >> "$GITHUB_ENV"
      - name: Install exact integrity-bound Codex provider
        id: provider-install
        if: ${{ %s && (github.event_name == 'schedule' || inputs.operation == 'cycle' || inputs.operation == 'doctor') }}
        continue-on-error: true
        shell: bash
        run: |
          set -euo pipefail
          umask 077
          provider_dir="$RUNNER_TEMP/hive-hosted-provider-${GITHUB_JOB}-${GITHUB_RUN_ID}"
          mkdir -m 0700 -- "$provider_dir"
          cd "$provider_dir"
          native_tarball="$("$HIVE_HOSTED_INSTALL/runtime/npm" pack @openai/codex@0.144.1-linux-x64 --ignore-scripts --pack-destination "$provider_dir" --silent)"
          test "$(openssl dgst -sha512 -binary "$provider_dir/$native_tarball" | openssl base64 -A)" = "HNGVI+BulrOaC/0IzBvd6EL62j7LrlbFKibrhw6hZjjCjAeUYzRB2jB4qDzXN1NfqDi6Xrvniof3kwbwab24lg=="
          mkdir -m 0700 -- "$provider_dir/native"
          tar -xzf "$provider_dir/$native_tarball" --strip-components=1 -C "$provider_dir/native"
          codex_path="$(find "$provider_dir/native" -type f -name codex -path '*/vendor/*' -print -quit)"
          test -n "$codex_path"
          chmod 0500 "$codex_path"
          test "$("$codex_path" --version)" = "codex-cli 0.144.1"
          printf 'HIVE_HOSTED_CODEX_CANDIDATE=%%s\n' "$codex_path" >> "$GITHUB_ENV"
          printf 'CODEX_HOME=%%s\n' "$RUNNER_TEMP/hive-hosted-codex-home-${GITHUB_JOB}-${GITHUB_RUN_ID}" >> "$GITHUB_ENV"
      - name: Authenticate exact Codex provider
        id: provider-auth
        if: ${{ %s && steps.provider-install.outcome == 'success' }}
        continue-on-error: true
        shell: bash
        env:
          OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
        run: |
          set -euo pipefail
          umask 077
          test -n "$OPENAI_API_KEY"
          mkdir -m 0700 -- "$CODEX_HOME"
          printf '%%s' "$OPENAI_API_KEY" | "$HIVE_HOSTED_CODEX_CANDIDATE" login --with-api-key >/dev/null 2>&1
          unset OPENAI_API_KEY
          auth_file="$CODEX_HOME/auth.json"
          test -f "$auth_file"
          test ! -L "$auth_file"
          chmod 0600 "$auth_file"
          test "$(stat -c '%%a' "$auth_file")" = 600
          "$HIVE_HOSTED_CODEX_CANDIDATE" login status >/dev/null 2>&1
          printf 'HIVE_HOSTED_CODEX=%%s\n' "$HIVE_HOSTED_CODEX_CANDIDATE" >> "$GITHUB_ENV"
      - name: Run hosted Hive operation
        id: hosted
        shell: bash
        env:
          HIVE_HOSTED_STATE_KEY: ${{ secrets.%s }}
          HIVE_GITHUB_TOKEN: ${{ github.token }}
          HIVE_INPUT_OPERATION: ${{ inputs.operation }}
          HIVE_INPUT_REQUEST_ID: ${{ inputs.request_id }}
          HIVE_INPUT_REQUEST_PAYLOAD: ${{ inputs.request_payload }}
          HIVE_INPUT_REQUEST_SHA256: ${{ inputs.request_sha256 }}
          HIVE_EVENT_ACTOR_ID: ${{ github.event.sender.id }}
          HIVE_EVENT_ACTOR_LOGIN: ${{ github.event.sender.login }}
          HIVE_PREVIOUS_RELEASE_VERSION: %q
          HIVE_PREVIOUS_HIVE_COMMIT: %q
          HIVE_PREVIOUS_VISUAL_COMMIT: %q
          HIVE_PREVIOUS_MANIFEST_SHA256: %q
          HIVE_PREVIOUS_CONTROLLER_PROTOCOL: %d
        run: |
          set -euo pipefail
          umask 077
          operation="$HIVE_INPUT_OPERATION"
          request_id="$HIVE_INPUT_REQUEST_ID"
          if test "$GITHUB_EVENT_NAME" = schedule; then
            operation=cycle
            request_id="schedule-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
          fi
          case "$operation" in cycle|status|doctor|pause|resume|approve-baseline-plan|approve-baseline-apply|approve-merge-plan|approve-merge-apply|retry-repair|recover-dispatch-plan|recover-dispatch-apply) ;; *) exit 2 ;; esac
          printf '%%s' "$request_id" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
          result="$RUNNER_TEMP/hive-hosted-result-${GITHUB_JOB}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.json"
          set +e
          "$HIVE_HOSTED_BIN" hosted-cycle \
            --repo %s \
            --repository-id %s \
            --state-branch %s \
            --state-dir "$GITHUB_WORKSPACE/hive-controller-state" \
            --checkout-dir "$GITHUB_WORKSPACE/target-metadata" \
            --state-key-env HIVE_HOSTED_STATE_KEY \
            --operation "$operation" \
            --request-id "$request_id" \
            --request-payload "$HIVE_INPUT_REQUEST_PAYLOAD" \
            --request-sha256 "$HIVE_INPUT_REQUEST_SHA256" \
            --restore-release-version "$HIVE_PREVIOUS_RELEASE_VERSION" \
            --restore-hive-commit "$HIVE_PREVIOUS_HIVE_COMMIT" \
            --restore-visual-commit "$HIVE_PREVIOUS_VISUAL_COMMIT" \
            --restore-manifest-sha256 "$HIVE_PREVIOUS_MANIFEST_SHA256" \
            --restore-controller-protocol "$HIVE_PREVIOUS_CONTROLLER_PROTOCOL" \
            --json >"$result"
          hive_exit=$?
          set -e
          case "$hive_exit" in ''|*[!0-9]*) exit 1 ;; esac
          printf 'exit_code=%%s\n' "$hive_exit" >> "$GITHUB_OUTPUT"
          test -f "$result"
          test ! -L "$result"
          result_bytes="$(wc -c < "$result" | tr -d ' ')"
          test "$result_bytes" -gt 1
          test "$result_bytes" -le 1048576
          python3 - "$result" <<'PY'
          import json, pathlib, sys
          raw = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
          decoder = json.JSONDecoder()
          _, end = decoder.raw_decode(raw)
          if raw[end:].strip():
              raise SystemExit("hosted-cycle stdout contained more than one JSON document")
          PY
          chmod 0400 "$result"
      - name: Upload bounded controller result
        if: ${{ always() && steps.hosted.outcome == 'success' }}
        uses: actions/upload-artifact@%s
        with:
          name: hive-hosted-result-${{ github.job }}-${{ github.run_id }}-${{ github.run_attempt }}
          path: ${{ runner.temp }}/hive-hosted-result-${{ github.job }}-${{ github.run_id }}-${{ github.run_attempt }}.json
          if-no-files-found: error
          retention-days: 14
          compression-level: 9
          overwrite: false
          include-hidden-files: false
      - name: Enforce Hive operation result
        if: ${{ always() }}
        shell: bash
        env:
          HIVE_EXIT_CODE: ${{ steps.hosted.outputs.exit_code }}
        run: |
          set -euo pipefail
          test "$HIVE_EXIT_CODE" = 0`,
		config.DefaultBranch, config.Repository, config.RepositoryID, config.DefaultBranch,
		config.Repository, HostedControllerWorkflowPath, config.DefaultBranch,
		checkoutActionSHA, checkoutActionSHA, config.StateBranch,
		config.ReleaseRepository, config.ReleaseTag, config.HiveCommit, config.VisualHiveCommit,
		config.DistributionManifestSHA256, providerRequired, providerRequired, config.StateKeySecret,
		previousVersion, previousHive, previousVisual, previousManifest, previousProtocol, config.Repository,
		config.RepositoryID, config.StateBranch, uploadArtifactActionSHA)
}

func previousHostedReleaseValues(identity *HostedReleaseIdentity) (string, string, string, string, int) {
	if identity == nil {
		return "", "", "", "", 0
	}
	return identity.Version, identity.HiveCommit, identity.VisualHiveCommit, identity.DistributionManifestSHA256, identity.HostedControllerProtocol
}
