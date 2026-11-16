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
import json
import re
import sys
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

ALIASES = {"opus", "sonnet", "haiku", "auto", "default"}


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
    by_vendor = {}
    for mid in live_ids:
        if "/" not in mid or mid.startswith("~"):
            continue
        vendor, rest = mid.split("/", 1)
        by_vendor.setdefault(vendor, set()).add(norm(rest))

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
                        newer.add(f"{vendor}/{key}")
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
    sys.exit(main())
