#!/usr/bin/env python3
"""Compare hive's static CLI model lists against public, keyless sources.

Run weekly by .github/workflows/static-model-freshness.yml. Prints a Markdown
drift report to stdout (empty body sections are omitted) and exits 0; the
workflow opens or updates ONE tracking issue with it.

Sources (all public, no auth):
  * OpenRouter GET https://openrouter.ai/api/v1/models. Used for:
      - the curated OpenRouter suggestions (crux/openrouter SuggestedModels and
        DefaultModel), which ARE OpenRouter ids: a missing id is drift;
      - the Claude, Gemini and OpenAI/Codex/Copilot static fallbacks, mapped
        onto their OpenRouter equivalents (anthropic/, google/, openai/). These
        are NOT the CLIs' own catalogs, so a miss is a hint to look, and a
        newer model in the same family that the static list lacks is reported
        as "newer upstream".
  * npm registry (registry.npmjs.org/<pkg>/latest) for the CLIs that ship as
    npm packages: the latest published version, as context for whether a
    CLI release may have changed its model list since the snapshot.

Not covered (need auth or have no machine-readable list): the Claude CLI's
own /v1/models (OAuth), the Copilot SDK catalog (Copilot login), the Codex
app-server catalog (ChatGPT login), Gemini CLI's internal list, Kiro (pi),
agy, omp, goose providers and bob. Those are probed live at runtime by the
dashboard; their static lists are fallbacks and are checked only for shape
(non-empty, no duplicates, Go/JS in sync) by the Go unit tests.
"""
import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
import urllib.request

OPENROUTER_MODELS = "https://openrouter.ai/api/v1/models"
NPM_LATEST = "https://registry.npmjs.org/{pkg}/latest"
TIMEOUT_S = 30
USER_AGENT = "hive-static-model-freshness"

# Package names of CLIs whose model lists hive snapshots.
NPM_PACKAGES = {
    "claude": "@anthropic-ai/claude-code",
    "copilot": "@github/copilot",
    "codex": "@openai/codex",
    "gemini": "@google/gemini-cli",
}

CLI_MODELS_GO = "src/pkg/dashboard/cli_models.go"
OPENROUTER_GO = "src/crux/openrouter/openrouter.go"

# Static Go lists checked against OpenRouter, with the OpenRouter provider
# prefix their ids map onto.
GO_LISTS = {
    "claudeStaticModels": "anthropic",
    "copilotStaticModels": None,  # mixed vendors; prefix inferred per id
    "codexStaticModels": "openai",
    "geminiStaticModels": "google",
}

# Bare CLI aliases, never compared against a catalogue: Claude's
# opus/sonnet/haiku/fable and the Gemini CLI's auto/pro/flash/flash-lite.
ALIASES = {"opus", "sonnet", "haiku", "fable", "auto", "default", "pro", "flash", "flash-lite"}


def fetch_json(url):
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=TIMEOUT_S) as resp:
        return json.load(resp)


def go_string_list(src, name):
    m = re.search(r"var " + re.escape(name) + r" = \[\]string\{(.*?)\n\}", src, re.S)
    if not m:
        return []
    body = re.sub(r"//[^\n]*", "", m.group(1))
    return re.findall(r'"([^"]+)"', body)


def openrouter_suggestions(src):
    ids = re.findall(r'\{ID: "([^"]+)", Label:', src)
    m = re.search(r'const DefaultModel = "([^"]+)"', src)
    return ids, (m.group(1) if m else "")


def vendor_for(model_id):
    if model_id.startswith("claude-"):
        return "anthropic"
    if model_id.startswith("gemini-"):
        return "google"
    if model_id.startswith(("gpt-", "o3", "o4", "codex-")):
        return "openai"
    return None


def norm(model_id):
    """Canonical comparison key: no date suffix, dots and dashes unified."""
    s = model_id.lower().split("/")[-1].split(":")[0]
    s = re.sub(r"-\d{8}$", "", s)
    return s.replace(".", "-")


def family(key):
    """(family, version) where family is the name around the version.

    claude-sonnet-5-5 -> (('claude-sonnet', ''), (5, 5));
    gemini-2-5-flash  -> (('gemini', 'flash'), (2, 5)). Variants with a
    different tail (flash-lite, flash-image) are different families, so only
    a genuinely newer version of the same line is reported.
    """
    m = re.match(r"^([a-z]+(?:-[a-z]+)*?)-(\d+(?:-\d+)*)(?:-(.*))?$", key)
    if not m:
        return None, ()
    return (m.group(1), m.group(3) or ""), tuple(int(x) for x in m.group(2).split("-"))


