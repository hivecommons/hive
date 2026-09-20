#!/bin/bash
# issue-classifier.sh — Pre-classify actionable issues with deterministic metadata.
# Enriches actionable.json with: complexity_tier, model_recommendation, is_tracker,
# cluster_key, lane, needs_architecture_review.
#
# All classification rules are read from hive-project.yaml → classification section.
# Other projects: add your own patterns there — this script is rule-agnostic.
#
# Called by enumerate-actionable.sh after the base enumeration pass.

set -euo pipefail

INPUT_FILE="/var/run/hive-metrics/actionable.json"
LOG="/var/log/kick-agents.log"

PROJECT_YAML="${HIVE_PROJECT_YAML:-/etc/hive/hive-project.yaml}"
if [ ! -f "$PROJECT_YAML" ]; then
  # A deployed copy of this script may have no repo checkout (and so no
  # examples/) beside it. Under `set -euo pipefail` a failing find would kill
  # the classifier stage instead of falling through to the built-in defaults
  # (#7810), so only search when the directory exists.
  EXAMPLES_DIR="$(dirname "$(dirname "$0")")/examples"
  PROJECT_YAML=""
  if [ -d "$EXAMPLES_DIR" ]; then
    PROJECT_YAML="$(find "$EXAMPLES_DIR" -name 'hive-project.yaml' -type f 2>/dev/null | head -1 || true)"
  fi
fi

log() { echo "[$(date -Is)] CLASSIFY $*" >> "$LOG"; }

if [ ! -f "$INPUT_FILE" ]; then
  log "ERROR: $INPUT_FILE not found — skipping classification"
  exit 0
fi

python3 -c "
import json, sys, re
from datetime import datetime, timezone
from collections import defaultdict

import yaml

# Load actionable data
with open(sys.argv[1]) as f:
    data = json.load(f)

# Load classification config
config = {}
try:
    with open(sys.argv[2]) as f:
        cfg = yaml.safe_load(f)
    config = cfg.get('classification', {})
except Exception:
    pass

issues = data.get('issues', {}).get('items', [])
now = datetime.now(timezone.utc)

# --- Build rules from config (with defaults) ---
complexity = config.get('complexity', {})

simple_cfg = complexity.get('simple', {})
SIMPLE_LABELS = set(simple_cfg.get('labels', ['auto-qa']))
SIMPLE_PATTERNS = [re.compile(p, re.IGNORECASE) for p in simple_cfg.get('title_patterns', [])]
SIMPLE_MODEL = simple_cfg.get('model', 'haiku')

complex_cfg = complexity.get('complex', {})
COMPLEX_LABELS = set(complex_cfg.get('labels', ['architecture', 'epic']))
COMPLEX_PATTERNS = [re.compile(p, re.IGNORECASE) for p in complex_cfg.get('title_patterns', [])]
COMPLEX_MODEL = complex_cfg.get('model', 'opus')

DEFAULT_MODEL = complexity.get('default_model', 'sonnet')

TRACKER_PREFIXES = config.get('tracker_prefixes', ['[Auto-QA]', '[Nightly]'])

lanes_cfg = config.get('lanes', {})
LANE_RULES = {}
for lane_name, lane_def in lanes_cfg.items():
    LANE_RULES[lane_name] = {
        'labels': set(lane_def.get('labels', [])),
        'patterns': [re.compile(p, re.IGNORECASE) for p in lane_def.get('title_patterns', [])]
    }

cluster_cfg = config.get('clustering', {})
REPORTER_WINDOW_SECONDS = cluster_cfg.get('reporter_window_seconds', 1800)
FAILURE_MODES = {}
for mode_key, pattern_str in cluster_cfg.get('failure_modes', {}).items():
    FAILURE_MODES[mode_key] = re.compile(pattern_str, re.IGNORECASE)

# --- Classify each issue ---
for issue in issues:
    labels = set(issue.get('labels', []))
    title = issue.get('title', '')

    # Complexity tier
    if labels & SIMPLE_LABELS or any(p.search(title) for p in SIMPLE_PATTERNS):
        issue['complexity_tier'] = 'Simple'
        issue['model_recommendation'] = SIMPLE_MODEL
    elif labels & COMPLEX_LABELS or any(p.search(title) for p in COMPLEX_PATTERNS):
        issue['complexity_tier'] = 'Complex'
        issue['model_recommendation'] = COMPLEX_MODEL
    else:
        issue['complexity_tier'] = 'Medium'
        issue['model_recommendation'] = DEFAULT_MODEL

    # Tracker detection
    issue['is_tracker'] = any(title.startswith(p) for p in TRACKER_PREFIXES)

    # Lane assignment — first matching lane wins, default is scanner
    issue['lane'] = 'scanner'
    issue['needs_architecture_review'] = False
    for lane_name, rule in LANE_RULES.items():
        if labels & rule['labels'] or any(p.search(title) for p in rule['patterns']):
            issue['lane'] = lane_name
            if lane_name == 'architect':
                issue['needs_architecture_review'] = True
            break

    # Cluster key — component prefix from title (text before first colon)
    cluster_key = None
    if ':' in title:
        prefix = title.split(':')[0].strip()
        if len(prefix) < 40:
            cluster_key = prefix.lower().replace(' ', '-')
    if not cluster_key:
        area_labels = [l for l in labels if l.startswith('area/') or l.startswith('component/')]
        kind_labels = [l for l in labels if l.startswith('kind/')]
        if area_labels:
            cluster_key = area_labels[0].lower()
        elif kind_labels:
            cluster_key = kind_labels[0].lower()
    issue['cluster_key'] = cluster_key

