// hivecommons/hive#7365 — the dashboard's Copilot model dropdown served a
// legacy chat-completions catalog (gpt-4o, gpt-3.5-turbo, …) while the CLI
// offered the real agent catalog (Claude Opus 5, GPT-5.6 Sol, …).
//
// Cause: bin/copilot-models.mjs pinned the SDK's runtime connection to
// "<pkg>/index.js", a file @github/copilot has never shipped — not in 1.0.78,
// not in 1.0.59. The package contains exactly four files. So existsSync() was
// always false, no cliPath was ever passed, and the SDK fell back to resolving
// a platform package from its OWN directory, which cannot see the one nested
// under @github/copilot/node_modules. Probe #1 failed every cycle on every
// spoke and discovery silently fell through to the raw-HTTP catalog.
//
// These tests use REAL directories rather than mocked fs, so they exercise the
// same path.resolve/existsSync behaviour the image does.

import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, symlinkSync, rmSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { resolveCopilotCliEntry } from "../../bin/copilot-models.mjs";

// The exact file list shipped by @github/copilot@1.0.78, verified with
// `npm pack @github/copilot@1.0.78 && tar tzf`. Note the absence of index.js.
const REAL_PACKAGE_FILES = ["npm-loader.js", "package.json", "LICENSE.md", "README.md"];

function makeTempRoot(t) {
  const root = mkdtempSync(path.join(tmpdir(), "hive-7365-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return root;
}

// Builds a package dir that mirrors a real install. `bin` mirrors the real
// manifest's { "copilot": "npm-loader.js" }.
function makePackageDir(root, { files = REAL_PACKAGE_FILES, bin = { copilot: "npm-loader.js" } } = {}) {
  const dir = path.join(root, "lib", "node_modules", "@github", "copilot");
  mkdirSync(dir, { recursive: true });
  for (const f of files) {
    if (f === "package.json") continue;
    writeFileSync(path.join(dir, f), "");
  }
  if (files.includes("package.json")) {
    writeFileSync(path.join(dir, "package.json"), JSON.stringify({ name: "@github/copilot", bin }));
  }
  return dir;
}

// THE regression test. A package dir holding exactly what 1.0.78 ships must
// resolve. The pre-fix code looked only for index.js and returned undefined.
test("#7365: resolves an entry from a package containing exactly the real 1.0.78 file set", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root);

  const entry = resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") });

  assert.equal(entry, path.join(dir, "npm-loader.js"));
  assert.ok(!entry.endsWith("index.js"), "must not resolve to index.js — that file does not exist upstream");
});

// The manifest is the mechanism that survives the NEXT rename. If upstream
// renames the entry again, reading bin keeps working with no code change —
// which is the whole point of the fix, rather than swapping one constant.
test("#7365: prefers the package's own package.json bin field over hardcoded basenames", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root, {
    files: ["npm-loader.js", "package.json", "future-entry.js"],
    bin: { copilot: "future-entry.js" },
  });

  const entry = resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") });

  assert.equal(entry, path.join(dir, "future-entry.js"),
    "a renamed upstream entry must be picked up from the manifest, not ignored in favour of a known basename");
});

test("#7365: accepts a string bin field as well as an object", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root, { files: ["npm-loader.js", "package.json"], bin: "npm-loader.js" });

  assert.equal(
    resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") }),
    path.join(dir, "npm-loader.js"),
  );
});

// A manifest that names a file which is not there must not win — otherwise the
// helper hands the SDK a path that cannot spawn, which is a worse failure than
// the one #7365 fixed.
test("#7365: falls back to a known basename when the manifest names a missing file", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root, {
    files: ["npm-loader.js", "package.json"],
    bin: { copilot: "gone.js" },
  });

  assert.equal(
    resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") }),
    path.join(dir, "npm-loader.js"),
  );
});

test("#7365: falls back to a known basename when package.json is unreadable", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root, { files: ["npm-loader.js"] }); // no package.json at all

  assert.equal(
    resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") }),
    path.join(dir, "npm-loader.js"),
  );
});

// Dev checkouts have no pinned CLI. Letting the SDK resolve its own bundled CLI
// is correct there, so this must stay undefined rather than throwing.
test("#7365: returns undefined when the pinned package is not installed", () => {
  assert.equal(
    resolveCopilotCliEntry({ env: {}, packageDir: "/nonexistent/hive-7365", binSymlink: "/nonexistent/copilot" }),
    undefined,
  );
});

test("#7365: COPILOT_CLI_PATH overrides everything", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root);

  assert.equal(
    resolveCopilotCliEntry({ env: { COPILOT_CLI_PATH: "/custom/entry.js" }, packageDir: dir }),
    "/custom/entry.js",
  );
});

// The loud-failure requirement. #7365 stayed hidden because an unusable pinned
// CLI was indistinguishable from "not in the image": both produced undefined,
// both handed off to the SDK, both surfaced as the SDK's misleading
// "platform package not found".
test("#7365: throws when the package is installed but has no runnable entry", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root, { files: ["README.md"] }); // installed, but nothing runnable

  assert.throws(
    () => resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") }),
    (err) => {
      assert.match(err.message, /installed at/);
      assert.match(err.message, /no runnable entry/);
      assert.match(err.message, /COPILOT_CLI_PATH/, "the error must name the operator's escape hatch");
      return true;
    },
    "an installed-but-unusable CLI must be loud, not silently downgraded to the HTTP catalog",
  );
});

// The image's /usr/local/bin/copilot is a symlink to the real entry. It is the
// last resort, and it must be resolved through realpath: the SDK only spawns
// `node <entry>` when the path ends in ".js", so handing it the bare "copilot"
// symlink path would have it exec the file directly instead.
test("#7365: resolves the bin symlink to its real .js target as a last resort", (t) => {
  const root = makeTempRoot(t);
  // Package dir exists but holds no recognised entry, forcing the symlink path.
  const dir = makePackageDir(root, { files: ["README.md"] });
  const realEntry = path.join(dir, "actual-loader.js");
  writeFileSync(realEntry, "");
  const binDir = path.join(root, "bin");
  mkdirSync(binDir, { recursive: true });
  const link = path.join(binDir, "copilot");
  symlinkSync(realEntry, link);

  const entry = resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: link });

  // Compare against the realpath of the target, not the constructed path: on
  // macOS the tmpdir itself sits behind a /var -> /private/var symlink, so the
  // resolved entry is correct but spelled differently.
  assert.equal(entry, realpathSync(realEntry));
  assert.ok(entry.endsWith(".js"), "the SDK only spawns `node <entry>` for a .js path");
});

// Every resolution route must end in .js for the same reason.
test("#7365: every resolved entry ends in .js so the SDK spawns it under node", (t) => {
  const root = makeTempRoot(t);
  const dir = makePackageDir(root);

  const entry = resolveCopilotCliEntry({ env: {}, packageDir: dir, binSymlink: path.join(root, "absent") });
  assert.ok(entry.endsWith(".js"), `resolved ${entry}, which the SDK would exec directly rather than run under node`);
});