def index_catalog(catalog):
    """{vendor: {normalised key}} for every plain OpenRouter id."""
    by_vendor = {}
    for m in catalog:
        mid = m.get("id") or ""
        if "/" not in mid or mid.startswith("~"):
            continue
        vendor, rest = mid.split("/", 1)
        by_vendor.setdefault(vendor, set()).add(norm(rest))
    return by_vendor


def compare_list(ids, prefix, by_vendor):
    """The comparison behind the report, for one static list.

    Returns (not_found, newer): the list's ids with no catalog counterpart,
    and the (vendor, key) pairs the catalog has in the same family at a
    higher version than the list's newest entry of that family.
    """
    not_found, newer = [], set()
    have = {norm(i) for i in ids}
    fams = {}
    for i in ids:
        vendor = prefix or vendor_for(i)
        if not vendor:
            continue
        key = norm(i)
        if key not in by_vendor.get(vendor, set()):
            not_found.append(i)
        fam, ver = family(key)
        if fam:
            fams.setdefault((vendor, fam), max(fams.get((vendor, fam), ()), ver))
    for (vendor, fam), top in fams.items():
        for key in by_vendor.get(vendor, set()):
            f2, v2 = family(key)
            if f2 == fam and v2 > top and key not in have:
                newer.add((vendor, key))
    return not_found, newer


# ---- --apply claude: add newer Claude models the pinned CLI accepts (#18754) ----
#
# Run weekly by .github/workflows/claude-model-freshness.yml. The candidates
# are the same comparison the report makes (compare_list over
# claudePinnedCLIModels and claudeStaticModels against OpenRouter's anthropic/
# ids), narrowed to the four Claude lines. Each candidate is then put to the
# Claude Code version pinned in src/Dockerfile, offline and without
# credentials: an unauthenticated `claude --model <id> -p x` prints an
# `unrecognized_model` diagnostic for an id the CLI's built-in model table
# does not know, and nothing of the kind for one it does, before it stops at
# "Not logged in". The "version X or newer is required" refusal (#8417) comes
# from the API after login, so it cannot be probed without credentials. Two
# controls run first (a listed id must be recognised, a made-up id must not);
# if either fails, the CLI's output can no longer be read and every candidate
# goes under "needs a check" instead of being added.

CLAUDE_LINES = ("fable", "opus", "sonnet", "haiku")
CLAUDE_KEY = re.compile(r"^claude-(" + "|".join(CLAUDE_LINES) + r")-(\d+(?:-\d+)*)$")
CLAUDE_GO_LISTS = ("claudePinnedCLIModels", "claudeStaticModels")
CLAUDE_JS_CONST = "CLAUDE_CLI_MODELS"
BACKENDS_JS = "src/pkg/dashboard/static-src/js/backends-tokens.js"
REQUIREMENTS = "src/scripts/cli-model-requirements.txt"
DOCKERFILE = "src/Dockerfile"
PROBE_NEGATIVE = "claude-hive-probe-0-0"
PROBE_TIMEOUT_S = 90
UNRECOGNIZED_MARKERS = ("unrecognized_model", "doesn't recognize the model")
REFUSED = re.compile(r"version \S+ or newer is required", re.I)
GO_ID = re.compile(r'^\s*"([^"]+)",')
JS_ID = re.compile(r"^\s*\{\s*value:\s*'([^']+)'")


def claude_line(key):
    """('fable', (5, 2)) for claude-fable-5-2; None for anything else."""
    m = CLAUDE_KEY.match(key)
    if not m:
        return None
    return m.group(1), tuple(int(x) for x in m.group(2).split("-"))


def claude_label(line, version):
    return f"{line.capitalize()} {'.'.join(str(v) for v in version)}"


def retired(model):
    """OpenRouter sets expiration_date on a model it has scheduled for
    withdrawal; one already retired or on its way out is never added."""
    return bool(model.get("expiration_date"))


