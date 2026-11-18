#!/usr/bin/env python3
"""Keep codexStaticModels / CODEX_CLI_MODELS equal to the pinned Codex CLI (#19036).

Codex has no per-account catalogue: its model list is built into the binary.
This runs the same `codex app-server` initialize + model/list exchange that
discoverCodexModels (src/pkg/dashboard/cli_models_codex.go) speaks, against the
Codex version pinned in src/Dockerfile and with an empty CODEX_HOME (no ChatGPT
login needed), then rewrites the static Go list, the JS list and its
static/index.html mirror in orderCodexServedModels order: the default id first,
then the other visible ids in picker order, then the hidden ids.

Gate (nothing is written unless all hold): `codex --version` equals the pin, the
probe answered, ids are unique, and at least one id is visible. A failure
prints the reason and leaves the tree untouched.

Usage: codex-model-freshness.py [--root DIR] [--codex-bin PATH] [--dry-run]
                                [--report-out FILE]
A re-run against an unchanged catalogue changes nothing.
"""

import argparse
import datetime
import json
import os
import re
import subprocess
import sys
import tempfile
import threading

CLI_MODELS_GO = "src/pkg/dashboard/cli_models.go"
BACKENDS_JS = "src/pkg/dashboard/static-src/js/backends-tokens.js"
INDEX_HTML = "src/pkg/dashboard/static/index.html"
DOCKERFILE = "src/Dockerfile"

PROBE_TIMEOUT = 60

GO_BLOCK = re.compile(r"(var codexStaticModels = \[\]string\{\n)(.*?)(\n\})", re.S)
JS_BLOCK = re.compile(r"(const CODEX_CLI_MODELS = \[\n)(.*?)(\n\s*\];)", re.S)
GO_STAMP = re.compile(r"matches codex [0-9][0-9.]* \(captured [0-9-]+")
JS_STAMP = re.compile(r"Snapshot of codex [0-9][0-9.]* \(captured [0-9-]+")


def read(path):
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def write(path, text):
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(text)


def pinned_version(root):
    m = re.search(r"^ARG CODEX_VERSION=(\S+)", read(os.path.join(root, DOCKERFILE)), re.M)
    if not m:
        raise SystemExit("no ARG CODEX_VERSION in " + DOCKERFILE)
    return m.group(1)


