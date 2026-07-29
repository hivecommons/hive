#!/bin/bash
# merge-gate.sh — Determines which open PRs are eligible for merge.
# Reads from /var/run/hive-metrics/actionable.json (produced by enumerate-actionable.sh).
# Writes /var/run/hive-metrics/merge-eligible.json.
#
# A PR is merge-eligible when:
#   - All required CI checks pass (ignoring: tide, Playwright, netlify)
#   - It is not a draft
#   - It is not excluded by the enumerator (hold, ADOPTERS, etc.)
#   - Author is AI-authored, OR community-authored with APPROVED review
#
# Agents should ONLY merge PRs that appear in this file.

set -euo pipefail

# Source project config for AI author
# shellcheck source=hive-config.sh
source "$(dirname "$0")/hive-config.sh" 2>/dev/null || source /usr/local/bin/hive-config.sh 2>/dev/null || true

ACTIONABLE_FILE="/var/run/hive-metrics/actionable.json"
OUTPUT_FILE="/var/run/hive-metrics/merge-eligible.json"
TMP_FILE="${OUTPUT_FILE}.tmp"
LOG="/var/log/kick-agents.log"

# CI check names to ignore when evaluating merge readiness
# NOTE: netlify/Deploy Preview are NOT ignored — Netlify failures must block merges
IGNORED_CHECKS="tide|Playwright|attribute|Storybook|Visual |Verify build after merge"

log() { echo "[$(date -Is)] MERGE-GATE $*" >> "$LOG"; }

if [ ! -f "$ACTIONABLE_FILE" ]; then
  log "SKIP — actionable.json not found, run enumerate-actionable.sh first"
  exit 0
fi

log "START"