def claude_candidates(cli_src, catalog):
    """Claude ids on OpenRouter newer than the lists' newest of the same line.

    Mythos (limited access), retired ids, variants (":thinking") and router
    aliases ("~anthropic/...") are never candidates. Each candidate carries
    the Claude CLI's dashed spelling: anthropic/claude-fable-5.2 ->
    claude-fable-5-2.
    """
    listed = []
    for name in CLAUDE_GO_LISTS:
        listed += [i for i in go_string_list(cli_src, name) if i not in ALIASES]
    usable = [m for m in catalog
              if (m.get("id") or "").startswith("anthropic/")
              and ":" not in m["id"]
              and "mythos" not in m["id"].lower()
              and not retired(m)]
    source = {norm(m["id"]): m["id"] for m in usable}
    _, newer = compare_list(listed, "anthropic", index_catalog(usable))
    out = []
    for _, key in newer:
        lv = claude_line(key)
        if not lv:
            continue
        out.append({"source_id": source[key], "cli_id": key, "line": lv[0],
                    "version": lv[1], "label": claude_label(*lv)})
    return sorted(out, key=lambda c: (CLAUDE_LINES.index(c["line"]), c["version"]))


def probe_claude(claude_bin, model):
    """Classify the pinned CLI's offline answer for one --model value."""
    with tempfile.TemporaryDirectory(prefix="claude-probe-") as home:
        # Only PATH and an empty HOME: no API key, no OAuth token, no config.
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": home, "LANG": "C.UTF-8"}
        try:
            proc = subprocess.run([claude_bin, "--model", model, "-p", "x"], cwd=home, env=env,
                                  stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT, text=True, timeout=PROBE_TIMEOUT_S)
        except (OSError, subprocess.TimeoutExpired) as exc:
            return "error", str(exc)
    out = proc.stdout or ""
    if any(marker in out for marker in UNRECOGNIZED_MARKERS):
        return "unrecognized", out
    if REFUSED.search(out):
        return "refused", out
    return "recognized", out


def claude_version(claude_bin):
    """The binary's `--version` output, or the error running it."""
    try:
        return subprocess.run([claude_bin, "--version"], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True, timeout=PROBE_TIMEOUT_S).stdout.strip()
    except (OSError, subprocess.TimeoutExpired) as exc:
        return f"error: {exc}"


def gate_candidates(candidates, claude_bin, known_id, pin, probe=probe_claude):
    """Split candidates into (accepted, needs_check, note)."""
    if not candidates:
        return [], [], "no candidates"
    if not claude_bin:
        note = "no pinned Claude Code binary was available to probe"
        return [], [(c, note) for c in candidates], note
    version = claude_version(claude_bin)
    if not re.match(re.escape(pin) + r"\b", version):
        note = f"the binary to probe reports `{version[:80]}`, not the pinned `{pin}`"
        return [], [(c, note) for c in candidates], note
    pos, _ = probe(claude_bin, known_id)
    neg, _ = probe(claude_bin, PROBE_NEGATIVE)
    if pos != "recognized" or neg != "unrecognized":
        note = (f"probe controls failed (listed id `{known_id}` -> {pos}, made-up id `{PROBE_NEGATIVE}` -> {neg}), "
                "so the pinned CLI's offline answer cannot be trusted this run")
        return [], [(c, note) for c in candidates], note
    accepted, needs = [], []
    for c in candidates:
        verdict, out = probe(claude_bin, c["cli_id"])
        if verdict == "recognized":
            accepted.append(c)
        elif verdict == "unrecognized":
            needs.append((c, "the pinned Claude Code does not recognise this id"))
        elif verdict == "refused":
            needs.append((c, "the pinned Claude Code refuses it: " + REFUSED.search(out).group(0)))
        else:
            needs.append((c, "probe error: " + out.strip()[:200]))
    note = (f"each id probed with the pinned CLI, unauthenticated; controls passed (listed `{known_id}` recognised, "
            f"made-up `{PROBE_NEGATIVE}` not)")
    return accepted, needs, note


def insert_listed(lines, cli_id, render, id_re):
    """Insert render(indent) into a list body for cli_id; None when present.

    Placement: before the first entry of the same Claude line with a lower
    version, else after that line's last entry, else before the bare aliases
    (and the comment above them), else at the end. Go and JS lists get the
    same placement, so lists that matched before still match.
    """
    ids = [(m.group(1) if (m := id_re.match(ln)) else None) for ln in lines]
    if cli_id in ids:
        return None
    line, version = claude_line(cli_id)
    same = []
    for i, x in enumerate(ids):
        lv = claude_line(norm(x)) if x else None
        if lv and lv[0] == line:
            same.append((i, lv[1]))
    at = next((i for i, v in same if v < version), None)
    if at is None and same:
        at = same[-1][0] + 1
    if at is None:
        at = next((i for i, x in enumerate(ids) if x in ALIASES), len(lines))
        while 0 < at < len(lines) and lines[at - 1].strip().startswith("//"):
            at -= 1
    sample = next((lines[i] for i, x in enumerate(ids) if x), "\t")
    indent = re.match(r"\s*", sample).group(0)
    return lines[:at] + [render(indent)] + lines[at:]


