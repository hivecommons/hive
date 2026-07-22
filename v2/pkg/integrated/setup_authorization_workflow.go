package integrated

import (
	"encoding/json"
	"strconv"
	"strings"
)

func setupAuthorizationWorkflowJob(config Config) string {
	transferAuthorizerID := config.SetupAuthorizationPreviousActorID
	if transferAuthorizerID <= 0 {
		transferAuthorizerID = config.SetupAuthorizationActorID
	}
	required := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	absent := []string{}
	if config.VisualHive {
		required = append(required, ".github/workflows/visual-hive-pr.yml", "docs/visual-hive.md", "visual-hive.config.yaml")
		absent = append(absent, standaloneVisualHiveWriterWorkflowPaths()...)
	}
	allowed := managedSetupFilesForConfig(config)
	uninstallRequired, uninstallAbsent := managedUninstallRequiredPaths(config)
	uninstallAllowed := managedSetupFilesForConfig(config)
	requiredJSON, _ := json.Marshal(required)
	absentJSON, _ := json.Marshal(absent)
	allowedJSON, _ := json.Marshal(allowed)
	uninstallRequiredJSON, _ := json.Marshal(uninstallRequired)
	uninstallAbsentJSON, _ := json.Marshal(uninstallAbsent)
	uninstallAllowedJSON, _ := json.Marshal(uninstallAllowed)
	value := `  setup-authorization:
    name: Hive setup authorization
    if: ${{ github.event_name == 'pull_request' || (github.event_name == 'pull_request_target' && github.event.pull_request.head.ref == '__EXPECTED_UNINSTALL_REF__' && github.event.pull_request.head.repo.full_name == github.repository && github.event.pull_request.base.repo.full_name == github.repository) }}
    runs-on: ubuntu-latest
    timeout-minutes: 12
    permissions:
      contents: read
      pull-requests: read
      statuses: read
    outputs:
      authorized: ${{ steps.authorize.outputs.authorized }}
      context: ${{ steps.authorize.outputs.context }}
      binding_digest: ${{ steps.authorize.outputs.binding_digest }}
      diff_digest: ${{ steps.authorize.outputs.diff_digest }}
      operation: ${{ steps.authorize.outputs.operation }}
    steps:
      - uses: actions/checkout@__CHECKOUT_ACTION__
        with:
          # The authorization lane reads proposed Git objects but never checks
          # out the proposed tree, including under pull_request_target.
          ref: ${{ github.event.pull_request.base.sha }}
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-node@__SETUP_NODE_ACTION__
        with:
          node-version: 22.23.1
      - name: Verify out-of-band exact setup authorization
        id: authorize
        shell: bash
        env:
          HIVE_STATUS_TOKEN: ${{ github.token }}
          HIVE_API_URL: ${{ github.api_url }}
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_REPOSITORY_ID: ${{ github.event.repository.id }}
          HIVE_PULL_REQUEST: ${{ github.event.pull_request.number }}
          HIVE_PULL_REQUEST_URL: ${{ github.event.pull_request.html_url }}
          HIVE_HEAD_REPOSITORY: ${{ github.event.pull_request.head.repo.full_name }}
          HIVE_HEAD_REF: ${{ github.event.pull_request.head.ref }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_BASE_REPOSITORY: ${{ github.event.pull_request.base.repo.full_name }}
          HIVE_BASE_REF: ${{ github.event.pull_request.base.ref }}
          HIVE_BASE_SHA: ${{ github.event.pull_request.base.sha }}
          HIVE_EXPECTED_HEAD_REF: __EXPECTED_HEAD_REF__
          HIVE_EXPECTED_UPGRADE_REF: __EXPECTED_UPGRADE_REF__
          HIVE_EXPECTED_ROLLBACK_REF: __EXPECTED_ROLLBACK_REF__
          HIVE_EXPECTED_AUTHORIZER_TRANSFER_REF: __EXPECTED_AUTHORIZER_TRANSFER_REF__
          HIVE_EXPECTED_UNINSTALL_REF: __EXPECTED_UNINSTALL_REF__
          HIVE_EXPECTED_BASE_REF: __EXPECTED_BASE_REF__
          HIVE_AUTHORIZER_ID: "__AUTHORIZER_ID__"
          HIVE_TRANSFER_AUTHORIZER_ID: "__TRANSFER_AUTHORIZER_ID__"
          HIVE_REQUIRED_FILES: '__REQUIRED_FILES__'
          HIVE_REQUIRED_ABSENT: '__REQUIRED_ABSENT__'
          HIVE_ALLOWED_FILES: '__ALLOWED_FILES__'
          HIVE_UNINSTALL_REQUIRED_FILES: '__UNINSTALL_REQUIRED_FILES__'
          HIVE_UNINSTALL_REQUIRED_ABSENT: '__UNINSTALL_REQUIRED_ABSENT__'
          HIVE_UNINSTALL_ALLOWED_FILES: '__UNINSTALL_ALLOWED_FILES__'
        run: |
          set -euo pipefail
          case "$HIVE_HEAD_REF" in
            "$HIVE_EXPECTED_HEAD_REF"|"$HIVE_EXPECTED_UPGRADE_REF"|"$HIVE_EXPECTED_ROLLBACK_REF"|"$HIVE_EXPECTED_AUTHORIZER_TRANSFER_REF"|"$HIVE_EXPECTED_UNINSTALL_REF")
              authorization_header="$(printf 'x-access-token:%s' "$HIVE_STATUS_TOKEN" | base64 | tr -d '\r\n')"
              git -c protocol.file.allow=never -c "http.extraheader=AUTHORIZATION: basic $authorization_header" fetch --force --no-tags --no-recurse-submodules origin "refs/heads/$HIVE_HEAD_REF:refs/remotes/origin/hive-authorization-head"
              unset authorization_header
              ;;
          esac
          node <<'NODE'
__VERIFIER_SCRIPT__
          NODE

`
	replacer := strings.NewReplacer(
		"__CHECKOUT_ACTION__", checkoutActionSHA,
		"__SETUP_NODE_ACTION__", setupNodeActionSHA,
		"__EXPECTED_HEAD_REF__", config.SetupBranch,
		"__EXPECTED_UPGRADE_REF__", managedOperationBranch(string(OperationUpgrade), config.RepositoryID),
		"__EXPECTED_ROLLBACK_REF__", managedOperationBranch(string(OperationRollback), config.RepositoryID),
		"__EXPECTED_AUTHORIZER_TRANSFER_REF__", managedOperationBranch(string(OperationAuthorizerTransfer), config.RepositoryID),
		"__EXPECTED_UNINSTALL_REF__", managedOperationBranch(string(OperationUninstall), config.RepositoryID),
		"__EXPECTED_BASE_REF__", config.DefaultBranch,
		"__AUTHORIZER_ID__", strconv.FormatInt(config.SetupAuthorizationActorID, 10),
		"__TRANSFER_AUTHORIZER_ID__", strconv.FormatInt(transferAuthorizerID, 10),
		"__REQUIRED_FILES__", string(requiredJSON),
		"__REQUIRED_ABSENT__", string(absentJSON),
		"__ALLOWED_FILES__", string(allowedJSON),
		"__UNINSTALL_REQUIRED_FILES__", string(uninstallRequiredJSON),
		"__UNINSTALL_REQUIRED_ABSENT__", string(uninstallAbsentJSON),
		"__UNINSTALL_ALLOWED_FILES__", string(uninstallAllowedJSON),
		"__VERIFIER_SCRIPT__", setupAuthorizationWorkflowVerifierScript(),
	)
	rendered := replacer.Replace(value)
	return strings.Replace(rendered, "HIVE_UNINSTALL_REQUIRED_FILES: '[]'", `HIVE_UNINSTALL_REQUIRED_FILES: "[]"`, 1)
}

