#!/usr/bin/env python3
"""Render the generated tables in src/docs/dashboard-ui-map.md (#19182).

The UI map page lists the dashboard's sidebar entries and Settings tabs with
the exact words the screen shows, so a person (or a docs search or Ask AI
answer) can match what they type to what they see. The tables are generated
from the dashboard sources so they cannot drift from the labels:

- the sidebar groups and entries: src/pkg/dashboard/static-src/html/sidebar.html
- each section's heading and one-line help: DASHBOARD_SECTION_HELP in
  src/pkg/dashboard/static-src/js/sections.js
- the Settings tabs and the per-agent tabs: GOVERNOR_CONFIG_TABS and
  AGENT_CONFIG_TABS in src/pkg/dashboard/static-src/js/config-state.js

Only the text between the BEGIN and END markers is rewritten; the rest of the
page is hand-written.

Usage:
  python3 src/pkg/dashboard/uimap/render-dashboard-ui-map.py          # rewrite the page
  python3 src/pkg/dashboard/uimap/render-dashboard-ui-map.py --check  # exit 1 if stale
"""

import html
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "..", ".."))
DASH = os.path.join(ROOT, "src", "pkg", "dashboard", "static-src")
SIDEBAR = os.path.join(DASH, "html", "sidebar.html")
SECTIONS = os.path.join(DASH, "js", "sections.js")
CONFIG_STATE = os.path.join(DASH, "js", "config-state.js")
DOCS = os.path.join(ROOT, "src", "docs")
PAGE = os.path.join(DOCS, "dashboard-ui-map.md")
SETTINGS_PAGE = os.path.join(DOCS, "settings.md")
SECTIONS_PAGE = "dashboard-sections.md"

BEGIN = "<!-- BEGIN GENERATED: dashboard-ui-map (src/pkg/dashboard/uimap/render-dashboard-ui-map.py) -->"
END = "<!-- END GENERATED: dashboard-ui-map -->"

# The Agents group holds fixed entries (Control panel, Activity) followed by
# "All agents (n)", a live tree of the hive's own agents.
AGENT_TREE_GROUP = "Agents"