def edit_block(src, pattern, cli_id, render, id_re):
    """Apply insert_listed to the list body the pattern's group 2 spans."""
    m = re.search(pattern, src, re.S)
    if not m:
        raise ValueError(f"list not found: {pattern}")
    new = insert_listed(m.group(2).split("\n"), cli_id, render, id_re)
    if new is None:
        return src, False
    return src[:m.start(2)] + "\n".join(new) + src[m.end(2):], True


def apply_claude(root, catalog, claude_bin, dry_run):
    """Gate the candidates and make the list edits. Returns (report, accepted)."""
    def path(rel):
        return os.path.join(root, rel)

    def read(rel):
        with open(path(rel), encoding="utf-8") as f:
            return f.read()

    go_src, js_src, reqs = read(CLI_MODELS_GO), read(BACKENDS_JS), read(REQUIREMENTS)
    pin_m = re.search(r"^ARG CLAUDE_CODE_VERSION=(\S+)", read(DOCKERFILE), re.M)
    if not pin_m:
        raise ValueError(f"no ARG CLAUDE_CODE_VERSION in {DOCKERFILE}")
    pin = pin_m.group(1)
    known = next(i for i in go_string_list(go_src, "claudePinnedCLIModels") if i not in ALIASES)
    candidates = claude_candidates(go_src, catalog)
    accepted, needs, note = gate_candidates(candidates, claude_bin, known, pin)

    added = []
    for c in accepted:
        cid, edits = c["cli_id"], []
        for name in CLAUDE_GO_LISTS:
            go_src, changed = edit_block(go_src, r"(var " + re.escape(name) + r" = \[\]string\{\n)(.*?)(\n\})",
                                         cid, lambda ind, cid=cid: f'{ind}"{cid}",', GO_ID)
            if changed:
                edits.append(f"`{CLI_MODELS_GO}`: `{name}` gains `\"{cid}\"`")
        js_line = f"{{ value: '{cid}', label: '{c['label']}' }},"
        js_src, changed = edit_block(js_src, r"(const " + CLAUDE_JS_CONST + r" = \[\n)(.*?)(\n\s*\];)",
                                     cid, lambda ind, t=js_line: ind + t, JS_ID)
        if changed:
            edits.append(f"`{BACKENDS_JS}`: `{CLAUDE_JS_CONST}` gains `{js_line}`")
            edits.append("`src/pkg/dashboard/static/index.html`: rebuilt with `make dashboard-gen`")
        if not any(ln.split()[:2] == ["claude", cid] for ln in reqs.splitlines()):
            req_line = f"claude {cid} {pin}"
            reqs = reqs + ("" if reqs.endswith("\n") else "\n") + req_line + "\n"
            edits.append(f"`{REQUIREMENTS}`: `{req_line}`")
        edits.append("`src/pkg/tokens/pricing.go`: `modelPrices` row from the OpenRouter quote, through "
                     "`pricetable --add` and its plausibility rules (see the price section)")
        added.append((c, edits))

    if accepted and not dry_run:
        for rel, body in ((CLI_MODELS_GO, go_src), (BACKENDS_JS, js_src), (REQUIREMENTS, reqs)):
            with open(path(rel), "w", encoding="utf-8") as f:
                f.write(body)

    lines = ["## Claude model freshness", "",
             f"Pinned Claude Code: `{pin}` (`{DOCKERFILE}`). Gate: {note}.", ""]
    if dry_run:
        lines += ["**Dry run:** nothing was written.", ""]
    lines += [f"### Added ({len(added)})", ""]
    if not added:
        lines += ["No model added.", ""]
    for c, edits in added:
        lines += [f"#### {c['label']} (`{c['cli_id']}`, from `{c['source_id']}`)", ""]
        lines += [f"- {e}" for e in edits] + [""]
    if needs:
        lines += [f"### Needs a check against the Claude Code changelog ({len(needs)})", "",
                  "Newer than the lists on OpenRouter, but not shown to be accepted by the pinned Claude Code, so not added:", ""]
        lines += [f"- {c['label']} (`{c['cli_id']}`, from `{c['source_id']}`): {why}" for c, why in needs] + [""]
    return "\n".join(lines), [c["cli_id"] for c in accepted]