def codex_version(codex_bin):
    out = subprocess.run([codex_bin, "--version"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         text=True, timeout=PROBE_TIMEOUT).stdout
    m = re.search(r"\d+\.\d+\.\d+\S*", out)
    return m.group(0) if m else ""


def probe_model_list(codex_bin):
    """The initialize handshake and model/list (includeHidden) over stdio."""
    with tempfile.TemporaryDirectory(prefix="codex-model-freshness-") as home:
        env = dict(os.environ, CODEX_HOME=home)
        proc = subprocess.Popen([codex_bin, "app-server"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, text=True, env=env)
        timer = threading.Timer(PROBE_TIMEOUT, proc.kill)
        timer.start()
        try:
            def send(msg):
                proc.stdin.write(json.dumps(msg) + "\n")
                proc.stdin.flush()

            def await_id(want):
                for line in proc.stdout:
                    try:
                        msg = json.loads(line)
                    except ValueError:
                        continue
                    if msg.get("id") == want:
                        if msg.get("error"):
                            raise RuntimeError("request %s rejected: %s" % (want, str(msg["error"])[:200]))
                        return msg.get("result") or {}
                raise RuntimeError("app-server closed before answering request %s" % want)

            send({"id": 1, "method": "initialize", "params": {"clientInfo": {"name": "hive-model-freshness", "version": "1"}}})
            await_id(1)
            send({"id": 2, "method": "model/list", "params": {"includeHidden": True}})
            return await_id(2).get("data") or []
        finally:
            timer.cancel()
            proc.kill()
            proc.wait()


def order_served(entries):
    """orderCodexServedModels: default, then visible in picker order, then hidden."""
    default, visible, hidden = [], [], []
    for e in entries:
        mid = e.get("id") or e.get("model") or ""
        if not mid:
            continue
        if e.get("hidden"):
            hidden.append(mid)
        elif e.get("isDefault"):
            default.append(mid)
        else:
            visible.append(mid)
    return default + visible + hidden, set(hidden)


def gate(pin, version, ids, hidden):
    problems = []
    if version != pin:
        problems.append("codex --version is %r but src/Dockerfile pins %s" % (version, pin))
    if not ids:
        problems.append("model/list returned no models")
    if len(set(ids)) != len(ids):
        problems.append("model/list returned duplicate ids")
    if ids and len(hidden) == len(ids):
        problems.append("model/list returned only hidden models")
    return problems


def js_labels(js_src):
    m = JS_BLOCK.search(js_src)
    return dict(re.findall(r"value: '([^']+)', label: '([^']*)'", m.group(2))) if m else {}


def go_ids(go_src):
    m = GO_BLOCK.search(go_src)
    return re.findall(r'^\t"([^"]+)",', m.group(2), re.M) if m else []


def stamp_date(src, stamp):
    m = stamp.search(src)
    return m.group(0).rsplit("captured ", 1)[1] if m else ""


def render_go(src, ids, hidden, pin, date):
    lines = []
    first_hidden = True
    for i in ids:
        if i in hidden and first_hidden:
            lines.append("\t// Hidden in the codex %s picker but still valid --model values." % pin)
            first_hidden = False
        lines.append('\t"%s",' % i)
    src = GO_BLOCK.sub(lambda m: m.group(1) + "\n".join(lines) + m.group(3), src, count=1)
    return GO_STAMP.sub("matches codex %s (captured %s" % (pin, date), src, count=1)


def render_js(src, ids, labels, pin, date):
    m = JS_BLOCK.search(src)
    indent = re.match(r"\s*", m.group(2)).group(0)
    lines = ["%s{ value: '%s', label: '%s' }," % (indent, i, labels[i].replace("'", "\\'")) for i in ids]
    src = JS_BLOCK.sub(lambda m: m.group(1) + "\n".join(lines) + m.group(3), src, count=1)
    return JS_STAMP.sub("Snapshot of codex %s (captured %s" % (pin, date), src, count=1)


def apply(root, ids, hidden, display, pin, dry_run, today):
    files = {rel: read(os.path.join(root, rel)) for rel in (CLI_MODELS_GO, BACKENDS_JS, INDEX_HTML)}
    labels = js_labels(files[BACKENDS_JS])
    labels = {i: labels.get(i) or display.get(i) or i for i in ids}

    def build(date):
        return {
            CLI_MODELS_GO: render_go(files[CLI_MODELS_GO], ids, hidden, pin, date),
            BACKENDS_JS: render_js(files[BACKENDS_JS], ids, labels, pin, date),
            INDEX_HTML: render_js(files[INDEX_HTML], ids, labels, pin, date),
        }

    # Keep the capture date when nothing else changes, so a re-run is a no-op.
    new = build(stamp_date(files[CLI_MODELS_GO], GO_STAMP) or today)
    if new != files:
        new = build(today)
    changed = [rel for rel in files if new[rel] != files[rel]]
    if not dry_run:
        for rel in changed:
            write(os.path.join(root, rel), new[rel])
    return changed


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--root", default=os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..")))
    ap.add_argument("--codex-bin", default="codex")
    ap.add_argument("--dry-run", action="store_true", help="report the edits; write nothing")
    ap.add_argument("--report-out", help="write the Markdown report (the PR body) here")
    args = ap.parse_args(argv)

    pin = pinned_version(args.root)
    try:
        version = codex_version(args.codex_bin)
        entries = probe_model_list(args.codex_bin)
    except (OSError, RuntimeError, subprocess.SubprocessError) as exc:
        print("codex probe failed: %s" % exc, file=sys.stderr)
        return 1
    ids, hidden = order_served(entries)
    problems = gate(pin, version, ids, hidden)
    if problems:
        for p in problems:
            print("gate failed: " + p, file=sys.stderr)
        return 1

    display = {(e.get("id") or e.get("model")): e.get("displayName") for e in entries}
    old = go_ids(read(os.path.join(args.root, CLI_MODELS_GO)))
    changed = apply(args.root, ids, hidden, display, pin, args.dry_run, datetime.date.today().isoformat())

    added = [i for i in ids if i not in old]
    removed = [i for i in old if i not in ids]
    report = ["### Codex static model list (codex %s)" % pin, ""]
    if not changed:
        report.append("`codexStaticModels` already matches the pinned binary; nothing to change.")
    else:
        verb = "would change" if args.dry_run else "changed"
        report.append("Probed the pinned `codex app-server` `model/list` (no login) and %s: %s." %
                      (verb, ", ".join("`%s`" % c for c in changed)))
        report.append("")
        report.append("- Added: %s" % (", ".join("`%s`" % i for i in added) or "none"))
        report.append("- Removed: %s" % (", ".join("`%s`" % i for i in removed) or "none"))
        report.append("- Order: default and visible ids first, then hidden ids (`orderCodexServedModels`).")
    text = "\n".join(report) + "\n"
    if args.report_out:
        write(args.report_out, text)
    print(text, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())