def read(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def anchor(heading):
    """GitHub-style heading anchor, matching dashboardSectionHelpAnchor."""
    s = re.sub(r"[^\w\s-]", "", heading.lower()).strip()
    return re.sub(r"\s", "-", s)


def cell(text):
    return html.unescape(text).replace("|", "\\|").strip()


def section_help():
    src = read(SECTIONS)
    block = re.search(r"const DASHBOARD_SECTION_HELP = Object\.freeze\(\{(.*?)\n\s*\}\);", src, re.S)
    if not block:
        sys.exit("render-dashboard-ui-map: DASHBOARD_SECTION_HELP not found in sections.js")
    entries = {}
    pattern = re.compile(
        r"'([\w-]+)':\s*\{\s*heading:\s*'((?:[^'\\]|\\.)*)',\s*help:\s*(?:'((?:[^'\\]|\\.)*)'|\"((?:[^\"\\]|\\.)*)\")\s*\}"
    )
    for m in pattern.finditer(block.group(1)):
        help_text = m.group(3) if m.group(3) is not None else m.group(4)
        entries[m.group(1)] = (m.group(2).replace("\\'", "'"), help_text.replace("\\'", "'").replace('\\"', '"'))
    if not entries:
        sys.exit("render-dashboard-ui-map: no entries parsed from DASHBOARD_SECTION_HELP")
    return entries


def sidebar_groups():
    src = read(SIDEBAR)
    groups = []
    for chunk in re.split(r'<div class="oc-nav-group"', src)[1:]:
        label = re.search(r'<div class="oc-nav-label">([^<]+)</div>', chunk)
        tree = re.search(r'<div class="oc-tree-toggle"[^>]*>.*?<span>([^<]+)</span>', chunk, re.S)
        name = (label or tree).group(1).strip() if (label or tree) else ""
        items = []
        for m in re.finditer(r'<a class="oc-nav-item[^"]*"([^>]*)>(.*?)</a>', chunk, re.S):
            attrs, body = m.group(1), m.group(2)
            text = re.search(r'<span class="oc-nav-text">([^<]+)</span>', body)
            if not text:
                continue
            section = re.search(r'data-section="([\w-]+)"', attrs)
            items.append((html.unescape(text.group(1)).strip(), section.group(1) if section else None, " hidden" in attrs or "data-feature=" in attrs))
        groups.append((name, items))
    return groups


def js_string_list(src, name, consts):
    m = re.search(r"const " + name + r" = \[(.*?)\];", src, re.S)
    if not m:
        sys.exit("render-dashboard-ui-map: %s not found in config-state.js" % name)
    out = []
    for tok in re.findall(r"'([^']*)'|([A-Z_]+)", m.group(1)):
        if tok[0]:
            out.append(tok[0])
        elif tok[1] in consts:
            out.append(consts[tok[1]])
    return out


def config_tabs():
    src = read(CONFIG_STATE)
    consts = dict(re.findall(r"const ([A-Z_]+) = '([^']*)';", src))
    return js_string_list(src, "GOVERNOR_CONFIG_TABS", consts), js_string_list(src, "AGENT_CONFIG_TABS", consts)


def settings_headings():
    return {h.strip(): anchor(h.strip()) for h in re.findall(r"^## (.+)$", read(SETTINGS_PAGE), re.M)}


def render():
    help_map = section_help()
    lines = [BEGIN, "", "### Sidebar", ""]
    lines.append("| Sidebar group | Entry | Section heading | What it shows | Help |")
    lines.append("|---|---|---|---|---|")
    for group, items in sidebar_groups():
        for text, section, optional in items:
            entry = "**%s**%s" % (cell(text), " (only when switched on)" if optional else "")
            if section and section in help_map:
                heading, help_text = help_map[section]
                lines.append("| %s | %s | %s | %s | [%s](%s#%s) |" % (group, entry, cell(heading), cell(help_text), cell(heading), SECTIONS_PAGE, anchor(heading)))
            elif section:
                lines.append("| %s | %s | | | |" % (group, entry))
            else:
                lines.append("| %s | %s | | A link or dialog, not a dashboard section. | |" % (group, entry))
        if group == AGENT_TREE_GROUP:
            lines.append("| Agents | **All agents (n)**: one entry per agent, plus **＋ Add agent** and **＋ Group** | | Opens that agent's card. Collapsing the list keeps the entries above it visible. | [Control panel](%s#control-panel) |" % SECTIONS_PAGE)
    governor_tabs, agent_tabs = config_tabs()
    documented = settings_headings()
    lines += ["", "### Settings tabs", ""]
    lines.append("Open **⚙ Settings** in the top bar. The tabs, in the order the dialog shows them:")
    lines.append("")
    lines.append("| Tab | Explained in |")
    lines.append("|---|---|")
    for tab in governor_tabs:
        # A tab's entry is the heading named after it, or one that starts with
        # its name ("Security and Access" covers the Security tab).
        heading = tab if tab in documented else next((h for h in documented if h.startswith(tab + " ")), None)
        ref = "[Settings: %s](settings.md#%s)" % (heading, documented[heading]) if heading else "[Dashboard settings explained](settings.md)"
        lines.append("| **%s** | %s |" % (tab, ref))
    lines += ["", "### Agent settings tabs", ""]
    lines.append("Open an agent's settings from its card (the ⚙ button) or from **＋ Add agent**. The tabs:")
    lines.append("")
    lines.append(", ".join("**%s**" % t for t in agent_tabs) + ".")
    lines += ["", END]
    return "\n".join(lines)


def main():
    check = "--check" in sys.argv[1:]
    page = read(PAGE)
    start, end = page.find(BEGIN), page.find(END)
    if start < 0 or end < start:
        sys.exit("render-dashboard-ui-map: markers not found in %s" % os.path.relpath(PAGE, ROOT))
    updated = page[:start] + render() + page[end + len(END):]
    if updated == page:
        print("dashboard-ui-map.md is up to date")
        return 0
    if check:
        print("dashboard-ui-map.md is stale: run python3 src/pkg/dashboard/uimap/render-dashboard-ui-map.py", file=sys.stderr)
        return 1
    with open(PAGE, "w", encoding="utf-8") as f:
        f.write(updated)
    print("rewrote %s" % os.path.relpath(PAGE, ROOT))
    return 0


if __name__ == "__main__":
    sys.exit(main())
