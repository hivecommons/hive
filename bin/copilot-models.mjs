#!/usr/bin/env node
// copilot-models.mjs — list the Copilot models available to this machine's
// Copilot auth, via the official @github/copilot-sdk.
//
// The SDK spawns the installed copilot CLI as a JSON-RPC server over stdio and
// asks it for its model catalog, so the probe rides the CLI's OWN auth
// resolution (stored device-flow OAuth under $HOME/.copilot, or the
// COPILOT_GITHUB_TOKEN env var) and the CLI's own TLS handling
// (NODE_EXTRA_CA_CERTS — required behind the egress proxy). That makes it work
// in auth configurations the dashboard's raw-HTTP /models probe cannot reach.
//
// Contract with the Go caller (src/pkg/dashboard/cli_models.go):
//   - success: exactly ONE line of JSON on stdout —
//       {"models":[{"id":...,"name":...,"policyState":...,"efforts":[...],"defaultEffort":...}]}
//     and exit code 0.
//   - failure: NOTHING on stdout, a short error on stderr, nonzero exit.
//   - never hangs: a hard internal deadline force-exits the process, and the
//     spawned copilot server is stopped in a finally + reaped by process exit.
//   - never prints tokens or environment values.

import { createRequire } from "node:module";
import { existsSync } from "node:fs";
import path from "node:path";

// ---- Constants ----

// Hard internal deadline for the whole probe (start + listModels + stop).
// The Go caller's exec timeout (copilotSDKProbeTimeout in cli_models.go) sits
// slightly ABOVE this so the helper always gets to report its own error
// instead of being killed mid-flight.
const INTERNAL_TIMEOUT_MS = 15_000;

// Process exit codes: 0 = model list on stdout, 1 = any failure (no stdout).
const EXIT_OK = 0;
const EXIT_FAIL = 1;

// Node entry of the PINNED copilot CLI installed globally by the image
// (src/Dockerfile Layer 3). The image deletes the CLI's native binary so the
// CLI runs its Node.js path (which honors NODE_EXTRA_CA_CERTS); pointing the
// SDK's runtime connection at THIS entry guarantees the probe runs the fleet's
// pinned CLI — never the newer @github/copilot the SDK bundles as its own
// nested dependency, whose native binary does not trust the proxy CA.
// The SDK spawns `node <entry> --server ...` when the path ends in ".js".
const PINNED_CLI_ENTRY = "/usr/local/lib/node_modules/@github/copilot/index.js";

// npm global roots to search for @github/copilot-sdk. ESM bare-specifier
// resolution NEVER consults the global node_modules, so a plain
// `import "@github/copilot-sdk"` fails for a globally-installed SDK; instead
// the SDK is loaded with createRequire() anchored inside the global root —
// the package ships a CJS build under its "require" export condition
// (dist/cjs/index.js), so require() works regardless of where this script
// lives. The execPath-derived candidate covers non-default npm prefixes.
const GLOBAL_NODE_MODULES_CANDIDATES = [
  "/usr/local/lib/node_modules",
  path.join(path.dirname(process.execPath), "..", "lib", "node_modules"),
];

// Anchor filename for createRequire inside a node_modules root. The file need
// not exist — createRequire only uses it to derive resolution paths.
const REQUIRE_ANCHOR_BASENAME = "hive-require-anchor.js";

const SDK_PACKAGE_NAME = "@github/copilot-sdk";

// ---- Failure-mode plumbing ----

function fail(message) {
  process.stderr.write(`copilot-models: ${message}\n`);
  process.exit(EXIT_FAIL);
}

// Watchdog: force-exit on deadline no matter what the SDK is doing. Not
// unref()ed on purpose — a successful run exits explicitly before it fires.
setTimeout(() => {
  fail(`timed out after ${INTERNAL_TIMEOUT_MS}ms`);
}, INTERNAL_TIMEOUT_MS);

// ---- SDK loading ----

function loadSdk() {
  // Local resolution first (works when a node_modules ancestor has the SDK,
  // e.g. dev checkouts), then the global roots.
  const localRequire = createRequire(import.meta.url);
  const anchors = [null, ...GLOBAL_NODE_MODULES_CANDIDATES];
  const errors = [];
  for (const root of anchors) {
    try {
      const req = root === null
        ? localRequire
        : createRequire(path.join(root, REQUIRE_ANCHOR_BASENAME));
      return req(SDK_PACKAGE_NAME);
    } catch (err) {
      errors.push(err?.code || err?.message || String(err));
    }
  }
  throw new Error(`cannot load ${SDK_PACKAGE_NAME} (${errors.join("; ")})`);
}

// ---- Main ----

async function main() {
  const { CopilotClient, RuntimeConnection } = loadSdk();

  // Honor an explicit COPILOT_CLI_PATH override, else pin to the image's CLI
  // entry when present, else let the SDK resolve (dev machines).
  const cliPath =
    process.env.COPILOT_CLI_PATH ||
    (existsSync(PINNED_CLI_ENTRY) ? PINNED_CLI_ENTRY : undefined);
  const options = {};
  if (cliPath) {
    options.connection = RuntimeConnection.forStdio({ path: cliPath });
  }
  // When a token is present, hand it to the SDK explicitly (the documented
  // path for token auth, hivecommons/hive#2519); when absent, the CLI
  // resolves its stored logged-in auth ($HOME/.copilot) on its own.
  if (process.env.COPILOT_GITHUB_TOKEN) {
    options.gitHubToken = process.env.COPILOT_GITHUB_TOKEN;
  }

  const client = new CopilotClient(options);
  try {
    await client.start();
    const models = await client.listModels();
    const out = {
      models: (models || []).map((m) => ({
        id: m.id,
        name: m.name,
        policyState: m.policy?.state,
        efforts: m.supportedReasoningEfforts,
        defaultEffort: m.defaultReasoningEffort,
      })),
    };
    process.stdout.write(JSON.stringify(out) + "\n");
  } finally {
    // Best-effort server shutdown; the explicit process.exit below reaps
    // anything a failed stop leaves behind so no copilot server lingers.
    try {
      await client.stop();
    } catch {
      // ignored — see comment above
    }
  }
}

main().then(
  () => process.exit(EXIT_OK),
  (err) => fail(err?.message || String(err)),
);