# Extract PR list from actionable.json
prs=$(python3 -c "
import json
with open('$ACTIONABLE_FILE') as f:
    data = json.load(f)
for p in data.get('prs', {}).get('items', []):
    print(f\"{p['repo']}:{p['number']}\")
" 2>/dev/null)

if [ -z "$prs" ]; then
  python3 -c "
import json
from datetime import datetime, timezone
result = {
    'generated_at': datetime.now(timezone.utc).isoformat(),
    'merge_eligible': [],
    'not_ready': [],
    'count': 0
}
print(json.dumps(result, indent=2))
" > "$OUTPUT_FILE"
  log "DONE — no actionable PRs"
  exit 0
fi

# Check CI status for each PR in parallel
checks_tmp=$(mktemp -d)
trap 'rm -rf "$checks_tmp"' EXIT

for entry in $prs; do
  repo="${entry%%:*}"
  num="${entry##*:}"
  (
    # Get check status (gh pr checks exits non-zero when checks are failing — use || true)
    checks=$(gh pr checks "$num" --repo "$repo" 2>/dev/null) || true

    if [ -z "$checks" ]; then
      echo "{\"repo\":\"$repo\",\"number\":$num,\"status\":\"error\",\"reason\":\"could not fetch checks\"}" > "$checks_tmp/${repo//\//_}_${num}.json"
    else
      # Parse check results, ignoring specified checks
      status=$(echo "$checks" | grep -viE "$IGNORED_CHECKS" | python3 -c "
import sys
lines = [l.strip() for l in sys.stdin if l.strip()]
if not lines:
    print('pass')
else:
    statuses = []
    for line in lines:
        parts = line.split('\t')
        if len(parts) >= 2:
            statuses.append(parts[1].strip().lower() if len(parts) > 1 else 'unknown')
        else:
            statuses.append('unknown')
    # Treat 'skipping' as 'pass' (conditional jobs that don't run on this PR)
    normalized = ['pass' if s == 'skipping' else s for s in statuses]
    if all(s == 'pass' for s in normalized):
        print('pass')
    elif any(s == 'fail' for s in normalized):
        print('fail')
    elif any(s == 'pending' for s in normalized):
        print('pending')
    else:
        print('unknown')
" 2>/dev/null || echo "unknown")

      # Get PR metadata (including reviewDecision for community PR approval and
      # labels for the deterministic hold-gate below).
      pr_info=$(gh pr view "$num" --repo "$repo" --json title,author,isDraft,mergeable,reviewDecision,labels 2>/dev/null || echo '{}')
      author=$(echo "$pr_info" | python3 -c "import json,sys; print(json.load(sys.stdin).get('author',{}).get('login','unknown'))" 2>/dev/null || echo "unknown")
      title=$(echo "$pr_info" | python3 -c "import json,sys; print(json.load(sys.stdin).get('title',''))" 2>/dev/null || echo "")
      mergeable=$(echo "$pr_info" | python3 -c "import json,sys; print(json.load(sys.stdin).get('mergeable','UNKNOWN'))" 2>/dev/null || echo "UNKNOWN")
      review=$(echo "$pr_info" | python3 -c "import json,sys; print(json.load(sys.stdin).get('reviewDecision',''))" 2>/dev/null || echo "")
      labels=$(echo "$pr_info" | python3 -c "import json,sys; print(json.dumps([l.get('name','') for l in json.load(sys.stdin).get('labels',[])]))" 2>/dev/null || echo "[]")

      echo "{\"repo\":\"$repo\",\"number\":$num,\"status\":\"$status\",\"author\":\"$author\",\"title\":$(python3 -c "import json,sys; print(json.dumps(sys.argv[1][:100]))" "$title" 2>/dev/null || echo '""'),\"mergeable\":\"$mergeable\",\"reviewDecision\":\"$review\",\"labels\":$labels}" > "$checks_tmp/${repo//\//_}_${num}.json"
    fi
  ) &
done
wait

# Assemble results
python3 -c "
import json, os, sys, glob
from datetime import datetime, timezone

checks_dir = sys.argv[1]
eligible = []
not_ready = []

AI_AUTHORS = {os.environ.get('PROJECT_AI_AUTHOR', ''), 'copilot-swe-agent[bot]', 'github-actions[bot]', 'dependabot[bot]', 'app/kubestellar-hive', 'kubestellar-hive[bot]'} - {''}

for f in sorted(glob.glob(os.path.join(checks_dir, '*.json'))):
    try:
        with open(f) as fh:
            pr = json.load(fh)
    except:
        continue

    is_ai = pr.get('author', '') in AI_AUTHORS
    ci_pass = pr.get('status') == 'pass'
    mergeable = pr.get('mergeable', 'UNKNOWN') in ('MERGEABLE', 'UNKNOWN')
    approved = pr.get('reviewDecision', '') == 'APPROVED'

    # Deterministic hold-gate: a PR carrying any hold label is NEVER merge-eligible,
    # regardless of CI/author. Hold-gating previously lived only in agent prompts
    # and the enumerator; enforcing it here in code makes it a hard gate the merge
    # step itself honors. Extra hold labels can be supplied via HOLD_LABELS (comma-
    # separated); the defaults cover the standard variants.
    hold_labels = {l.strip().lower() for l in os.environ.get('HOLD_LABELS', 'hold,on-hold,hold/review,do-not-merge,dont-merge,wip').split(',') if l.strip()}
    pr_labels = {str(l).lower() for l in pr.get('labels', [])}
    held = bool(pr_labels & hold_labels)

    pr['ai_authored'] = is_ai
    pr['ci_pass'] = ci_pass
    pr['approved'] = approved
    pr['held'] = held

    # Eligible: CI green + mergeable + not held + (AI-authored OR community-approved)
    if ci_pass and mergeable and not held and (is_ai or approved):
        eligible.append(pr)
    else:
        reasons = []
        if held:
            reasons.append('held (hold label present)')
        if not ci_pass:
            reasons.append(f\"ci={pr.get('status','?')}\")
        if not mergeable:
            reasons.append(f\"mergeable={pr.get('mergeable','?')}\")
        if not is_ai and not approved:
            reasons.append(f\"author={pr.get('author','?')} (not AI, not approved)\")
        pr['block_reasons'] = reasons
        not_ready.append(pr)

result = {
    'generated_at': datetime.now(timezone.utc).isoformat(),
    'merge_eligible': eligible,
    'not_ready': not_ready,
    'count': len(eligible)
}
print(json.dumps(result, indent=2))
" "$checks_tmp" > "$TMP_FILE" 2>/dev/null

mv "$TMP_FILE" "$OUTPUT_FILE"

eligible_count=$(python3 -c "import json; print(len(json.load(open('$OUTPUT_FILE')).get('merge_eligible',[])))" 2>/dev/null || echo 0)
total=$(python3 -c "import json; d=json.load(open('$OUTPUT_FILE')); print(len(d.get('merge_eligible',[]))+len(d.get('not_ready',[])))" 2>/dev/null || echo 0)

log "DONE — $eligible_count merge-eligible out of $total PRs"