func uninstallCheckPublisherWorkflowJob() string {
	return `  uninstall-required-check:
    name: Publish exact Hive uninstall check
    needs: setup-authorization
    if: ${{ always() && github.event_name == 'pull_request_target' && needs.setup-authorization.outputs.operation == 'uninstall' }}
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions:
      checks: write
    steps:
      - name: Publish exact authorized uninstall check
        shell: bash
        env:
          HIVE_CHECK_TOKEN: ${{ github.token }}
          HIVE_API_URL: ${{ github.api_url }}
          HIVE_REPOSITORY: ${{ github.repository }}
          HIVE_HEAD_SHA: ${{ github.event.pull_request.head.sha }}
          HIVE_PULL_REQUEST_URL: ${{ github.event.pull_request.html_url }}
          HIVE_SETUP_AUTHORIZED: ${{ needs.setup-authorization.outputs.authorized }}
          HIVE_SETUP_BINDING_DIGEST: ${{ needs.setup-authorization.outputs.binding_digest }}
        run: |
          set -euo pipefail
          node <<'NODE'
          const digest = String(process.env.HIVE_SETUP_BINDING_DIGEST || "").toLowerCase();
          const head = String(process.env.HIVE_HEAD_SHA || "").toLowerCase();
          const repository = String(process.env.HIVE_REPOSITORY || "");
          const details = String(process.env.HIVE_PULL_REQUEST_URL || "");
          if (process.env.HIVE_SETUP_AUTHORIZED !== "true" || !/^[a-f0-9]{64}$/u.test(digest) || !/^[a-f0-9]{40}$/u.test(head) || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(repository) || !details) {
            throw new Error("exact out-of-band uninstall authorization is absent");
          }
          const external = "hive-uninstall-" + digest;
          const headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": "Bearer " + process.env.HIVE_CHECK_TOKEN,
            "Content-Type": "application/json",
            "X-GitHub-Api-Version": "2022-11-28"
          };
          async function request(url, init = {}) {
            const response = await fetch(url, { ...init, headers: { ...headers, ...(init.headers || {}) } });
            const text = await response.text();
            if (!response.ok) throw new Error("GitHub Checks API returned HTTP " + response.status + ": " + text.slice(0, 300));
            return text ? JSON.parse(text) : {};
          }
          async function exactExisting() {
            const matched = [];
            for (let page = 1; page <= 10; page++) {
              const value = await request(process.env.HIVE_API_URL + "/repos/" + repository + "/commits/" + head + "/check-runs?check_name=visual-hive&filter=all&per_page=100&page=" + page);
              const runs = Array.isArray(value.check_runs) ? value.check_runs : [];
              for (const run of runs) {
                if (String(run.external_id || "") === external && String(run.head_sha || "").toLowerCase() === head && run.app && String(run.app.slug || "").toLowerCase() === "github-actions") matched.push(run);
              }
              if (runs.length < 100) break;
              if (page === 10) throw new Error("exact uninstall check discovery exceeded its bounded page limit");
            }
            if (matched.length > 1) throw new Error("multiple exact uninstall checks claim one authorization binding");
            return matched[0] || null;
          }
          async function main() {
            let check = await exactExisting();
            if (!check) {
              check = await request(process.env.HIVE_API_URL + "/repos/" + repository + "/check-runs", {
                method: "POST",
                body: JSON.stringify({
                  name: "visual-hive",
                  head_sha: head,
                  status: "completed",
                  conclusion: "success",
                  details_url: details,
                  external_id: external,
                  output: { title: "Exact Hive uninstall authorized", summary: "Hive verified the immutable cleanup diff out of band. No pull-request code or workflow from the proposed head was executed." }
                })
              });
            }
            const exact = Number(check.id) > 0 && check.name === "visual-hive" && String(check.head_sha || "").toLowerCase() === head &&
              check.status === "completed" && check.conclusion === "success" && check.details_url === details && check.external_id === external &&
              check.app && String(check.app.slug || "").toLowerCase() === "github-actions";
            if (!exact) throw new Error("created or recovered uninstall check did not match the exact immutable authorization");
            console.log("Published exact authorized uninstall check " + check.id + " for " + head);
          }
          main().catch((error) => { console.error(String(error)); process.exitCode = 1; });
          NODE

`
}

