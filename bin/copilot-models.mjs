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
import { existsSync, readFileSync, realpathSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

// ---- Constants ----

// Hard internal deadline for the whole probe (start + listModels + stop).
// The Go caller's exec timeout (copilotSDKProbeTimeout in cli_models.go) sits
// slightly ABOVE this so the helper always gets to report its own error
// instead of being killed mid-flight.
const INTERNAL_TIMEOUT_MS = 15_000;

// Process exit codes: 0 = model list on stdout, 1 = any failure (no stdout).
const EXIT_OK = 0;
const EXIT_FAIL = 1;

// Install directory of the PINNED copilot CLI (src/Dockerfile Layer 3).
// Pointing the SDK's runtime connection at this package's entry guarantees the
// probe runs the fleet's pinned CLI — never the newer @github/copilot the SDK
// nests as its own dependency, whose native binary does not trust the proxy CA.
const COPILOT_PACKAGE_DIR = "/usr/local/lib/node_modules/@github/copilot";

// Entry filenames to try when the package manifest does not name one. Ordered
// newest-first. THIS LIST IS A FALLBACK, NOT THE PRIMARY MECHANISM — see
// resolveCopilotCliEntry, which reads the package's own "bin" field first.
//
// hivecommons/hive#7365: this file previously hardcoded a single entry,
// "index.js", which @github/copilot has never shipped — not in 1.0.78, not in
// 1.0.59. The package contains exactly four files (npm-loader.js,
// package.json, LICENSE.md, README.md). So the existsSync() guard below was
// always false, cliPath was always undefined, and the SDK fell back to
// resolving a platform package from ITS OWN directory — which cannot see the
// one nested under @github/copilot/node_modules. Every discovery cycle on
// every spoke failed with "Could not find a @github/copilot platform package",
// and the dashboard silently served the legacy chat-completions catalog
// instead of the CLI's real one.
const CLI_ENTRY_BASENAMES = ["npm-loader.js", "index.js"];

// The image's stable CLI symlink, used as a last resort. Resolved through
// realpath because the SDK only spawns `node <entry>` when the path ends in
// ".js"; a bare "copilot" path would be exec'd directly instead.
const CLI_BIN_SYMLINK = "/usr/local/bin/copilot";

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

// isDirectRun reports whether this module was executed as the process entry
// (`node copilot-models.mjs`) rather than imported. The watchdog and main() are
// side effects that must NOT fire on import, so the unit tests can exercise
// resolveCopilotCliEntry without spawning a copilot server or arming a timer.
function isDirectRun() {
  const entry = process.argv[1];
  if (!entry) return false;
  try {
    return import.meta.url === pathToFileURL(entry).href;
  } catch {
    return false;
  }
}

// Watchdog: force-exit on deadline no matter what the SDK is doing. Not
// unref()ed on purpose — a successful run exits explicitly before it fires.
if (isDirectRun()) {
  setTimeout(() => {
    fail(`timed out after ${INTERNAL_TIMEOUT_MS}ms`);
  }, INTERNAL_TIMEOUT_MS);
}

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

// ---- CLI entry resolution (hivecommons/hive#7365) ----

// resolveCopilotCliEntry decides which copilot CLI entry the SDK should drive.
//
// Resolution order:
//   1. COPILOT_CLI_PATH — explicit operator override, honored verbatim.
//   2. the package's OWN package.json "bin" field, joined to the package dir.
//      This is the mechanism that survives upstream renaming its entry, which
//      is exactly what #7365 was: a hardcoded filename that silently stopped
//      matching. Reading the manifest means the next rename costs nothing.
//   3. known entry basenames, newest-first.
//   4. realpath of the image's /usr/local/bin/copilot symlink.
//
// Returns undefined when the pinned package is not installed at all — a dev
// checkout, where letting the SDK resolve its own bundled CLI is correct.
//
// Throws when the package IS installed but no entry can be found. That case is
// an image that will never produce a working probe, and #7365 is the argument
// for making it loud: the old code treated it as "no pinned CLI", handed off to
// the SDK, and the resulting failure looked identical to "not in the image".
export function resolveCopilotCliEntry(deps = {}) {
  const {
    env = process.env,
    exists = existsSync,
    readFile = readFileSync,
    realpath = realpathSync,
    packageDir = COPILOT_PACKAGE_DIR,
    binSymlink = CLI_BIN_SYMLINK,
  } = deps;

  if (env.COPILOT_CLI_PATH) return env.COPILOT_CLI_PATH;

  // Not the image (or the CLI layer failed to install): let the SDK resolve.
  if (!exists(packageDir)) return undefined;

  const tried = [];

  // (2) The manifest's own bin field.
  const manifestPath = path.join(packageDir, "package.json");
  try {
    const manifest = JSON.parse(readFile(manifestPath, "utf8"));
    const bin = manifest?.bin;
    const rel = typeof bin === "string" ? bin : bin?.copilot;
    if (rel) {
      const entry = path.resolve(packageDir, rel);
      if (exists(entry)) return entry;
      tried.push(`${entry} (from package.json bin)`);
    }
  } catch (err) {
    tried.push(`${manifestPath} unreadable (${err?.code || err?.message})`);
  }

  // (3) Known basenames.
  for (const base of CLI_ENTRY_BASENAMES) {
    const entry = path.join(packageDir, base);
    if (exists(entry)) return entry;
    tried.push(entry);
  }

  // (4) The image's stable symlink, resolved to its real .js target.
  try {
    if (exists(binSymlink)) {
      const real = realpath(binSymlink);
      if (real.endsWith(".js") && exists(real)) return real;
      tried.push(`${binSymlink} -> ${real} (not a .js entry)`);
    }
  } catch (err) {
    tried.push(`${binSymlink} unresolvable (${err?.code || err?.message})`);
  }

  throw new Error(
    `the pinned copilot CLI is installed at ${packageDir} but no runnable entry was found ` +
      `(tried: ${tried.join("; ")}). Set COPILOT_CLI_PATH to override.`,
  );
}

// ---- Main ----

async function main() {
  const { CopilotClient, RuntimeConnection } = loadSdk();

  // Resolve the CLI entry the SDK should drive (#7365). Throws when the pinned
  // package is present but unusable — main()'s catch reports it on stderr and
  // exits nonzero, so the Go caller logs a precise cause instead of the SDK's
  // misleading "platform package not found".
  const cliPath = resolveCopilotCliEntry();
  const options = {};
  if (cliPath) {
    options.connection = RuntimeConnection.forStdio({ path: cliPath });
  }
  // When a token is present, hand it to the SDK explicitly (the documented
  // path for token auth, kubestellar/hive#2519); when absent, the CLI
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

if (isDirectRun()) {
  main().then(
    () => process.exit(EXIT_OK),
    (err) => fail(err?.message || String(err)),
  );
}
