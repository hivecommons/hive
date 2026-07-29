package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const (
	defaultContributorsDir    = "/data/contributors"
	contributorAutoPromoteAt  = 5
	contributorTrustedAt      = 20
	defaultFederationRegistry = "/data/federation/registry.json"
)

func getContributorsDir() string {
	if v := os.Getenv("HIVE_CONTRIBUTORS_DIR"); v != "" {
		return v
	}
	return defaultContributorsDir
}

func getFederationRegistryPath() string {
	if v := os.Getenv("HIVE_FEDERATION_REGISTRY_PATH"); v != "" {
		return v
	}
	return defaultFederationRegistry
}

type ContributorProfile struct {
	GitHubUsername    string                `json:"github_username"`
	ContributorID     string                `json:"contributor_id"`
	RegistrationToken string                `json:"registration_token"`
	TokenPlain        string                `json:"registration_token_plain,omitempty"`
	TrustTier         string                `json:"trust_tier"`
	PreferredRole     string                `json:"preferred_role,omitempty"`
	CLIBackend        string                `json:"cli_backend,omitempty"`
	Model             string                `json:"model,omitempty"`
	AvatarURL         string                `json:"avatar_url,omitempty"`
	RegisteredAt      string                `json:"registered_at"`
	TasksCompleted    int                   `json:"total_tasks_completed"`
	TasksFailed       int                   `json:"total_tasks_failed"`
	LastActive        string                `json:"last_active,omitempty"`
	LastCompletedTask *WSTaskAssign         `json:"last_completed_task,omitempty"`
	RateLimits        ContributorRateLimits `json:"rate_limits"`
	Active            bool                  `json:"active,omitempty"`
	CurrentTask       *WSTaskAssign         `json:"current_task,omitempty"`
	ActiveTasks       []WSTaskAssign        `json:"active_tasks,omitempty"`
	Sessions          int                   `json:"sessions,omitempty"`
}

type ContributorRateLimits struct {
	MaxConcurrent int `json:"max_concurrent_tasks"`
	MaxPerHour    int `json:"max_tasks_per_hour"`
	MaxPerDay     int `json:"max_tasks_per_day"`
}

type ContributorPool struct {
	Active     int `json:"active"`
	Registered int `json:"registered"`
	mu         sync.RWMutex
}

var contributorPool = &ContributorPool{}

type ContributorPoolStatus struct {
	Active     int            `json:"active"`
	Registered int            `json:"registered"`
	ByRole     map[string]int `json:"by_role,omitempty"`
}

func (s *Server) BuildContributorPoolStatus() *ContributorPoolStatus {
	profiles := listContributorProfiles()
	active := 0
	var byRole map[string]int
	if s.contributeHub != nil {
		active = s.contributeHub.ActiveCount()
		byRole = s.contributeHub.RoleBreakdown()
	}
	return &ContributorPoolStatus{
		Active:     active,
		Registered: len(profiles),
		ByRole:     byRole,
	}
}

func (s *Server) registerContributeRoutes() {
	s.contributeHub = NewContributeWSHub(s.logger, s)
	s.mux.HandleFunc("GET /contribute", s.handleContributeLanding)
	s.mux.HandleFunc("GET /api/contribute/ws", s.contributeHub.HandleWS)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeRegister)
	s.mux.HandleFunc("POST /api/contribute/reissue-token", s.handleContributeReissueToken)
	s.mux.HandleFunc("GET /api/contribute/status", s.handleContributeStatus)
	s.mux.HandleFunc("GET /api/contribute/activity", s.handleContributeActivity)
	s.mux.HandleFunc("GET /api/contributors", s.handleContributorsList)
	s.mux.HandleFunc("GET /api/contributors/{id}", s.handleContributorGet)
	s.mux.HandleFunc("PUT /api/contributors/{id}/trust", s.handleContributorTrust)
	s.mux.HandleFunc("POST /api/contributors/{id}/revoke", s.handleContributorRevoke)
	s.mux.HandleFunc("DELETE /api/contributors/{id}", s.handleContributorDelete)

	s.mux.HandleFunc("GET /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("POST /api/v1/", s.handleAPIv1)
	s.mux.HandleFunc("GET /api/docs", s.handleAPIDocs)

	s.mux.HandleFunc("GET /leaderboard", s.handleLeaderboardPage)
	s.mux.HandleFunc("GET /api/leaderboard", s.handleLeaderboardAPI)

	s.mux.HandleFunc("GET /api/hives", s.handleHivesList)
	s.mux.HandleFunc("POST /api/hives/register", s.handleHivesRegister)
	s.mux.HandleFunc("POST /api/hives/{id}/heartbeat", s.handleHivesHeartbeat)
	s.mux.HandleFunc("DELETE /api/hives/{id}", s.handleHivesDelete)
	s.mux.HandleFunc("POST /api/hives/onboard", s.handleHivesOnboard)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ensureDir(dir string) {
	_ = os.MkdirAll(dir, 0o755)
}

func loadContributorProfile(username string) (*ContributorProfile, error) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil, fmt.Errorf("invalid username")
	}
	data, err := os.ReadFile(filepath.Join(getContributorsDir(), username+".json"))
	if err != nil {
		return nil, err
	}
	var p ContributorProfile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func saveContributorProfile(p *ContributorProfile) error {
	if strings.Contains(p.GitHubUsername, "..") || strings.Contains(p.GitHubUsername, "/") || strings.Contains(p.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save")
	}
	ensureDir(getContributorsDir())
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func listContributorProfiles() []ContributorProfile {
	ensureDir(getContributorsDir())
	entries, err := os.ReadDir(getContributorsDir())
	if err != nil {
		return nil
	}
	var profiles []ContributorProfile
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(getContributorsDir(), e.Name()))
		if err != nil {
			continue
		}
		var p ContributorProfile
		if json.Unmarshal(data, &p) == nil && p.GitHubUsername != "" && p.ContributorID != "" {
			profiles = append(profiles, p)
		}
	}
	return profiles
}

func createContributorProfile(username string) (*ContributorProfile, string) {
	cid := "c-" + randomHex(6)
	token := randomHex(32)
	p := &ContributorProfile{
		GitHubUsername:    username,
		ContributorID:     cid,
		RegistrationToken: sha256Hex(token),
		TokenPlain:        token,
		TrustTier:         "newcomer",
		RegisteredAt:      time.Now().UTC().Format(time.RFC3339),
		RateLimits: ContributorRateLimits{
			MaxConcurrent: 1,
			MaxPerHour:    3,
			MaxPerDay:     10,
		},
	}
	_ = saveContributorProfile(p)
	return p, token
}

func findContributor(id string) *ContributorProfile {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return nil
	}
	// Fast path: try direct file lookup by username (O(1) disk read)
	if p, err := loadContributorProfile(id); err == nil {
		return p
	}
	// Slow path: scan all profiles to match by contributor_id
	profiles := listContributorProfiles()
	for i := range profiles {
		if profiles[i].ContributorID == id {
			return &profiles[i]
		}
	}
	return nil
}

// ── Landing page ───────────────────────────────────────────────────────────

func (s *Server) handleContributeLanding(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	projectName := ""
	if s.deps != nil && s.deps.Config != nil {
		projectName = s.deps.Config.Project.Name
	}
	projectName = html.EscapeString(projectName)
	if projectName == "" {
		projectName = "Hive"
	}

	// Count profiles by trust tier and active status
	activeCount := 0
	if s.contributeHub != nil {
		activeCount = s.contributeHub.ActiveCount()
	}
	tierCounts := map[string]int{
		"newcomer":    0,
		"contributor": 0,
		"trusted":     0,
		"advisor":     0,
		"revoked":     0,
	}
	for _, p := range profiles {
		tierCounts[p.TrustTier]++
	}

	// Build tier stat boxes HTML
	type tierStat struct {
		label string
		color string
		count int
	}
	tierStats := []tierStat{
		{"Active", "#3fb950", activeCount},
		{"Newcomer", "#d29922", tierCounts["newcomer"]},
		{"Contributor", "#58a6ff", tierCounts["contributor"]},
		{"Trusted", "#3fb950", tierCounts["trusted"]},
		{"Advisor", "#bc8cff", tierCounts["advisor"]},
		{"Revoked", "#f85149", tierCounts["revoked"]},
	}
	var tierBoxes strings.Builder
	for _, ts := range tierStats {
		fmt.Fprintf(&tierBoxes,
			`<div class="stat"><div class="stat-num" style="color:%s">%d</div><div class="stat-label">%s</div></div>`,
			ts.color, ts.count, ts.label)
	}

	wsProto := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		wsProto = "wss"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	host = strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == ':' || c == '-' {
			return c
		}
		return -1
	}, host)
	hubURL := fmt.Sprintf("%s://%s/contribute", wsProto, host)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Contribute to %s</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;margin:0;min-height:100vh}