func setupAuthorizationWorkflowVerifierScript() string {
	return `          const child = require("child_process");
          const crypto = require("crypto");
          const fs = require("fs");

          const domain = "hive.setup-diff-tree-blobs.v1";
          const schema = "hive.setup-pr-authorization.v1";
          const diffAlgorithm = "hive.setup-diff-tree-blobs.v1+sha256";
          const contextPrefix = "hive/setup-authorized/";
          const sha40 = /^[a-f0-9]{40}$/u;
          const digest64 = /^[a-f0-9]{64}$/u;
          const oidPattern = /^[a-f0-9]{40}(?:[a-f0-9]{24})?$/u;
          const baselinePattern = /^\.visual-hive\/snapshots\/(?:[^/]+\/)*[^/]+\.png$/u;
          let required = JSON.parse(process.env.HIVE_REQUIRED_FILES || "[]").sort();
          let absent = JSON.parse(process.env.HIVE_REQUIRED_ABSENT || "[]").sort();
          let allowed = new Set(JSON.parse(process.env.HIVE_ALLOWED_FILES || "[]"));

          function output(authorized, context, bindingDigest, diffDigest, operation) {
            const rows = [
              "authorized=" + (authorized ? "true" : "false"),
              "context=" + (context || ""),
              "binding_digest=" + (bindingDigest || ""),
              "diff_digest=" + (diffDigest || ""),
              "operation=" + (operation || "")
            ];
            fs.appendFileSync(process.env.GITHUB_OUTPUT, rows.join("\n") + "\n");
          }

          function git(args) {
            const result = child.spawnSync("git", args, {
              encoding: null,
              maxBuffer: 256 * 1024 * 1024,
              env: { ...process.env, LC_ALL: "C", LANG: "C", GIT_CONFIG_NOSYSTEM: "1", GIT_ATTR_NOSYSTEM: "1", GIT_NO_REPLACE_OBJECTS: "1" }
            });
            if (result.status !== 0) throw new Error("git " + args.join(" ") + " failed");
            return Buffer.from(result.stdout || []);
          }

          function u64(value) {
            const result = Buffer.alloc(8);
            result.writeBigUInt64BE(BigInt(value));
            return result;
          }

          function put(hasher, value) {
            const data = Buffer.isBuffer(value) ? value : Buffer.from(String(value), "utf8");
            hasher.update(u64(data.length));
            hasher.update(data);
          }

          function readNul(raw, state) {
            const end = raw.indexOf(0, state.offset);
            if (end < 0) throw new Error("canonical diff ended inside a NUL record");
            const value = raw.subarray(state.offset, end);
            state.offset = end + 1;
            return value;
          }

          function parseDiff(raw) {
            const state = { offset: 0 };
            const records = [];
            while (state.offset < raw.length) {
              const header = readNul(raw, state).toString("ascii").trim().split(/\s+/u);
              const filePath = readNul(raw, state);
              if (header.length !== 5 || !header[0].startsWith(":") || header[0].length !== 7 || header[1].length !== 6 || !/^[ADMTUX]$/u.test(header[4])) throw new Error("malformed canonical diff-tree record");
              const oldOID = header[2];
              const newOID = header[3];
              if (!oidPattern.test(oldOID) || !oidPattern.test(newOID)) throw new Error("malformed canonical object ID");
              const pathText = filePath.toString("utf8");
              if (!pathText || Buffer.from(pathText, "utf8").compare(filePath) !== 0 || pathText.includes("\\") || pathText.includes("\0") || pathText.startsWith("/") || pathText.split("/").some((part) => !part || part === "." || part === "..")) throw new Error("unsafe canonical diff path");
              records.push({ status: header[4], oldMode: header[0].slice(1), newMode: header[1], oldOID, newOID, path: filePath, pathText });
            }
            records.sort((left, right) => Buffer.compare(left.path, right.path));
            for (let index = 1; index < records.length; index++) if (Buffer.compare(records[index - 1].path, records[index].path) === 0) throw new Error("duplicate canonical diff path");
            return records;
          }

          function hashObject(hasher, mode, oid) {
            if (mode === "000000" || /^0+$/u.test(oid)) {
              put(hasher, "missing");
              hasher.update(u64(0));
              return;
            }
            if (mode === "160000") {
              put(hasher, "gitlink");
              hasher.update(u64(0));
              return;
            }
            if (!["100644", "100755", "120000"].includes(mode)) throw new Error("unsupported Git mode " + mode);
            if (git(["cat-file", "-t", oid]).toString("utf8").trim() !== "blob") throw new Error("canonical object is not a blob");
            const data = git(["cat-file", "blob", oid]);
            const declared = Number(git(["cat-file", "-s", oid]).toString("utf8").trim());
            if (!Number.isSafeInteger(declared) || declared !== data.length) throw new Error("canonical blob size mismatch");
            put(hasher, "blob");
            hasher.update(u64(data.length));
            hasher.update(data);
          }

          function diffEvidence(baseSHA, headSHA) {
            git(["cat-file", "-e", baseSHA + "^{commit}"]);
            git(["cat-file", "-e", headSHA + "^{commit}"]);
            const records = parseDiff(git(["diff-tree", "--no-commit-id", "--raw", "-r", "-z", "--no-renames", baseSHA, headSHA, "--"]));
            if (records.length === 0) throw new Error("setup authorization refuses an empty diff");
            const hasher = crypto.createHash("sha256");
            put(hasher, domain);
            hasher.update(u64(records.length));
            for (const record of records) {
              for (const value of [record.status, record.oldMode, record.newMode, record.oldOID, record.newOID, record.path]) put(hasher, value);
              hashObject(hasher, record.oldMode, record.oldOID);
              hashObject(hasher, record.newMode, record.newOID);
            }
            return { digest: hasher.digest("hex"), records };
          }

          function treeEntry(headSHA, filePath) {
            const raw = git(["ls-tree", "-z", headSHA, "--", ":(literal)" + filePath]);
            if (raw.length === 0) return null;
            if (raw[raw.length - 1] !== 0 || raw.subarray(0, raw.length - 1).includes(0)) throw new Error("ambiguous required setup path " + filePath);
            const row = raw.subarray(0, raw.length - 1);
            const tab = row.indexOf(9);
            if (tab < 0 || row.subarray(tab + 1).toString("utf8") !== filePath) throw new Error("inexact required setup path " + filePath);
            const metadata = row.subarray(0, tab).toString("ascii").trim().split(/\s+/u);
            if (metadata.length !== 3 || !oidPattern.test(metadata[2])) throw new Error("malformed required setup tree entry");
            return { mode: metadata[0], type: metadata[1], oid: metadata[2] };
          }

          function requiredFiles(headSHA) {
            const files = [];
            for (const filePath of required) {
              const entry = treeEntry(headSHA, filePath);
              if (!entry || entry.mode !== "100644" || entry.type !== "blob") throw new Error("required setup file missing or not regular: " + filePath);
              const data = git(["cat-file", "blob", entry.oid]);
              const declared = Number(git(["cat-file", "-s", entry.oid]).toString("utf8").trim());
              if (!Number.isSafeInteger(declared) || declared !== data.length) throw new Error("required setup blob size mismatch");
              files.push({ path: filePath, mode: entry.mode, blob_oid: entry.oid, size: data.length, content_sha256: crypto.createHash("sha256").update(data).digest("hex") });
            }
            for (const filePath of absent) if (treeEntry(headSHA, filePath)) throw new Error("required absent managed path is present: " + filePath);
            return files;
          }

          async function latestStatus(repository, headSHA, expectedContext) {
            for (let page = 1; page <= 10; page++) {
              const response = await fetch(process.env.HIVE_API_URL + "/repos/" + repository + "/commits/" + headSHA + "/statuses?per_page=100&page=" + page, {
                headers: { "Accept": "application/vnd.github+json", "Authorization": "Bearer " + process.env.HIVE_STATUS_TOKEN, "X-GitHub-Api-Version": "2022-11-28" }
              });
              if (!response.ok) throw new Error("status lookup returned HTTP " + response.status);
              const statuses = await response.json();
              if (!Array.isArray(statuses)) throw new Error("status lookup returned a non-array");
              const matched = statuses.find((status) => String(status.context || "").toLowerCase() === expectedContext);
              if (matched) return matched;
              if (statuses.length < 100) return null;
            }
            throw new Error("status lookup exceeded its bounded page limit");
          }

          async function main() {
            let context = "";
            let bindingDigest = "";
            let diffDigest = "";
            let operation = "";
            try {
              const repository = String(process.env.HIVE_REPOSITORY || "").toLowerCase();
              const repositoryID = String(process.env.HIVE_REPOSITORY_ID || "");
              const pullRequest = Number(process.env.HIVE_PULL_REQUEST || "0");
              let authorizerID = String(process.env.HIVE_AUTHORIZER_ID || "");
              const transferAuthorizerID = String(process.env.HIVE_TRANSFER_AUTHORIZER_ID || "");
              const headSHA = String(process.env.HIVE_HEAD_SHA || "").toLowerCase();
              const baseSHA = String(process.env.HIVE_BASE_SHA || "").toLowerCase();
              const headRef = String(process.env.HIVE_HEAD_REF || "");
              const baseRef = String(process.env.HIVE_BASE_REF || "");
              if (!sha40.test(headSHA) || !sha40.test(baseSHA) || !/^[1-9][0-9]*$/u.test(repositoryID) || !/^[1-9][0-9]*$/u.test(authorizerID) || !/^[1-9][0-9]*$/u.test(transferAuthorizerID) || !Number.isSafeInteger(pullRequest) || pullRequest <= 0) throw new Error("invalid immutable setup event identity");
              if (baseRef !== process.env.HIVE_EXPECTED_BASE_REF || String(process.env.HIVE_HEAD_REPOSITORY || "").toLowerCase() !== repository || String(process.env.HIVE_BASE_REPOSITORY || "").toLowerCase() !== repository) throw new Error("pull request is not the expected same-repository managed operation");
              if (headRef === process.env.HIVE_EXPECTED_UNINSTALL_REF) {
                operation = "uninstall";
                required = JSON.parse(process.env.HIVE_UNINSTALL_REQUIRED_FILES || "[]").sort();
                absent = JSON.parse(process.env.HIVE_UNINSTALL_REQUIRED_ABSENT || "[]").sort();
                allowed = new Set(JSON.parse(process.env.HIVE_UNINSTALL_ALLOWED_FILES || "[]"));
              } else if (headRef === process.env.HIVE_EXPECTED_UPGRADE_REF) {
                operation = "upgrade";
              } else if (headRef === process.env.HIVE_EXPECTED_ROLLBACK_REF) {
                operation = "rollback";
              } else if (headRef === process.env.HIVE_EXPECTED_AUTHORIZER_TRANSFER_REF) {
                operation = "authorizer-transfer";
                authorizerID = transferAuthorizerID;
              } else if (headRef === process.env.HIVE_EXPECTED_HEAD_REF) {
                operation = "setup";
              } else {
                throw new Error("pull request is not the expected same-repository managed operation");
              }
              const diff = diffEvidence(baseSHA, headSHA);
              diffDigest = diff.digest;
              for (const record of diff.records) if (!allowed.has(record.pathText) && !(operation !== "uninstall" && baselinePattern.test(record.pathText))) throw new Error("unmanaged setup path " + record.pathText);
              const files = requiredFiles(headSHA);
              const binding = {
                schema_version: schema,
                repository,
                repository_id: repositoryID,
                pull_request: pullRequest,
                head_ref: headRef,
                head_sha: headSHA,
                base_ref: baseRef,
                base_sha: baseSHA,
                authorizer_id: authorizerID,
                diff_algorithm: diffAlgorithm,
                diff_sha256: diffDigest,
                required_files: files,
                required_absent: absent
              };
              bindingDigest = crypto.createHash("sha256").update(JSON.stringify(binding)).digest("hex");
              if (!digest64.test(bindingDigest)) throw new Error("invalid setup binding digest");
              context = contextPrefix + bindingDigest;
              const pollAttempts = Math.min(20, Math.max(1, Number(process.env.HIVE_SETUP_STATUS_POLL_ATTEMPTS || "8")));
              const pollDelay = Math.min(30000, Math.max(0, Number(process.env.HIVE_SETUP_STATUS_POLL_DELAY_MS || "5000")));
              let status = null;
              for (let attempt = 0; attempt < pollAttempts && !status; attempt++) {
                status = await latestStatus(repository, headSHA, context);
                if (!status && attempt + 1 < pollAttempts) await new Promise((resolve) => setTimeout(resolve, pollDelay));
              }
              const creator = status && status.creator;
              const exact = status && String(status.state || "").toLowerCase() === "success" && String(status.context || "").toLowerCase() === context &&
                creator && String(creator.id || "") === authorizerID && String(creator.type || "").toLowerCase() === "user" &&
                String(status.target_url || "") === String(process.env.HIVE_PULL_REQUEST_URL || "");
              if (!exact) throw new Error("latest exact-head setup status was absent or did not match creator/target/state");
              output(true, context, bindingDigest, diffDigest, operation);
            } catch (error) {
              console.log("Setup authorization not present: " + String(error && error.message ? error.message : error));
              output(false, context, bindingDigest, diffDigest, operation);
            }
          }

          main().catch((error) => {
            console.log("Setup authorization verifier failed closed: " + String(error));
            output(false, "", "", "", "");
          });`
}