def main_apply(args):
    if args.catalog:
        with open(args.catalog, encoding="utf-8") as f:
            catalog = json.load(f).get("data", [])
    else:
        catalog = fetch_json(OPENROUTER_MODELS).get("data", [])
    report, accepted = apply_claude(args.root, catalog, args.claude_bin, args.dry_run)
    print(report)
    if args.accepted_out:
        with open(args.accepted_out, "w", encoding="utf-8") as f:
            f.write("".join(i + "\n" for i in accepted))
    return 0


def parse_args(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--apply", choices=["claude"], help="edit the Claude lists for newer models the pinned CLI accepts")
    ap.add_argument("--catalog", help="OpenRouter /api/v1/models JSON file instead of fetching it")
    ap.add_argument("--claude-bin", help="the pinned Claude Code binary to probe (without it nothing is added)")
    ap.add_argument("--root", default=".", help="repository root (default: .)")
    ap.add_argument("--dry-run", action="store_true", help="print the edits; write nothing")
    ap.add_argument("--accepted-out", help="write the added CLI ids here, one per line")
    return ap.parse_args(argv)


def main():
    with open(CLI_MODELS_GO, encoding="utf-8") as f:
        cli_src = f.read()
    with open(OPENROUTER_GO, encoding="utf-8") as f:
        or_src = f.read()

    sections = []
    drift = False
    try:
        catalog = fetch_json(OPENROUTER_MODELS).get("data", [])
    except Exception as exc:  # noqa: BLE001 — report, never fail the cron
        catalog = []
        sections.append(f"### OpenRouter catalog unavailable\n\n`{OPENROUTER_MODELS}` failed: `{exc}`. Nothing was compared against it this week.")
    live_ids = {m.get("id", "") for m in catalog if m.get("id")}
    by_vendor = index_catalog(catalog)

    if live_ids:
        ids, default = openrouter_suggestions(or_src)
        missing = [i for i in ids + ([default] if default else []) if i not in live_ids]
        if missing:
            drift = True
            sections.append("### OpenRouter suggestions no longer in the catalog\n\n"
                            "`crux/openrouter` SuggestedModels / DefaultModel (the dashboard already hides these at runtime; refresh the list and `testdata/models.json`):\n\n"
                            + "\n".join(f"- `{i}`" for i in sorted(set(missing))))

        for name, prefix in GO_LISTS.items():
            ids = [i for i in go_string_list(cli_src, name) if i not in ALIASES]
            if not ids:
                sections.append(f"### `{name}` could not be parsed\n\nThe checker found no ids; update the parser or the list.")
                continue
            not_found, newer_keys = compare_list(ids, prefix, by_vendor)
            newer = {f"{vendor}/{key}" for vendor, key in newer_keys}
            lines = []
            if newer:
                lines.append("Newer models of the same family on OpenRouter that the list lacks:\n\n" + "\n".join(f"- `{n}`" for n in sorted(newer)))
            if not_found:
                lines.append("Ids with no OpenRouter counterpart (may be CLI-only aliases; check the CLI's own catalog):\n\n" + "\n".join(f"- `{n}`" for n in sorted(not_found)))
            if lines:
                drift = True
                sections.append(f"### `{name}` ({CLI_MODELS_GO})\n\n" + "\n\n".join(lines))

    versions = []
    for cli, pkg in NPM_PACKAGES.items():
        try:
            versions.append(f"- {cli}: `{pkg}` latest `{fetch_json(NPM_LATEST.format(pkg=pkg)).get('version', '?')}`")
        except Exception as exc:  # noqa: BLE001
            versions.append(f"- {cli}: `{pkg}` lookup failed (`{exc}`)")

    if not drift:
        print("NO_DRIFT")
    print("## Static model list freshness\n")
    print("Weekly comparison of hive's static model lists against public sources. "
          "Static lists are fallbacks: Claude, Copilot, Codex, Gemini, goose, agy and omp are probed live at runtime, "
          "and agents on a gateway inherit the gateway's live `/v1/models` list.\n")
    print("\n\n".join(sections) if sections else "No drift found against OpenRouter this week.")
    print("\n### CLI releases (npm, for context)\n\n" + "\n".join(versions))
    print("\n### Not checked\n\nNeeds auth or has no machine-readable list: the Claude CLI's `/v1/models` (OAuth), the Copilot SDK catalog, "
          "the Codex app-server catalog, Kiro (pi), agy, omp, goose providers, bob. The Go unit test `TestStaticModelListsShape` still checks every list is "
          "non-empty, duplicate-free and matches `static/index.html`.")
    return 0


if __name__ == "__main__":
    _args = parse_args(sys.argv[1:])
    sys.exit(main_apply(_args) if _args.apply else main())