# --- Build clusters ---
clusters_by_key = defaultdict(list)
for issue in issues:
    key = issue.get('cluster_key')
    if key:
        clusters_by_key[key].append({
            'repo': issue['repo'],
            'number': issue['number'],
            'title': issue['title']
        })

# Reporter window clustering
reporter_issues = defaultdict(list)
for issue in issues:
    author = issue.get('author', '')
    if author:
        reporter_issues[author].append(issue)

for author, author_issues in reporter_issues.items():
    if len(author_issues) < 2:
        continue
    sorted_issues = sorted(author_issues, key=lambda x: x.get('created_at', ''))
    window = []
    for issue in sorted_issues:
        try:
            created = datetime.fromisoformat(issue['created_at'].replace('Z', '+00:00'))
        except (ValueError, KeyError):
            continue
        if window and (created - window[0][1]).total_seconds() > REPORTER_WINDOW_SECONDS:
            if len(window) >= 2:
                key = f'reporter-{author}-{window[0][0][\"number\"]}'
                for w_issue, _ in window:
                    if not w_issue.get('cluster_key'):
                        w_issue['cluster_key'] = key
                clusters_by_key[key] = [
                    {'repo': w['repo'], 'number': w['number'], 'title': w['title']}
                    for w, _ in window
                ]
            window = []
        window.append((issue, created))
    if len(window) >= 2:
        key = f'reporter-{author}-{window[0][0][\"number\"]}'
        for w_issue, _ in window:
            if not w_issue.get('cluster_key'):
                w_issue['cluster_key'] = key
        clusters_by_key[key] = [
            {'repo': w['repo'], 'number': w['number'], 'title': w['title']}
            for w, _ in window
        ]

# Failure mode clustering
for mode_key, pattern in FAILURE_MODES.items():
    group = []
    for issue in issues:
        if pattern.search(issue.get('title', '')):
            group.append({
                'repo': issue['repo'],
                'number': issue['number'],
                'title': issue['title']
            })
    if len(group) >= 2:
        fkey = f'failure-{mode_key}'
        if fkey not in clusters_by_key:
            clusters_by_key[fkey] = group

# Only keep clusters with 2+ issues, deduplicate
all_clusters = [
    {'key': k, 'issues': v, 'count': len(v)}
    for k, v in clusters_by_key.items()
    if len(v) >= 2
]
all_clusters.sort(key=lambda c: c['count'], reverse=True)

seen_issues = set()
deduped = []
for cluster in all_clusters:
    unique = [i for i in cluster['issues'] if f\"{i['repo']}#{i['number']}\" not in seen_issues]
    if len(unique) >= 2:
        for i in unique:
            seen_issues.add(f\"{i['repo']}#{i['number']}\")
        cluster['issues'] = unique
        cluster['count'] = len(unique)
        deduped.append(cluster)

# --- Write enriched output ---
data['issues']['items'] = issues
data['clusters'] = deduped
data['classification'] = {
    'generated_at': now.isoformat(),
    'config_source': sys.argv[2],
    'tier_counts': {
        'Simple': sum(1 for i in issues if i.get('complexity_tier') == 'Simple'),
        'Medium': sum(1 for i in issues if i.get('complexity_tier') == 'Medium'),
        'Complex': sum(1 for i in issues if i.get('complexity_tier') == 'Complex'),
    },
    'lane_counts': {},
    'cluster_count': len(deduped),
    'tracker_count': sum(1 for i in issues if i.get('is_tracker')),
}
for issue in issues:
    lane = issue.get('lane', 'scanner')
    data['classification']['lane_counts'][lane] = data['classification']['lane_counts'].get(lane, 0) + 1

with open(sys.argv[1], 'w') as f:
    json.dump(data, f, indent=2)

c = data['classification']
print(f\"Classified {len(issues)} issues: {c['tier_counts']['Simple']}S/{c['tier_counts']['Medium']}M/{c['tier_counts']['Complex']}C, {c['cluster_count']} clusters, lanes={c['lane_counts']}\")
" "$INPUT_FILE" "$PROJECT_YAML"

log "DONE — classification complete"