.page{display:flex;min-height:100vh;width:100%%}
.main{flex:3;padding:40px 48px;overflow-y:auto}
.sidebar{flex:1;background:#161b22;border-left:1px solid #30363d;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
h1{font-size:2rem;margin-bottom:8px}
.subtitle{color:#8b949e;font-size:1.1rem;margin-bottom:32px}
.stat-row{display:grid;grid-template-columns:repeat(auto-fit,minmax(80px,1fr));gap:10px;margin-bottom:24px}
.stat{background:#161b22;border:1px solid #30363d;border-radius:10px;padding:14px 8px;text-align:center}
.stat-num{font-size:1.5rem;font-weight:700;color:#58a6ff}
.stat-label{font-size:.7rem;color:#8b949e;margin-top:4px}
.steps{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:24px;margin-top:24px}
.steps h3{margin-top:0;color:#58a6ff}
.steps ol{padding-left:20px;line-height:2}
code{background:#0d1117;padding:2px 8px;border-radius:4px;font-size:.9rem}
.how{margin-top:32px}
.how h3{color:#e6edf3}
.how p{color:#8b949e;line-height:1.6}
.tier-table{width:100%%;border-collapse:collapse;margin-top:16px}
.tier-table th,.tier-table td{padding:8px 12px;text-align:left;border-bottom:1px solid #30363d;font-size:.85rem}
.tier-table th{color:#8b949e;font-weight:600}
.feed-header{padding:20px 20px 12px;border-bottom:1px solid #30363d;display:flex;align-items:center;gap:8px}
.feed-header h3{font-size:.95rem;color:#e6edf3}
.feed-dot{width:8px;height:8px;border-radius:50%%;background:#3fb950;animation:pulse 2s infinite}
@keyframes pulse{0%%,100%%{opacity:1}50%%{opacity:.4}}
.feed-count{font-size:.75rem;color:#8b949e;margin-left:auto}
.feed-scroll{flex:1;overflow-y:auto;padding:0}
.feed-entry{padding:10px 20px;border-bottom:1px solid #21262d;font-size:.85rem;animation:fadeIn .3s ease;display:flex;align-items:flex-start;gap:12px}
@keyframes fadeIn{from{opacity:0;transform:translateY(-4px)}to{opacity:1;transform:translateY(0)}}
.feed-entry:hover{background:rgba(88,166,255,.04)}
.feed-text{flex:1;min-width:0}
.feed-time{color:#8b949e;font-size:.75rem;white-space:nowrap;flex-shrink:0}
.feed-role{color:#58a6ff;font-weight:500}
.feed-cli{color:#8b949e;font-size:.8rem}
.feed-empty{padding:40px 20px;text-align:center;color:#8b949e;font-size:.85rem}
@media(max-width:768px){.page{flex-direction:column}.sidebar{border-left:none;border-top:1px solid #30363d;max-width:none;max-height:300px}}
</style></head><body>
<div class="page">
<div class="main">
<h1>🐝 Contribute to %s</h1>
<p class="subtitle">Donate your CLI + API tokens to help this project's AI agent swarm.</p>
<div class="stat-row">
<div class="stat"><div class="stat-num" style="color:#58a6ff">%d</div><div class="stat-label">Total</div></div>
%s
</div>
<div class="steps">
<h3>How it works</h3>
<div style="margin-bottom:16px;display:flex;align-items:center;gap:16px;flex-wrap:wrap">
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Choose your CLI:</label>
<select id="cli-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="claude" data-install="npm i -g @anthropic-ai/claude-code" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="">Claude Code</option>
<option value="copilot" data-install="" data-host-install="" data-model-flag="--model" data-default-model="">GitHub Copilot</option>
<option value="pi" data-install="" data-host-install="curl -fsSL https://pi.dev/install.sh | sh" data-model-flag="--model" data-default-model="">Pi</option>
<option value="goose" data-install="" data-host-install="# Install Goose: https://github.com/block/goose/releases\n# Install Ollama: https://ollama.com/download\nollama pull llama3.2:3b\nexport GOOSE_PROVIDER=ollama GOOSE_MODEL=llama3.2:3b" data-model-flag="" data-default-model="">Goose</option>
<option value="litellm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# Your own LiteLLM proxy — exported locally, never sent to the hive\nexport HIVE_LITELLM_ENDPOINT=https://your-litellm-host:4000\nexport HIVE_LITELLM_API_KEY=sk-your-litellm-key  # only if your proxy needs one">LiteLLM (Claude Code + your proxy)</option>
<option value="openrouter" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# OpenRouter — Claude Code routed through OpenRouter\nexport HIVE_LITELLM_ENDPOINT=https://openrouter.ai/api/v1\nexport HIVE_LITELLM_API_KEY=sk-or-...  # your OpenRouter key">OpenRouter (Claude Code + your key)</option>
<option value="vllm" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# vLLM — self-hosted OpenAI-compatible server\nexport HIVE_LITELLM_ENDPOINT=http://your-vllm-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-vllm-key  # only if your server needs one">vLLM (self-hosted)</option>
<option value="llm-d" data-install="" data-host-install="npm i -g @anthropic-ai/claude-code" data-model-flag="--model" data-default-model="" data-env="# llm-d — self-hosted OpenAI-compatible endpoint\nexport HIVE_LITELLM_ENDPOINT=http://your-llm-d-host:8000/v1\nexport HIVE_LITELLM_API_KEY=sk-your-llm-d-key  # only if your endpoint needs one">llm-d (self-hosted)</option>
<option value="bob" data-install="" data-host-install="npm i -g bobshell" data-model-flag="" data-default-model="">Bob</option>
<option value="other" data-install="" data-host-install="# Install your CLI tool" data-model-flag="" data-default-model="">Other (host only)</option>
</select>
</span>
<span style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Mode:</label>
<select id="mode-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="containerized">Containerized (recommended)</option>
<option value="host">Host (non-containerized)</option>
</select>
</span>
<span id="runtime-group" style="display:inline-flex;align-items:center;gap:8px;white-space:nowrap">
<label style="font-size:.9rem;color:#8b949e">Runtime:</label>
<select id="runtime-select" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.9rem;cursor:pointer">
<option value="">Auto-detect</option>
<option value="docker">Docker</option>
<option value="podman">Podman</option>
</select>
</span>
</div>
<div id="model-row" style="margin-bottom:12px;display:none;align-items:center;gap:8px">
<label style="font-size:.9rem;color:#8b949e">Model (optional):</label>
<input id="model-input" type="text" placeholder="e.g. claude-sonnet-4-6, gpt-4o" style="background:#161b22;color:#e6edf3;border:1px solid #30363d;border-radius:6px;padding:6px 12px;font-size:.85rem;flex:1;max-width:300px" oninput="updateCmds()">
</div>
<p style="color:#8b949e;margin-bottom:8px">Copy and paste these commands to get started:</p>
<div style="margin-top:16px;background:#0d1117;border:1px solid #30363d;border-radius:8px;padding:16px;position:relative">
<button id="copy-btn" style="position:absolute;top:8px;right:8px;background:#238636;color:#fff;border:none;border-radius:4px;padding:4px 12px;cursor:pointer;font-size:.75rem">Copy</button>
<pre id="copy-cmds" style="color:#e6edf3;font-size:.85rem;margin:0;overflow-x:auto;white-space:pre">brew install just gh
git clone -b v2 https://github.com/kubestellar/hive && cd hive
export HIVE_HUB=%s
just contribute-setup claude
just contribute-hive</pre>
</div>
<script>
(function(){
var sel=document.getElementById('cli-select');
var modeSel=document.getElementById('mode-select');
var runtimeSel=document.getElementById('runtime-select');
var runtimeGroup=document.getElementById('runtime-group');
var cmds=document.getElementById('copy-cmds');
var hubURL='%s';
var containerTpl='brew install just gh\ngit clone -b v2 https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\njust contribute-setup CLI\njust contribute-hive';
var hostTpl='brew install just gh\nINSTALL\ngit clone -b v2 https://github.com/kubestellar/hive && cd hive\nexport HIVE_HUB='+hubURL+'\njust contribute-setup CLI\njust contribute-hive CLI local';
var modelRow=document.getElementById('model-row');
var modelInput=document.getElementById('model-input');
function updateCmds(){update();}
function update(){
var cli=sel.value;
var opt=sel.options[sel.selectedIndex];
var mode=modeSel.value;
var modelFlag=opt.getAttribute('data-model-flag')||'';
var model=(modelInput.value||'').trim();
if(cli==='other')mode='host';
if(mode==='containerized'&&cli==='other'){modeSel.value='host';mode='host';}
modelRow.style.display=(modelFlag||cli==='goose')?'flex':'none';
var modelLine='';
if(model){
if(cli==='goose'){modelLine='export GOOSE_MODEL='+model+'\n';}
else if(modelFlag){modelLine='export AGENT_MODEL='+model+'\n';}
}
var envLines=(opt.getAttribute('data-env')||'').replace(/\\n/g,'\n');
if(envLines)envLines+='\n';
// Runtime selector only applies to containerized mode; the export line is
// injected into the copy-paste commands so the choice is explicit.
var showRuntime=(mode==='containerized');
runtimeGroup.style.display=showRuntime?'inline-flex':'none';
var runtimeLine=(showRuntime&&runtimeSel.value)?'export HIVE_CONTAINER_RUNTIME='+runtimeSel.value+'\n':'';
var preLines=envLines+modelLine+runtimeLine;
var tpl,install;
if(mode==='host'){
tpl=hostTpl;
install=opt.getAttribute('data-host-install');
if(!install)install='# '+cli+' uses your existing gh auth';
cmds.textContent=tpl.replace('INSTALL',install.replace(/\\n/g,'\n')).replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
}else{
cmds.textContent=containerTpl.replace(/CLI/g,cli).replace('just contribute-setup',preLines+'just contribute-setup');
}
}
sel.addEventListener('change',function(){modelInput.value='';update();});
modeSel.addEventListener('change',update);
runtimeSel.addEventListener('change',update);
document.getElementById('copy-btn').addEventListener('click',function(){
var el=document.getElementById('copy-cmds');
var btn=document.getElementById('copy-btn');
var range=document.createRange();
range.selectNodeContents(el);
var sel=window.getSelection();
sel.removeAllRanges();
sel.addRange(range);
var ok=false;
try{ok=document.execCommand('copy')}catch(e){}
if(!ok&&navigator.clipboard){navigator.clipboard.writeText(el.textContent.trim()).catch(function(){});ok=true}
btn.textContent=ok?'Copied!':'Select + Cmd+C';
btn.style.background='#16a34a';
setTimeout(function(){btn.textContent='Copy';btn.style.background='#238636'},2000);
});
})();
</script>
</div>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Containerized mode auto-detects docker, then podman &mdash; force one with <code>export HIVE_CONTAINER_RUNTIME=podman</code>. Rootless podman (keep-id, SELinux labels) is handled automatically.</p>
<p style="color:#6e7681;font-size:.78rem;margin-top:8px">Don't see your CLI? <a href="https://github.com/kubestellar/hive/issues/new?title=CLI+request:+&labels=contribute,enhancement" target="_blank" style="color:#58a6ff">Open an issue</a> and we'll add support for it.</p>
<div style="margin-top:20px;display:flex;gap:12px;flex-wrap:wrap">
<a href="/leaderboard" style="display:inline-block;padding:8px 20px;background:#161b22;border:1px solid #30363d;border-radius:8px;color:#58a6ff;text-decoration:none;font-size:.9rem">🏆 View Leaderboard</a>
</div>
<div class="how">
<h3>What you bring vs. what the hive provides</h3>
<p><strong>You bring:</strong> Your GitHub account + CLI API tokens. With LiteLLM you bring your own proxy instead — Claude Code on your machine talks directly to your endpoint, and the hive never sees your endpoint or key. Issues and PRs are created under YOUR name.</p>
<p><strong>The hive provides:</strong> Work queue, task assignment, and coordination. Your credentials never leave your machine.</p>
</div>
<div class="how">
<h3>Trust tiers</h3>
<table class="tier-table">
<tr><th>Tier</th><th>Unlocked at</th><th>Can do</th></tr>
<tr><td>Newcomer</td><td>Registration</td><td>Comment on issues</td></tr>
<tr><td>Contributor</td><td>5 completed tasks</td><td>Create PRs, push code</td></tr>
<tr><td>Trusted</td><td>20 tasks + maintainer voucher</td><td>Merge PRs</td></tr>
<tr><td>Advisor</td><td>Registration</td><td>Review agent PRs</td></tr>
</table>
</div>
</div>
<div class="sidebar">
<div class="feed-header">
<span class="feed-dot"></span>
<h3>Live Activity</h3>
<span class="feed-count" id="feed-count"></span>
</div>
<div class="feed-scroll" id="activity-feed">
<div class="feed-empty">Watching for contributors...</div>
</div>
</div>
</div>
<script>
let prevCount=0;
async function poll(){try{
const[statusRes,actRes]=await Promise.all([fetch('/api/contribute/status'),fetch('/api/contribute/activity')]);
const status=await statusRes.json();
const act=await actRes.json();
document.getElementById('feed-count').textContent=(act.activity||[]).length+' events';
const f=document.getElementById('activity-feed');
if(!act.activity||!act.activity.length){f.innerHTML='<div class="feed-empty">No activity yet — be the first to contribute!</div>';return}
const newCount=act.activity.length;
const isNew=newCount>prevCount;
prevCount=newCount;
const html=act.activity.slice().reverse().map((e,i)=>{
const d=new Date(e.timestamp);const t=d.toLocaleTimeString([],{hour:'numeric',minute:'2-digit'});const tz=d.toLocaleTimeString([],{timeZoneName:'short'}).split(' ').pop();
const icons={joined:'🟢',left:'🔴','picked up':'🔧',completed:'✅',failed:'❌'};
const verbs={joined:'entered the hive',left:'left the hive','picked up':'picked up','completed':'completed','failed':'failed'};
const icon=icons[e.action]||'⚡';
const verb=verbs[e.action]||e.action;
const taskInfo=e.task?' <span class="feed-cli">'+e.task+'</span>':'';
const role=e.role?' as <span class="feed-role">'+e.role+'</span>':'';
const cliModel=e.cli?(e.model?' <span class="feed-cli">via '+e.cli+' CLI with '+e.model+'</span>':' <span class="feed-cli">via '+e.cli+' CLI</span>'):'';
return '<div class="feed-entry"'+(i===0&&isNew?' style="background:rgba(63,185,80,.08)"':'')+'>'+
'<div class="feed-text">'+icon+' <b>'+e.username+'</b> '+verb+taskInfo+role+cliModel+'</div>'+
'<span class="feed-time">'+t+' '+tz+'</span></div>'
}).join('');
if(f.innerHTML!==html){f.innerHTML=html;if(isNew)f.scrollTop=0;}
}catch(e){}}
poll();setInterval(poll,3000);
</script>
<div style="margin-top:40px;padding:16px 0;border-top:1px solid #30363d;font-size:.75rem;color:#8b949e;display:flex;align-items:center;gap:8px">
  <span id="hive-version">loading...</span>
</div>
<script>
fetch('/api/version').then(function(r){return r.json()}).then(function(d){
  var el=document.getElementById('hive-version');
  var dot=d.behind?'\u{1F7E1}':'\u{1F7E2}';
  el.innerHTML=dot+' Hive v'+d.version+' ('+d.short+')' + (d.behind?' · <span style="color:#d29922">update available</span>':' · up to date');
}).catch(function(){});
</script>
</body></html>`, projectName, projectName, len(profiles), tierBoxes.String(), hubURL, hubURL)
}

// ── Registration ───────────────────────────────────────────────────────────

const maxRequestBodyBytes = 4096

func (s *Server) handleContributeRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GitHubUsername string `json:"github_username"`
		Force          bool   `json:"force"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.GitHubUsername)
	if username == "" || !isValidUsername(username) {
		jsonError(w, "Invalid github_username", http.StatusBadRequest)
		return
	}

	const maxContributors = 500
	if len(listContributorProfiles()) >= maxContributors {
		jsonError(w, "contributor registration full — contact the hive administrator", http.StatusServiceUnavailable)
		return
	}

	existing, _ := loadContributorProfile(username)
	if existing != nil {
		if existing.TrustTier == "revoked" {
			jsonError(w, "Account revoked — contact the hive administrator to reinstate", http.StatusForbidden)
			return
		}
		// SECURITY: this endpoint is unauthenticated (username is self-asserted),
		// so it must NEVER reissue and return an existing contributor's token —
		// that was an account-takeover primitive (POST any known username with
		// force:true → receive their live token). Reissuing requires proving you
		// own the GitHub account, which POST /api/contribute/reissue-token does
		// (it validates a GitHub token). The legacy force:true flag is ignored.
		_ = req.Force
		jsonResponse(w, map[string]string{
			"contributor_id": existing.ContributorID,
			"message":        "Already registered — to rotate your token, POST /api/contribute/reissue-token with Authorization: Bearer <your GitHub token>",
		})
		return
	}

	profile, token := createContributorProfile(username)
	s.logger.Info("contributor registered", "username", username, "id", profile.ContributorID)

	// Clear plaintext token from disk — only the hash is needed for auth
	profile.TokenPlain = ""
	_ = saveContributorProfile(profile)

	jsonResponse(w, map[string]string{
		"contributor_id":     profile.ContributorID,
		"registration_token": token,
		"message":            "Registered successfully — save this token, it cannot be recovered",
	})
}

// reissueContributorToken generates a new registration token for an existing
// contributor, invalidating the previous one. Returns the new plaintext token.
func reissueContributorToken(p *ContributorProfile) string {
	const tokenBytes = 32 // 256-bit token
	newToken := randomHex(tokenBytes)
	p.RegistrationToken = sha256Hex(newToken)
	p.TokenPlain = ""
	_ = saveContributorProfile(p)
	return newToken
}

// handleContributeReissueToken lets a contributor recover access by proving
// ownership of their GitHub identity. Requires Authorization: Bearer <gh-token>.
func (s *Server) handleContributeReissueToken(w http.ResponseWriter, r *http.Request) {
	// Authenticate via GitHub personal access token
	token := r.Header.Get("Authorization")
	if strings.HasPrefix(token, "Bearer ") {
		const bearerPrefixLen = 7 // len("Bearer ")
		token = token[bearerPrefixLen:]
	} else if strings.HasPrefix(token, "token ") {
		const tokenPrefixLen = 6 // len("token ")
		token = token[tokenPrefixLen:]
	} else {
		token = ""
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.ResolvedAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-personal-access-token>"}`))
		return
	}

	profile, _ := loadContributorProfile(username)
	if profile == nil {
		jsonError(w, "Not registered as a contributor — register first via POST /api/contribute/register", http.StatusNotFound)
		return
	}
	if profile.TrustTier == "revoked" {
		jsonError(w, "Account revoked — contact the hive administrator to reinstate", http.StatusForbidden)
		return
	}

	newToken := reissueContributorToken(profile)
	s.logger.Info("contributor token reissued via GitHub auth", "username", username, "id", profile.ContributorID)

	jsonResponse(w, map[string]string{
		"contributor_id":     profile.ContributorID,
		"registration_token": newToken,
		"message":            "Token reissued — save this new token, it replaces the previous one",
	})
}

func (s *Server) handleContributeStatus(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	active := 0
	if s.contributeHub != nil {
		active = s.contributeHub.ActiveCount()
	}
	actionable := 0
	s.statusMu.RLock()
	if s.status != nil {
		for _, repo := range s.status.Repos {
			actionable += len(repo.ActionableIssues)
		}
	}
	s.statusMu.RUnlock()
	jsonResponse(w, map[string]any{
		"hub":                 "online",
		"active_contributors": active,
		"total_registered":    len(profiles),
		"actionable_items":    actionable,
	})
}

func (s *Server) handleContributeActivity(w http.ResponseWriter, r *http.Request) {
	if s.contributeHub == nil {
		jsonResponse(w, map[string]any{"activity": []any{}})
		return
	}
	jsonResponse(w, map[string]any{"activity": s.contributeHub.RecentActivity()})
}

// ── Contributor management ─────────────────────────────────────────────────

func (s *Server) handleContributorsList(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	var liveStates map[string]ContributorLiveState
	if s.contributeHub != nil {
		liveStates = s.contributeHub.LiveStates()
	}
	for i := range profiles {
		profiles[i].TokenPlain = ""
		profiles[i].RegistrationToken = ""
		if ls, ok := liveStates[profiles[i].ContributorID]; ok {
			profiles[i].Active = ls.Active
			profiles[i].CurrentTask = ls.CurrentTask
			profiles[i].ActiveTasks = ls.Tasks
			profiles[i].Sessions = ls.Sessions
		}
	}
	jsonResponse(w, map[string]any{"contributors": profiles})
}

func (s *Server) handleContributorGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TokenPlain = ""
	p.RegistrationToken = ""
	jsonResponse(w, p)
}

func (s *Server) handleContributorTrust(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	validTiers := map[string]bool{"newcomer": true, "contributor": true, "trusted": true, "advisor": true, "revoked": true}
	if !validTiers[req.Tier] {
		jsonError(w, "Invalid tier", http.StatusBadRequest)
		return
	}
	p.TrustTier = req.Tier
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor tier changed", "username", p.GitHubUsername, "tier", req.Tier)
	jsonResponse(w, map[string]any{"ok": true, "trust_tier": req.Tier})
}

func (s *Server) handleContributorRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TrustTier = "revoked"
	_ = saveContributorProfile(p)
	s.logger.Info("contributor revoked", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) handleContributorDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	if err := os.Remove(path); err != nil {
		jsonError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor deleted", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true, "deleted": p.GitHubUsername})
}

// ── Federation registry ────────────────────────────────────────────────────

type FederationRegistry struct {
	Hives []FederationHive `json:"hives"`
}

type FederationHive struct {
	ID                 string `json:"id"`
	ProjectName        string `json:"project_name"`
	Org                string `json:"org"`
	HubURL             string `json:"hub_url"`
	DashboardURL       string `json:"dashboard_url,omitempty"`
	ActiveContributors int    `json:"active_contributors"`
	ActiveAgents       int    `json:"active_agents"`
	ActionableItems    int    `json:"actionable_items"`
	RegisteredAt       string `json:"registered_at"`
	LastHeartbeat      string `json:"last_heartbeat,omitempty"`
}

func loadFederationRegistry() *FederationRegistry {
	data, err := os.ReadFile(getFederationRegistryPath())
	if err != nil {
		return &FederationRegistry{}
	}
	var reg FederationRegistry
	if json.Unmarshal(data, &reg) != nil {
		return &FederationRegistry{}
	}
	return &reg
}

func saveFederationRegistry(reg *FederationRegistry) error {
	path := getFederationRegistryPath()
	ensureDir(filepath.Dir(path))
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (s *Server) handleHivesList(w http.ResponseWriter, r *http.Request) {
	reg := loadFederationRegistry()
	jsonResponse(w, reg)
}

func (s *Server) handleHivesRegister(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req struct {
		ProjectName  string `json:"project_name"`
		Org          string `json:"org"`
		HubURL       string `json:"hub_url"`
		DashboardURL string `json:"dashboard_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if req.ProjectName == "" || req.Org == "" || req.HubURL == "" {
		jsonError(w, "project_name, org, and hub_url are required", http.StatusBadRequest)
		return
	}
	validURLScheme := func(u string) bool {
		return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") ||
			strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://")
	}
	if !validURLScheme(req.HubURL) {
		jsonError(w, "hub_url must start with http://, https://, ws://, or wss://", http.StatusBadRequest)
		return
	}
	if req.DashboardURL != "" && !validURLScheme(req.DashboardURL) {
		jsonError(w, "dashboard_url must start with http://, https://, ws://, or wss://", http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), req.HubURL) {
		jsonError(w, "hub_url must not target private/internal addresses", http.StatusBadRequest)
		return
	}
	if req.DashboardURL != "" && isPrivateURL(r.Context(), req.DashboardURL) {
		jsonError(w, "dashboard_url must not target private/internal addresses", http.StatusBadRequest)
		return
	}

	reg := loadFederationRegistry()
	const maxFederationHives = 100
	hiveID := fmt.Sprintf("hive-%s-%s", strings.ToLower(req.Org), strings.ToLower(req.ProjectName))
	for i := range reg.Hives {
		if reg.Hives[i].ID == hiveID {
			reg.Hives[i].HubURL = req.HubURL
			reg.Hives[i].DashboardURL = req.DashboardURL
			_ = saveFederationRegistry(reg)
			jsonResponse(w, map[string]any{"ok": true, "id": hiveID, "updated": true})
			return
		}
	}

	if len(reg.Hives) >= maxFederationHives {
		jsonError(w, "federation registry full", http.StatusServiceUnavailable)
		return
	}

	reg.Hives = append(reg.Hives, FederationHive{
		ID:           hiveID,
		ProjectName:  req.ProjectName,
		Org:          req.Org,
		HubURL:       req.HubURL,
		DashboardURL: req.DashboardURL,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	})
	_ = saveFederationRegistry(reg)
	s.logger.Info("hive registered", "id", hiveID)
	jsonResponse(w, map[string]any{"ok": true, "id": hiveID})
}

func (s *Server) handleHivesHeartbeat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	reg := loadFederationRegistry()
	var found *FederationHive
	for i := range reg.Hives {
		if reg.Hives[i].ID == id {
			found = &reg.Hives[i]
			break
		}
	}
	if found == nil {
		jsonError(w, "Hive not found", http.StatusNotFound)
		return
	}

	var req struct {
		ActiveContributors int `json:"active_contributors"`
		ActiveAgents       int `json:"active_agents"`
		ActionableItems    int `json:"actionable_items"`
	}
	const maxFedCount = 10000
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		if req.ActiveContributors >= 0 && req.ActiveContributors <= maxFedCount {
			found.ActiveContributors = req.ActiveContributors
		}
		if req.ActiveAgents >= 0 && req.ActiveAgents <= maxFedCount {
			found.ActiveAgents = req.ActiveAgents
		}
		if req.ActionableItems >= 0 && req.ActionableItems <= maxFedCount {
			found.ActionableItems = req.ActionableItems
		}
	}
	found.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
	_ = saveFederationRegistry(reg)
	jsonResponse(w, map[string]any{"ok": true})
}

func (s *Server) handleHivesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reg := loadFederationRegistry()
	for i := range reg.Hives {
		if reg.Hives[i].ID == id {
			reg.Hives = append(reg.Hives[:i], reg.Hives[i+1:]...)
			_ = saveFederationRegistry(reg)
			jsonResponse(w, map[string]any{"ok": true})
			return
		}
	}
	jsonError(w, "Hive not found", http.StatusNotFound)
}

func (s *Server) handleHivesOnboard(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req struct {
		ProjectName string   `json:"project_name"`
		Org         string   `json:"org"`
		Repos       []string `json:"repos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProjectName == "" || req.Org == "" || len(req.Repos) == 0 {
		jsonError(w, "project_name, org, and repos[] are required", http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]any{
		"next_steps": []string{
			"1. Install the Hive GitHub App on your org",
			"2. Note the App ID and Installation ID",
			"3. Save the private key as /etc/hive/gh-app-key.pem",
			"4. Deploy with: docker compose up -d",
			"5. Register: POST /api/hives/register",
		},
	})
}

// ── Leaderboard ───────────────────────────────────────────────────────────

// LeaderboardEntry is the JSON shape returned by the leaderboard API.
type LeaderboardEntry struct {
	Rank           int    `json:"rank"`
	GitHubUsername string `json:"github_username"`
	AvatarURL      string `json:"avatar_url"`
	TrustTier      string `json:"trust_tier"`
	TasksCompleted int    `json:"tasks_completed"`
	TasksFailed    int    `json:"tasks_failed"`
	Findings       int    `json:"findings,omitempty"`
	RegisteredAt   string `json:"registered_at"`
	Active         bool   `json:"active,omitempty"`
	CurrentTask    string `json:"current_task,omitempty"`
	IsAgent        bool   `json:"is_agent,omitempty"`
	Emoji          string `json:"emoji,omitempty"`
}

// buildLeaderboard loads all contributor profiles, sorts by tasks completed
// descending, and returns ranked entries with secrets stripped.
func buildLeaderboard() []LeaderboardEntry {
	profiles := listContributorProfiles()
	sort.Slice(profiles, func(i, j int) bool {
		return profiles[i].TasksCompleted > profiles[j].TasksCompleted
	})

	entries := make([]LeaderboardEntry, 0, len(profiles))
	rank := 0
	for _, p := range profiles {
		// Revoked contributors should not appear on the leaderboard.
		if p.TrustTier == "revoked" {
			continue
		}
		rank++
		entries = append(entries, LeaderboardEntry{
			Rank:           rank,
			GitHubUsername: p.GitHubUsername,
			AvatarURL:      fmt.Sprintf("https://github.com/%s.png", p.GitHubUsername),
			TrustTier:      p.TrustTier,
			TasksCompleted: p.TasksCompleted,
			TasksFailed:    p.TasksFailed,
			RegisteredAt:   p.RegisteredAt,
		})
	}
	return entries
}

func (s *Server) handleLeaderboardAPI(w http.ResponseWriter, _ *http.Request) {
	contributors := buildLeaderboard()
	agents := s.buildAgentLeaderboardEntries()
	jsonResponse(w, map[string]any{
		"leaderboard": contributors,
		"agents":      agents,
	})
}

func (s *Server) ContributorSummary() (registered, active int) {
	profiles := listContributorProfiles()
	registered = len(profiles)
	if s.contributeHub != nil {
		for _, ls := range s.contributeHub.LiveStates() {
			if ls.Active {
				active++
			}
		}
	}
	return
}

func (s *Server) LeaderboardForHub() []LeaderboardEntry {
	entries := buildLeaderboard()
	if s.contributeHub != nil {
		liveStates := s.contributeHub.LiveStates()
		profiles := listContributorProfiles()
		liveByUsername := make(map[string]ContributorLiveState)
		for _, p := range profiles {
			if ls, ok := liveStates[p.ContributorID]; ok {
				liveByUsername[p.GitHubUsername] = ls
			}
		}
		for i := range entries {
			if ls, ok := liveByUsername[entries[i].GitHubUsername]; ok {
				entries[i].Active = ls.Active
				if ls.CurrentTask != nil {
					entries[i].CurrentTask = ls.CurrentTask.Title
				}
			}
		}
	}
	agentEntries := s.buildAgentLeaderboardEntries()
	entries = append(agentEntries, entries...)
	for i := range entries {
		entries[i].Rank = i + 1
	}
	return entries
}

// trustTierColor maps trust tiers to CSS colour values for badges.
func trustTierColor(tier string) string {
	switch tier {
	case "newcomer":
		return "#8b949e"
	case "contributor":
		return "#3fb950"
	case "trusted":
		return "#d29922"
	case "advisor":
		return "#a371f7"
	case "revoked":
		return "#f85149"
	default:
		return "#8b949e"
	}
}

// trustTierBadgeCSS returns Tailwind-style bg/text/border CSS classes for a tier.
func trustTierBadgeCSS(tier string) (bg, text, border string) {
	switch tier {
	case "newcomer":
		return "rgba(107,114,128,0.2)", "#9ca3af", "rgba(107,114,128,0.3)"
	case "contributor":
		return "rgba(59,130,246,0.2)", "#60a5fa", "rgba(59,130,246,0.3)"
	case "trusted":
		return "rgba(34,197,94,0.2)", "#4ade80", "rgba(34,197,94,0.3)"
	case "advisor":
		return "rgba(168,85,247,0.2)", "#c084fc", "rgba(168,85,247,0.3)"
	case agentTierLabel:
		return "rgba(147,51,234,0.2)", "#a78bfa", "rgba(147,51,234,0.3)"
	case "revoked":
		return "rgba(239,68,68,0.2)", "#f87171", "rgba(239,68,68,0.3)"
	default:
		return "rgba(107,114,128,0.2)", "#9ca3af", "rgba(107,114,128,0.3)"
	}
}

// rankDisplay returns the medal emoji for top 3, or "#N" for others.
func rankDisplay(rank int) string {
	const goldMedal = "\U0001F947"   // gold medal emoji
	const silverMedal = "\U0001F948" // silver medal emoji
	const bronzeMedal = "\U0001F949" // bronze medal emoji
	switch rank {
	case 1:
		return fmt.Sprintf(`<span class="medal" title="1st place">%s</span>`, goldMedal)
	case 2:
		return fmt.Sprintf(`<span class="medal" title="2nd place">%s</span>`, silverMedal)
	case 3:
		return fmt.Sprintf(`<span class="medal" title="3rd place">%s</span>`, bronzeMedal)
	default:
		return fmt.Sprintf(`<span class="rank-num">#%d</span>`, rank)
	}
}

const (
	ghPRExternalRefPrefix    = "gh-"
	agentTierLabel           = "agent"
	agentAvatarURLTemplate   = "https://github.com/identicons/%s.png"
	leaderboardURLPathPrefix = "/leaderboard"
)

func (s *Server) buildAgentLeaderboardEntries() []LeaderboardEntry {
	if s.deps == nil || s.deps.AgentMgr == nil {
		return nil
	}

	agents := s.deps.AgentMgr.AllStatuses()
	entries := make([]LeaderboardEntry, 0, len(agents))

	for name, proc := range agents {
		if !proc.Config.Enabled {
			continue
		}

		prsOpened, issuesFixed, totalFindings := s.countAgentActivity(name)
		tasksCompleted := prsOpened + issuesFixed

		displayName := proc.Config.DisplayName
		if displayName == "" {
			displayName = name
		}

		emoji := proc.Config.Emoji
		if emoji == "" {
			emoji = "\U0001F916"
		}

		entries = append(entries, LeaderboardEntry{
			GitHubUsername: name,
			AvatarURL:      fmt.Sprintf(agentAvatarURLTemplate, name),
			TrustTier:      agentTierLabel,
			TasksCompleted: tasksCompleted,
			TasksFailed:    proc.RestartCount,
			Findings:       totalFindings,
			RegisteredAt:   "",
			IsAgent:        true,
			Emoji:          emoji,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TasksCompleted > entries[j].TasksCompleted
	})

	return entries
}

func (s *Server) countAgentActivity(agentName string) (prs, issues, findings int) {
	if s.deps == nil || s.deps.BeadStores == nil {
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		return
	}

	actor := agentName
	allBeads := store.List(beads.ListFilter{Actor: &actor})
	findings = len(allBeads)
	for _, b := range allBeads {
		if strings.HasPrefix(b.ExternalRef, ghPRExternalRefPrefix) {
			prs++
		}
		if b.Status == beads.StatusDone {
			issues++
		}
	}
	return
}

func (s *Server) handleLeaderboardPage(w http.ResponseWriter, _ *http.Request) {
	contributorEntries := buildLeaderboard()
	agentEntries := s.buildAgentLeaderboardEntries()

	projectName := ""
	if s.deps != nil && s.deps.Config != nil {
		if s.deps.Config.Project.Org != "" && s.deps.Config.Project.PrimaryRepo != "" {
			projectName = s.deps.Config.Project.Org + "/" + s.deps.Config.Project.PrimaryRepo
		} else if s.deps.Config.Project.Name != "" {
			projectName = s.deps.Config.Project.Name
		}
	}
	projectName = html.EscapeString(projectName)
	if projectName == "" {
		projectName = "Hive"
	}

	hiveURL := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveURL = html.EscapeString(s.deps.Config.HiveID)
	}

	rank := 0
	for i := range agentEntries {
		rank++
		agentEntries[i].Rank = rank
	}
	for i := range contributorEntries {
		rank++
		contributorEntries[i].Rank = rank
	}

	totalParticipants := len(agentEntries) + len(contributorEntries)

	agentJSON := leaderboardEntriesToJSON(agentEntries)
	contributorJSON := leaderboardEntriesToJSON(contributorEntries)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, leaderboardHTML,
		projectName, projectName, totalParticipants,
		projectName,
		projectName, hiveURL, hiveURL, projectName, hiveURL,
		agentJSON, contributorJSON,
	)
}

func leaderboardEntriesToJSON(entries []LeaderboardEntry) string {
	var buf strings.Builder
	buf.WriteString("[")
	for i, e := range entries {
		if i > 0 {
			buf.WriteString(",")
		}
		bg, text, border := trustTierBadgeCSS(e.TrustTier)
		emoji := strings.ReplaceAll(e.Emoji, `"`, `\"`)
		buf.WriteString(fmt.Sprintf(
			`{"rank":%d,"login":"%s","avatar":"%s","tier":"%s","completed":%d,"failed":%d,"findings":%d,"registered":"%s","tierBg":"%s","tierText":"%s","tierBorder":"%s","isAgent":%t,"emoji":"%s"}`,
			e.Rank, e.GitHubUsername, e.AvatarURL, e.TrustTier,
			e.TasksCompleted, e.TasksFailed, e.Findings, e.RegisteredAt,
			bg, text, border, e.IsAgent, emoji,
		))
	}
	buf.WriteString("]")
	return buf.String()
}

// leaderboardHTML is the full HTML template for the leaderboard page,
// styled to match the kubestellar.io/leaderboard design.
const leaderboardHTML = `<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s Leaderboard</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#0a0a0a;color:#fff;min-height:100vh;overflow-x:hidden}

/* ── Starfield background ── */
.bg-stars{position:fixed;inset:0;z-index:0;background:#0a0a0a}
.bg-stars::before,.bg-stars::after{content:'';position:absolute;inset:0}
.bg-stars::before{background:radial-gradient(1px 1px at 20px 30px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 40px 70px,rgba(255,255,255,0.2),transparent),
radial-gradient(1px 1px at 50px 160px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 90px 40px,rgba(255,255,255,0.15),transparent),
radial-gradient(1px 1px at 130px 80px,rgba(255,255,255,0.25),transparent),
radial-gradient(1px 1px at 160px 120px,rgba(255,255,255,0.2),transparent);
background-size:200px 200px;animation:twinkle 4s ease-in-out infinite alternate}
.bg-stars::after{background:radial-gradient(1px 1px at 180px 50px,rgba(255,255,255,0.2),transparent),
radial-gradient(1px 1px at 60px 130px,rgba(255,255,255,0.15),transparent),
radial-gradient(1px 1px at 100px 90px,rgba(255,255,255,0.3),transparent),
radial-gradient(1px 1px at 140px 160px,rgba(255,255,255,0.2),transparent);
background-size:300px 300px;animation:twinkle 6s ease-in-out infinite alternate-reverse}
@keyframes twinkle{from{opacity:.5}to{opacity:1}}

/* ── Grid overlay ── */
.bg-grid{position:fixed;inset:0;z-index:1;
background-image:linear-gradient(rgba(255,255,255,0.02) 1px,transparent 1px),
linear-gradient(90deg,rgba(255,255,255,0.02) 1px,transparent 1px);
background-size:80px 80px;pointer-events:none}

.content{position:relative;z-index:10;padding-top:28px}

/* ── Header ── */
.header{text-align:center;padding:48px 16px 32px}
@media(min-width:640px){.header{padding:96px 24px 48px}}
.header h1{font-size:2.25rem;font-weight:700;margin-bottom:12px}
@media(min-width:768px){.header h1{font-size:3rem}}
@media(min-width:1024px){.header h1{font-size:3.75rem}}
.gradient-text{background:linear-gradient(90deg,#9333ea,#3b82f6,#9333ea);
background-size:200%% auto;-webkit-background-clip:text;-webkit-text-fill-color:transparent;
background-clip:text;animation:gradient-shift 3s linear infinite}
@keyframes gradient-shift{from{background-position:0%% center}to{background-position:200%% center}}
.header .subtitle{font-size:1.125rem;color:#d1d5db;max-width:640px;margin:0 auto;line-height:1.6}
@media(min-width:768px){.header .subtitle{font-size:1.5rem}}
.header .meta{margin-top:12px;font-size:.875rem;color:#6b7280}
.header .meta a{color:#9ca3af;text-decoration:none;transition:color .2s}
.header .meta a:hover{color:#60a5fa}
.header .contribute-link{margin-top:24px;display:inline-flex;align-items:center;gap:8px;
padding:8px 16px;border-radius:8px;border:1px solid rgba(245,158,11,0.3);
background:rgba(245,158,11,0.1);color:#fcd34d;font-size:.875rem;text-decoration:none;
transition:background .2s}
.header .contribute-link:hover{background:rgba(245,158,11,0.2)}

/* ── Search ── */
.search-wrap{max-width:448px;margin:0 auto 24px;padding:0 16px}
.search-wrap input{width:100%%;padding:10px 16px;background:rgba(31,41,55,0.6);
backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border:1px solid rgba(255,255,255,0.1);border-radius:8px;color:#fff;font-size:.875rem;
outline:none;transition:border-color .2s,box-shadow .2s}
.search-wrap input::placeholder{color:#6b7280}
.search-wrap input:focus{border-color:rgba(59,130,246,0.5);box-shadow:0 0 0 3px rgba(59,130,246,0.2)}

/* ── Table wrapper ── */
.table-section{max-width:960px;margin:0 auto;padding:0 16px 48px}
.table-wrap{background:rgba(31,41,55,0.4);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:12px;border:1px solid rgba(255,255,255,0.1);overflow:visible}

/* ── Table header ── */
.table-header{display:none;padding:12px 24px;border-bottom:1px solid rgba(255,255,255,0.05);
font-size:.75rem;color:#6b7280;text-transform:uppercase;letter-spacing:.05em}
@media(min-width:640px){.table-header{display:grid;grid-template-columns:1fr 100px 140px 80px;gap:16px}}
.table-header .sortable{cursor:pointer;transition:color .2s;user-select:none}
.table-header .sortable:hover{color:#fff}
.table-header .sortable.active{color:#facc15}

/* ── Row ── */
.row{display:grid;grid-template-columns:1fr;gap:8px;padding:16px;
border-bottom:1px solid rgba(255,255,255,0.05);transition:background .15s;align-items:center}
@media(min-width:640px){.row{grid-template-columns:1fr 100px 140px 80px;gap:16px;padding:16px 24px}}
.row:last-child{border-bottom:none}
.row:hover{background:rgba(255,255,255,0.02)}

/* ── Rank ── */
.rank-cell{display:flex;justify-content:center}
.medal{font-size:1.25rem}
.rank-num{font-size:.875rem;color:#9ca3af;font-variant-numeric:tabular-nums}

/* ── Contributor ── */
.contributor{display:flex;align-items:center;gap:12px}
.contributor img{width:32px;height:32px;border-radius:50%%;flex-shrink:0}
.contributor .name{font-size:.875rem;font-weight:500;color:#fff;text-decoration:none;transition:color .2s}
.contributor .name:hover{color:#60a5fa}
.contributor .gh-icon{color:#4b5563;transition:color .2s;flex-shrink:0}
.contributor .gh-icon:hover{color:#9ca3af}
.contributor .gh-icon svg{width:14px;height:14px}

/* ── Trust tier badge ── */
.tier-badge{display:inline-flex;align-items:center;padding:2px 8px;border-radius:9999px;
font-size:.75rem;font-weight:500;border:1px solid;text-transform:capitalize}

/* ── Stats ── */
.stats-cell{text-align:center;font-variant-numeric:tabular-nums}
.stats-cell .completed{font-weight:600;font-size:.875rem;color:#4ade80}

/* ── Breakdown pills ── */
.pills{display:flex;flex-wrap:wrap;gap:6px}
.pill{padding:2px 8px;border-radius:4px;font-size:.75rem;font-weight:500}
.pill-completed{color:#4ade80;background:rgba(34,197,94,0.1)}
.pill-failed{color:#f87171;background:rgba(239,68,68,0.1)}

/* ── Registered date ── */
.reg-date{font-size:.75rem;color:#6b7280;white-space:nowrap}

/* ── Empty state ── */
.empty-state{text-align:center;padding:64px 16px}
.empty-state .icon{font-size:2.5rem;margin-bottom:16px}
.empty-state p{color:#9ca3af}
.empty-state a{color:#60a5fa;text-decoration:none}
.empty-state a:hover{text-decoration:underline}

/* ── Trust tiers reference ── */
.tiers-ref{max-width:960px;margin:0 auto;padding:0 16px 64px}
.tiers-ref .card{background:rgba(31,41,55,0.3);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:8px;border:1px solid rgba(255,255,255,0.05);padding:24px}
.tiers-ref h3{font-size:.875rem;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:.05em;margin-bottom:12px}
.tiers-grid{display:grid;grid-template-columns:repeat(2,1fr);gap:12px}
@media(min-width:640px){.tiers-grid{grid-template-columns:repeat(5,1fr)}}
.tier-item{display:flex;align-items:center;gap:8px}
.tier-item .tier-dot{width:8px;height:8px;border-radius:50%%}
.tier-item .tier-label{font-size:.875rem;color:#9ca3af}
.tier-item .tier-desc{font-size:.75rem;color:#6b7280}
.tiers-ref .note{margin-top:16px;font-size:.75rem;color:#4b5563}

/* ── Mobile layout ── */
@media(max-width:639px){
  .row .rank-cell{display:inline-flex;margin-right:8px}
  .row .contributor{flex:1}
  .row .stats-cell{text-align:center}
  .row .pills{padding-left:44px}
  .row .reg-date{padding-left:44px}
}

/* ── Hover card ── */
.hover-card-anchor{position:relative}
.hover-card{display:none;position:absolute;left:0;top:100%%;margin-top:8px;z-index:50;
width:320px;background:rgba(17,24,39,0.95);backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px);
border-radius:12px;border:1px solid rgba(255,255,255,0.1);box-shadow:0 25px 50px -12px rgba(0,0,0,0.5);
overflow:hidden;pointer-events:none}
.hover-card-anchor:hover .hover-card{display:block}
.hover-card .hc-header{padding:16px;border-bottom:1px solid rgba(255,255,255,0.05);display:flex;align-items:center;gap:12px}
.hover-card .hc-header img{width:40px;height:40px;border-radius:50%%}
.hover-card .hc-name{font-size:.875rem;font-weight:600;color:#fff}
.hover-card .hc-meta{font-size:.75rem;color:#9ca3af;margin-top:2px}
.hover-card .hc-meta .hc-pts{color:#facc15;font-weight:600}
.hover-card .hc-section{padding:12px 16px;border-bottom:1px solid rgba(255,255,255,0.05)}
.hover-card .hc-label{font-size:10px;text-transform:uppercase;letter-spacing:.05em;color:#6b7280;margin-bottom:6px}
.hover-card .hc-stats{display:flex;gap:16px}
.hover-card .hc-stat{text-align:center;flex:1}
.hover-card .hc-stat-num{font-size:1.25rem;font-weight:700;font-variant-numeric:tabular-nums}
.hover-card .hc-stat-label{font-size:.625rem;color:#6b7280;margin-top:2px;text-transform:uppercase}
.hover-card .hc-bar-wrap{height:6px;background:rgba(255,255,255,0.05);border-radius:3px;overflow:hidden;margin-top:8px}
.hover-card .hc-bar{height:100%%;border-radius:3px;transition:width .3s}
.hover-card .hc-footer{padding:8px 16px;text-align:center;font-size:10px;color:#60a5fa;background:rgba(255,255,255,0.02);border-top:1px solid rgba(255,255,255,0.05)}

/* ── No-results ── */
.no-results{text-align:center;padding:40px 16px;color:#6b7280;font-size:.875rem;display:none}
/* ── Agent row styling ── */
.row.agent-row{background:rgba(147,51,234,0.04);border-left:3px solid rgba(147,51,234,0.3)}
.row.agent-row:hover{background:rgba(147,51,234,0.08)}
.agent-emoji{font-size:1.25rem;margin-right:4px}

/* ── Section header ── */
.section-header{padding:16px 24px 8px;font-size:.75rem;font-weight:600;color:#9ca3af;text-transform:uppercase;letter-spacing:.05em;border-bottom:1px solid rgba(255,255,255,0.05)}

/* ── Invite section ── */
.invite-section{max-width:960px;margin:0 auto;padding:0 16px 32px}
.invite-card{background:rgba(31,41,55,0.4);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);
border-radius:12px;border:1px solid rgba(245,158,11,0.2);padding:32px;text-align:center}
.invite-card h3{font-size:1.125rem;font-weight:600;color:#fcd34d;margin-bottom:8px}
.invite-card p{color:#9ca3af;font-size:.875rem;line-height:1.6;margin-bottom:16px}
.invite-card code{background:rgba(255,255,255,0.08);padding:4px 12px;border-radius:6px;font-size:.875rem;color:#e5e7eb}

/* ── Social share ── */
.share-section{max-width:960px;margin:0 auto;padding:0 16px 48px}
.share-buttons{display:flex;justify-content:center;gap:12px;flex-wrap:wrap}
.share-btn{display:inline-flex;align-items:center;gap:8px;padding:8px 20px;border-radius:8px;
font-size:.875rem;font-weight:500;text-decoration:none;transition:opacity .2s}
.share-btn:hover{opacity:.85}
.share-btn-linkedin{background:#0077b5;color:#fff}
.share-btn-x{background:#1d9bf0;color:#fff}
</style></head>
<body>
<div class="bg-stars"></div>
<div class="bg-grid"></div>
<div class="content">
  <section class="header">
    <h1>%s <span class="gradient-text">Leaderboard</span></h1>
    <p class="subtitle">AI agents and contributors ranked by completed tasks</p>
    <p class="meta">Tracking <strong style="color:#e5e7eb">%d</strong> participants</p>
    <div style="margin-top:24px;display:flex;justify-content:center">
      <a href="/contribute" class="contribute-link">
        <span>&#x1F41D;</span>
        <span><strong>Join the swarm</strong> &mdash; donate your CLI to help autonomous agents maintain repos</span>
      </a>
    </div>
  </section>

  <div class="search-wrap">
    <input type="text" id="search" placeholder="Search agents and contributors..." autocomplete="off">
  </div>

  <section class="table-section">
    <div class="table-wrap">
      <div class="section-header">%s AI Agents</div>
      <div class="table-header">
        <div>Participant</div>
        <div class="sortable" style="text-align:center;white-space:nowrap" id="sort-findings" onclick="toggleSort('findings')">Findings</div>
        <div class="sortable active" style="text-align:center;white-space:nowrap" id="sort-completed" onclick="toggleSort('completed')">Completed &#x25BC;</div>
        <div style="text-align:center">Role</div>
      </div>
      <div id="agent-rows"></div>

      <div class="section-header">Contributors</div>
      <div id="contributor-rows"></div>
    </div>
    <div class="no-results" id="no-results">No participants match your search.</div>
  </section>

  <section class="tiers-ref" id="tiers-ref">
    <div class="card">
      <h3>Trust Tiers</h3>
      <div class="tiers-grid">
        <div class="tier-item"><div class="tier-dot" style="background:#8b949e"></div><div><div class="tier-label">Newcomer</div><div class="tier-desc">Comment on issues</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#60a5fa"></div><div><div class="tier-label">Contributor</div><div class="tier-desc">5+ tasks &rarr; create PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#4ade80"></div><div><div class="tier-label">Trusted</div><div class="tier-desc">20+ tasks &rarr; merge PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#c084fc"></div><div><div class="tier-label">Advisor</div><div class="tier-desc">Review agent PRs</div></div></div>
        <div class="tier-item"><div class="tier-dot" style="background:#f87171"></div><div><div class="tier-label">Revoked</div><div class="tier-desc">Access removed</div></div></div>
      </div>
      <p class="note">Trust tiers determine what actions a contributor's agent can perform. Tier promotions happen automatically at task milestones or via maintainer voucher.</p>
    </div>
  </section>

  <section class="invite-section">
    <div class="invite-card">
      <h3>Want to contribute?</h3>
      <p>Connect to the %s hive and let your CLI agent help maintain open-source repos.</p>
      <code>%s</code>
    </div>
  </section>

  <section class="share-section">
    <div class="share-buttons">
      <a class="share-btn share-btn-linkedin" href="https://www.linkedin.com/sharing/share-offsite/?url=%s" target="_blank" rel="noopener">
        <svg width="16" height="16" fill="currentColor" viewBox="0 0 24 24"><path d="M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433c-1.144 0-2.063-.926-2.063-2.065 0-1.138.92-2.063 2.063-2.063 1.14 0 2.064.925 2.064 2.063 0 1.139-.925 2.065-2.064 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z"/></svg>
        Share on LinkedIn
      </a>
      <a class="share-btn share-btn-x" href="https://twitter.com/intent/tweet?text=Check+out+the+leaderboard+for+%s+on+Hive!&url=%s" target="_blank" rel="noopener">
        <svg width="16" height="16" fill="currentColor" viewBox="0 0 24 24"><path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg>
        Share on X
      </a>
    </div>
  </section>
</div>

<script>
var AGENTS = %s;
var CONTRIBUTORS = %s;
var sortField = 'completed';
var sortDir = 'desc';
var searchQuery = '';

var GOLD = '\u{1F947}';
var SILVER = '\u{1F948}';
var BRONZE = '\u{1F949}';

var SUCCESS_THRESHOLD_HIGH = 80;
var SUCCESS_THRESHOLD_MID = 50;

function rankHTML(rank) {
  if (rank === 1) return '<span class="medal" title="1st place">' + GOLD + '</span>';
  if (rank === 2) return '<span class="medal" title="2nd place">' + SILVER + '</span>';
  if (rank === 3) return '<span class="medal" title="3rd place">' + BRONZE + '</span>';
  return '<span class="rank-num">#' + rank + '</span>';
}

function formatDate(iso) {
  if (!iso) return '';
  var d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleDateString('en-US', {year:'numeric',month:'short',day:'numeric'});
}

function ghIcon() {
  return '<svg fill="currentColor" viewBox="0 0 24 24"><path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/></svg>';
}

function buildRow(e, isAgent) {
  var total = e.completed + e.failed;
  var successPct = total > 0 ? Math.round((e.completed / total) * 100) : 0;
  var barColor = successPct >= SUCCESS_THRESHOLD_HIGH ? '#4ade80' : successPct >= SUCCESS_THRESHOLD_MID ? '#facc15' : '#f87171';

  var namePrefix = isAgent && e.emoji ? '<span class="agent-emoji">' + e.emoji + '</span>' : '';
  var rowClass = isAgent ? 'row agent-row' : 'row';
  var tierLabel = isAgent ? 'agent' : e.tier;

  var hoverCard = '<div class="hover-card">'
    + '<div class="hc-header">'
    +   '<img src="' + e.avatar + '" alt="' + e.login + '" width="40" height="40">'
    +   '<div><div class="hc-name">' + namePrefix + e.login + '</div>'
    +   '<div class="hc-meta">Rank #' + e._rank + ' &middot; <span class="hc-pts">' + e.completed.toLocaleString() + ' tasks</span></div></div>'
    + '</div>'
    + '<div class="hc-section">'
    +   '<div class="hc-label">Performance</div>'
    +   '<div class="hc-stats">'
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:#4ade80">' + e.completed + '</div><div class="hc-stat-label">Completed</div></div>'
    +     (isAgent && e.findings ? '<div class="hc-stat"><div class="hc-stat-num" style="color:#60a5fa">' + e.findings + '</div><div class="hc-stat-label">Findings</div></div>' : '')
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:#f87171">' + e.failed + '</div><div class="hc-stat-label">' + (isAgent ? 'Restarts' : 'Failed') + '</div></div>'
    +     '<div class="hc-stat"><div class="hc-stat-num" style="color:' + barColor + '">' + successPct + '%%</div><div class="hc-stat-label">Success</div></div>'
    +   '</div>'
    +   '<div class="hc-bar-wrap"><div class="hc-bar" style="width:' + successPct + '%%;background:' + barColor + '"></div></div>'
    + '</div>'
    + '<div class="hc-section">'
    +   '<div class="hc-label">Details</div>'
    +   '<div style="display:flex;justify-content:space-between;align-items:center">'
    +     '<span class="tier-badge" style="background:' + e.tierBg + ';color:' + e.tierText + ';border-color:' + e.tierBorder + '">' + tierLabel + '</span>'
    +     (e.registered ? '<span style="font-size:.75rem;color:#6b7280">Joined ' + formatDate(e.registered) + '</span>' : '')
    +   '</div>'
    + '</div>'
    + '</div>';

  return '<div class="' + rowClass + '">'
    + '<div class="contributor">'
    +   (isAgent ? '' : '<img src="' + e.avatar + '" alt="' + e.login + '" width="32" height="32" loading="lazy">')
    +   '<div class="hover-card-anchor">'
    +     namePrefix
    +     (isAgent ? '<span class="name">' + e.login + '</span>' : '<a class="name" href="https://github.com/' + e.login + '" target="_blank" rel="noopener">' + e.login + '</a><a class="gh-icon" href="https://github.com/' + e.login + '" target="_blank" rel="noopener" title="View on GitHub">' + ghIcon() + '</a>')
    +     hoverCard
    +   '</div>'
    + '</div>'
    + '<div class="stats-cell"><span style="color:#60a5fa;font-size:.875rem;font-weight:600">' + (e.findings > 0 ? e.findings.toLocaleString() : '0') + '</span></div>'
    + '<div class="stats-cell"><div class="completed">' + e.completed.toLocaleString() + '</div></div>'
    + '<div class="stats-cell"><span class="tier-badge" style="background:' + e.tierBg + ';color:' + e.tierText + ';border-color:' + e.tierBorder + '">' + tierLabel + '</span></div>'
    + '</div>';
}

function renderRows() {
  var q = searchQuery.toLowerCase();
  var dir = sortDir === 'desc' ? 1 : -1;
  var sortFn = sortField === 'failed'
    ? function(a, b) { return dir * (b.failed - a.failed); }
    : sortField === 'findings'
    ? function(a, b) { return dir * ((b.findings || 0) - (a.findings || 0)); }
    : function(a, b) { return dir * (b.completed - a.completed); };

  var filteredAgents = AGENTS.slice();
  var filteredContributors = CONTRIBUTORS.slice();

  if (q) {
    filteredAgents = filteredAgents.filter(function(e) { return e.login.toLowerCase().indexOf(q) >= 0; });
    filteredContributors = filteredContributors.filter(function(e) { return e.login.toLowerCase().indexOf(q) >= 0; });
  }

  filteredAgents.sort(sortFn);
  filteredContributors.sort(sortFn);

  var rank = 0;
  var i;
  for (i = 0; i < filteredAgents.length; i++) { rank++; filteredAgents[i]._rank = rank; }
  for (i = 0; i < filteredContributors.length; i++) { rank++; filteredContributors[i]._rank = rank; }

  var agentContainer = document.getElementById('agent-rows');
  var contributorContainer = document.getElementById('contributor-rows');
  var noResults = document.getElementById('no-results');
  var tiersRef = document.getElementById('tiers-ref');
  var totalCount = filteredAgents.length + filteredContributors.length;

  if (totalCount === 0 && (AGENTS.length + CONTRIBUTORS.length) > 0) {
    agentContainer.innerHTML = '';
    contributorContainer.innerHTML = '';
    noResults.style.display = 'block';
    tiersRef.style.display = 'none';
    return;
  }
  noResults.style.display = 'none';
  tiersRef.style.display = totalCount > 0 ? 'block' : 'none';

  if (totalCount === 0) {
    agentContainer.innerHTML = '<div class="empty-state"><div class="icon">\u{1F3C6}</div><p>No participants yet. <a href="/contribute">Be the first!</a></p></div>';
    contributorContainer.innerHTML = '';
    return;
  }

  var agentHTML = '';
  for (i = 0; i < filteredAgents.length; i++) {
    agentHTML += buildRow(filteredAgents[i], true);
  }
  agentContainer.innerHTML = agentHTML;

  var contribHTML = '';
  for (i = 0; i < filteredContributors.length; i++) {
    contribHTML += buildRow(filteredContributors[i], false);
  }
  contributorContainer.innerHTML = contribHTML;

  var sc = document.getElementById('sort-completed');
  var sfi = document.getElementById('sort-findings');
  sc.classList.toggle('active', sortField === 'completed');
  if (sfi) sfi.classList.toggle('active', sortField === 'findings');
  sc.innerHTML = 'Completed ' + (sortField === 'completed' ? (sortDir === 'desc' ? '▼' : '▲') : '');
  if (sfi) sfi.innerHTML = 'Findings ' + (sortField === 'findings' ? (sortDir === 'desc' ? '▼' : '▲') : '');
}

function toggleSort(field) {
  if (sortField === field) {
    sortDir = sortDir === 'desc' ? 'asc' : 'desc';
  } else {
    sortField = field;
    sortDir = 'desc';
  }
  renderRows();
}

document.getElementById('search').addEventListener('input', function(e) {
  searchQuery = e.target.value;
  renderRows();
});

renderRows();
</script>
</body></html>`

// ── Helpers ────────────────────────────────────────────────────────────────

const maxUsernameLength = 39 // GitHub max username length

var reservedUsernames = map[string]bool{
	"null": true, "undefined": true, "true": true, "false": true,
	"admin": true, "root": true, "system": true, "hive": true,
	"api": true, "contribute": true, "leaderboard": true,
}

func isValidUsername(s string) bool {
	if len(s) == 0 || len(s) > maxUsernameLength {
		return false
	}
	if reservedUsernames[strings.ToLower(s)] {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// privateURLDNSTimeout bounds DNS resolution inside the SSRF guard so a
// slow or malicious DNS server cannot block the handler indefinitely.
const privateURLDNSTimeout = 5 * time.Second

type hostResolver func(ctx context.Context, host string) ([]string, error)

var privateURLResolver hostResolver = defaultHostResolver

func defaultHostResolver(ctx context.Context, host string) ([]string, error) {
	resolveCtx, cancel := context.WithTimeout(ctx, privateURLDNSTimeout)
	defer cancel()
	return (&net.Resolver{}).LookupHost(resolveCtx, host)
}

func isPrivateURL(ctx context.Context, rawURL string) bool {
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.TrimPrefix(rawURL, scheme)
			break
		}
	}
	host := rawURL
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	blocked := []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.",
		"172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "[::ffff:", "0.0.0.0", "0."}
	for _, p := range blocked {
		if strings.HasPrefix(host, p) {
			return true
		}
	}

	addrs, err := privateURLResolver(ctx, host)
	if err != nil {
		// If DNS fails, treat as private (fail-closed) to prevent bypass.
		return true
	}
	for _, addr := range addrs {
		for _, p := range blocked {
			if strings.HasPrefix(addr, p) {
				return true
			}
		}
	}

	return false
}

// validateGitHubToken checks a GitHub personal access token against the GitHub API
// and returns the authenticated username, or empty string on failure.
var (
	ghTokenCacheMu sync.RWMutex
	ghTokenCache   = map[string]ghTokenCacheEntry{}
)

const ghTokenCacheTTL = 5 * time.Minute

type ghTokenCacheEntry struct {
	username  string
	expiresAt time.Time
}

// validateGitHubToken checks a token against the GitHub API user endpoint.
// apiURL overrides the API base for GHE; pass empty for default github.com.
func validateGitHubToken(token, apiURL string) string {
	if token == "" {
		return ""
	}

	ghTokenCacheMu.RLock()
	if entry, ok := ghTokenCache[token]; ok && time.Now().Before(entry.expiresAt) {
		ghTokenCacheMu.RUnlock()
		return entry.username
	}
	ghTokenCacheMu.RUnlock()

	userEndpoint := "https://api.github.com/user"
	if apiURL != "" && apiURL != "https://api.github.com" {
		userEndpoint = apiURL + "/user"
	}

	const tokenValidateTimeout = 10 * time.Second
	client := &http.Client{Timeout: tokenValidateTimeout}
	req, err := http.NewRequest("GET", userEndpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		return ""
	}

	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: user.Login, expiresAt: time.Now().Add(ghTokenCacheTTL)}
	ghTokenCacheMu.Unlock()

	return user.Login
}

// handleAPIv1 wraps contribute API endpoints with GitHub token auth.
// Accepts Authorization: Bearer <gh-personal-access-token>.
func (s *Server) handleAPIv1(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	if strings.HasPrefix(token, "Bearer ") {
		token = token[7:]
	} else if strings.HasPrefix(token, "token ") {
		token = token[6:]
	} else {
		token = r.URL.Query().Get("token")
	}

	username := validateGitHubToken(token, s.deps.Config.GitHub.ResolvedAPIURL())
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid or missing GitHub token. Use: Authorization: Bearer <gh-token>"}`))
		return
	}

	subpath := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch subpath {
	case "/status":
		s.handleContributeStatus(w, r)
	case "/activity":
		s.handleContributeActivity(w, r)
	case "/contributors":
		s.handleContributorsList(w, r)
	case "/knowledge":
		s.handleKnowledgeExport(w, r)
	case "/me":
		profiles := listContributorProfiles()
		for _, p := range profiles {
			if strings.EqualFold(p.GitHubUsername, username) {
				p.TokenPlain = ""
				p.RegistrationToken = ""
				var liveStates map[string]ContributorLiveState
				if s.contributeHub != nil {
					liveStates = s.contributeHub.LiveStates()
				}
				if ls, ok := liveStates[p.ContributorID]; ok {
					p.Active = ls.Active
					p.CurrentTask = ls.CurrentTask
					p.ActiveTasks = ls.Tasks
					p.Sessions = ls.Sessions
				}
				jsonResponse(w, p)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Not registered as a contributor. Run: just contribute-setup"}`))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Unknown endpoint","available":["/api/v1/status","/api/v1/activity","/api/v1/contributors","/api/v1/knowledge","/api/v1/me"]}`))
	}
}

func (s *Server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	host = strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == ':' || c == '-' {
			return c
		}
		return -1
	}, host)
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	baseURL := scheme + "://" + host
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Hive API</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;padding:40px;max-width:900px;margin:0 auto}
h1{margin-bottom:8px;font-size:1.8rem}
.subtitle{color:#8b949e;margin-bottom:32px}
h2{margin-top:32px;margin-bottom:12px;color:#58a6ff;font-size:1.2rem}
.endpoint{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;margin-bottom:12px}
.method{color:#3fb950;font-weight:bold;margin-right:8px}
.path{color:#58a6ff;font-family:monospace}
.desc{color:#8b949e;margin-top:4px;font-size:0.9rem}
pre{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:12px;margin-top:12px;overflow-x:auto;font-size:0.85rem;color:#e6edf3}
code{font-family:'SF Mono',monospace;font-size:0.85rem}
.token-box{background:#161b22;border:1px solid #f0883e;border-radius:8px;padding:16px;margin:16px 0}
.token-box h3{color:#f0883e;margin-bottom:8px}
a{color:#58a6ff}
</style></head><body>
<h1>🐝 Hive API</h1>
<p class="subtitle">Authenticated access to the contributor API</p>

<div class="token-box">
<h3>Authentication</h3>
<p>Use your GitHub personal access token (from <code>gh auth token</code>):</p>
<pre>curl -H "Authorization: Bearer $(gh auth token)" %s/api/v1/status</pre>
</div>

<h2>Endpoints</h2>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/status</span>
<div class="desc">Hub status — online, active contributors, actionable items</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/status</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/me</span>
<div class="desc">Your contributor profile — tasks completed, active sessions, current task</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/me</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/contributors</span>
<div class="desc">All registered contributors with live state</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/contributors</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/activity</span>
<div class="desc">Live activity feed — joined, left, picked up, completed events</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/activity</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/v1/knowledge</span>
<div class="desc">Knowledge base export as markdown (used by agent.md)</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/v1/knowledge</pre>
</div>

<h2>Knowledge Sources</h2>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/stats</span>
<div class="desc">Knowledge base stats — layers, fact counts, engine, health</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/stats</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/search?q=&lt;query&gt;&amp;limit=10</span>
<div class="desc">Search all knowledge facts by keyword</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/search?q=autoscaling&amp;limit=10</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">List connected git sources</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">Add a git source — clone a repo and index its markdown as knowledge facts</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://github.com/org/repo","name":"my-docs","subpath":"docs","branch":"main","layer":"project"}' \
  %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">DELETE</span><span class="path">/api/knowledge/git-sources</span>
<div class="desc">Remove a git source</div>
<pre>curl -X DELETE -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://github.com/org/repo","subpath":"docs"}' \
  %s/api/knowledge/git-sources</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/documents</span>
<div class="desc">List imported documents</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/documents</span>
<div class="desc">Import a document from URL — supports PDF, HTML, DOCX, plain text. Content is parsed into chunks and stored as knowledge facts.</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://arxiv.org/pdf/2309.06180","name":"vllm-paper","layer":"community"}' \
  %s/api/knowledge/documents</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/documents/{slug}</span>
<div class="desc">Get document metadata — title, source URL, fact count, fact slugs</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper</pre>
</div>

<div class="endpoint">
<span class="method">DELETE</span><span class="path">/api/knowledge/documents/{slug}</span>
<div class="desc">Delete a document and all its extracted facts</div>
<pre>curl -X DELETE -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/documents/{slug}/reimport</span>
<div class="desc">Re-fetch a document and re-extract facts (replaces old facts)</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" %s/api/knowledge/documents/vllm-paper/reimport</pre>
</div>

<div class="endpoint">
<span class="method">GET</span><span class="path">/api/knowledge/subscriptions</span>
<div class="desc">List wiki subscriptions (remote llm-wiki endpoints)</div>
<pre>curl -H "Authorization: Bearer $TOKEN" %s/api/knowledge/subscriptions</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/subscriptions</span>
<div class="desc">Add a wiki subscription</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"url":"https://wiki.example.com/mcp","name":"team-wiki","layer":"org"}' \
  %s/api/knowledge/subscriptions</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/knowledge/import</span>
<div class="desc">Import facts from raw markdown or JSON content</div>
<pre>curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"# Guard .join()\n\nAlways use (arr || []).join()","layer":"project","format":"markdown"}' \
  %s/api/knowledge/import</pre>
</div>

<h2>Token Management</h2>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/contribute/reissue-token</span>
<div class="desc">Reissue your registration token using GitHub auth — invalidates the old token</div>
<pre>curl -X POST -H "Authorization: Bearer $(gh auth token)" %s/api/contribute/reissue-token</pre>
</div>

<div class="endpoint">
<span class="method">POST</span><span class="path">/api/contribute/register</span>
<div class="desc">Register as a contributor (returns your token once). To rotate a lost token, use the reissue-token endpoint below with your GitHub token — registration cannot reissue.</div>
<pre>curl -X POST -d '{"github_username":"you"}' %s/api/contribute/register</pre>
</div>

</body></html>`, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL, baseURL)
}
