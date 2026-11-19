package prosefilter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Outbound send-site inventory (hivecommons/hive#19103).
//
// Every non-test function under src/pkg, src/cmd and src/crux that sends an
// HTTP request with a body (http.NewRequest / NewRequestWithContext with a
// method other than GET, HEAD, DELETE or OPTIONS, http.Post, http.PostForm,
// or a client's Post(url, "content/type", body)) must be listed in
// outboundSites with a class. TestOutboundSitesInventory fails when a new
// sender is added without an entry, when an entry no longer names a sender,
// and when a site classed as filtered stops calling the shared filter. That
// keeps the em-dash rule from regressing as new channels are added: a new
// Slack, Discord, meeting, email or forge sender cannot land without someone
// deciding, in review, how its text is filtered.
//
// Hive's own go-github writes do not appear here (they are not raw HTTP
// sends): their text is either agent text passed through relayBody or a
// hive-rendered template guarded by TestNoNewDashesInEmittedText. The
// egress proxy's relay of agent writes and the push broker's commit message
// rule are pinned by requiredCalls below.

type outboundClass string

const (
	// classFiltered: the function itself calls prosefilter before sending.
	classFiltered outboundClass = "filtered"
	// classVia: the text was filtered by the named function before it got
	// here (via must itself call prosefilter).
	classVia outboundClass = "via"
	// classTemplate: hive-rendered text whose source package (source) is
	// under the emit ratchet, so no dash can be written into it. Used where a
	// send compares the rendered text with what is already on the forge, and
	// rewriting after the compare would edit on every pass.
	classTemplate outboundClass = "template"
	// classMachine: the body carries no text a person reads on a third-party
	// surface (tokens, ids, state, labels, heartbeats, model prompts, calls
	// to hive's own services).
	classMachine outboundClass = "machine"
)

type outboundSite struct {
	class  outboundClass
	via    string
	source string
	reason string
}

func filtered() outboundSite { return outboundSite{class: classFiltered} }
func via(fn, reason string) outboundSite {
	return outboundSite{class: classVia, via: fn, reason: reason}
}
func template(source, reason string) outboundSite {
	return outboundSite{class: classTemplate, source: source, reason: reason}
}
func machine(reason string) outboundSite { return outboundSite{class: classMachine, reason: reason} }

const (
	reasonSpokeToHub   = "hive-to-hub or hub-to-spoke control traffic; hub-side delivery is inventoried separately"
	reasonModelPrompt  = "model or agent-runtime request; nothing here is published"
	reasonAuth         = "OAuth, token or credential exchange"
	reasonInternalAPI  = "call to hive's own API or service"
	reasonPlatform     = "platform API (Kubernetes, object storage, secrets store)"
	reasonStateOnly    = "state, labels or ids only; no text"
	reasonTranscribe   = "audio upload for transcription"
	reasonTestFixture  = "test fixture server"
	reasonFleetReport  = "seen-log and affected-hives bookkeeping, compared with the forge copy before an edit"
	reasonForgeWrite   = "forge comment and issue writes; text is agent text already filtered or hive templates"
	reasonFixedMessage = "fixed ASCII commit message"
)

var outboundSites = map[string]outboundSite{
	// Filtered at the send boundary.
	"discord.(*Bot).do":                        filtered(),
	"discord.(*Bot).setChannelTopic":           filtered(),
	"github.(*Client).createCommitOnBranch":    filtered(),
	"hub.(*GitHubActivityFeed).postDiscord":    filtered(),
	"hub.(*HubServer).pushAuthAuditAlert":      filtered(),
	"hub.postDiscordRequestWebhook":            filtered(),
	"hub.postHubGitHubIssue":                   filtered(),
	"hub.sendSlackDM":                          filtered(),
	"inbox.(*DibsDestination).Deliver":         filtered(),
	"linearagent.doGraphQL":                    filtered(),
	"meetings.(*Client).post":                  filtered(),
	"worksource.(*jiraSource).doJSON":          filtered(),
	"worksource.linearGraphQL":                 filtered(),
	"notify.(*Notifier).sendNtfy":              via("notify.(*Notifier).Send", "ntfy alert"),
	"notify.(*Notifier).sendSlack":             via("notify.(*Notifier).Send", "Slack webhook alert"),
	"notify.(*Notifier).sendDiscordWebhook":    via("notify.(*Notifier).Send", "Discord webhook alert"),
	"main.hubSelfReportRelay":                  via("hub.postHubGitHubIssue", "spoke self-report; the hub files it"),
	"dashboard.(*Server).forwardFeedbackToHub": via("hub.postHubGitHubIssue", "operator feedback; the hub files it"),

	// Hive-rendered text under the emit ratchet.
	"hub.postHubGitHubIssueComment":      template("pkg/fleetreport", reasonFleetReport),
	"hub.patchHubGitHubIssueBody":        template("pkg/fleetreport", reasonFleetReport),
	"hub.patchHubGitHubIssueComment":     template("pkg/fleetreport", reasonFleetReport),
	"forge.(*gitLabForge).doWrite":       template("pkg/forge", reasonForgeWrite),
	"forge.(*giteaForge).doWrite":        template("pkg/forge", reasonForgeWrite),
	"forge.(*giteaForge).Merge":          template("pkg/forge", "merge request on a non-GitHub forge"),
	"forge.(*gitLabForge).doAcceptMerge": template("pkg/forge", "merge request on a non-GitHub forge"),
	"feedbackbundle.githubJSON":          template("pkg/feedbackbundle", "feedback bundle commit message"),

	// No human-read text.
	"agent.(*sharedSessionPoolClient).request":         machine(reasonInternalAPI),
	"api.(*Client).request":                            machine(reasonInternalAPI),
	"askai.(*KapaClient).Chat":                         machine(reasonModelPrompt),
	"auth.(*Provider).fetchIDToken":                    machine(reasonAuth),
	"callback.(*Client).do":                            machine(reasonInternalAPI),
	"client.(*Client).doJSON":                          machine(reasonInternalAPI),
	"connector.(*HTTPClient).once":                     machine(reasonInternalAPI),
	"dashboard.(*Server).forwardNPS":                   machine(reasonSpokeToHub),
	"dashboard.(*Server).handleCopilotAuthStart":       machine(reasonAuth),
	"dashboard.(*Server).handleReleaseChannelSwitch":   machine(reasonSpokeToHub),
	"dashboard.(*Server).handleSelfUpgrade":            machine(reasonSpokeToHub),
	"dashboard.(*Server).hubRequest":                   machine(reasonSpokeToHub),
	"dashboard.(*Server).pollCopilotToken":             machine(reasonAuth),
	"dashboard.(*Server).postBenchmarksBatch":          machine(reasonSpokeToHub),
	"dashboard.kubernetesSelfUpgradeRequest":           machine(reasonPlatform),
	"dashboard.meetingsSpeak":                          machine("persona voice sample, played back to the operator in the dashboard only"),
	"dashboard.npsRelayPost":                           machine(reasonSpokeToHub),
	"dashboard.patchKeyIntoHiveSecrets":                machine(reasonPlatform),
	"dashboard.probeLiteLLMInference":                  machine(reasonModelPrompt),
	"discord.(*Bot).dashboardPost":                     machine(reasonInternalAPI),
	"fixture.(*Server).attemptEffect":                  machine(reasonTestFixture),
	"flue.(*Adapter).do":                               machine(reasonModelPrompt),
	"forge.(*gitLabForge).doRebase":                    machine(reasonStateOnly),
	"github.PollDeviceFlowWithScope":                   machine(reasonAuth),
	"github.StartDeviceFlowWithScope":                  machine(reasonAuth),
	"hivectl.(*Client).request":                        machine(reasonInternalAPI),
	"hivectl.Register":                                 machine(reasonInternalAPI),
	"hivectl.Reissue":                                  machine(reasonInternalAPI),
	"hub.(*HubServer).exchangeGitHubLogin":             machine(reasonAuth),
	"hub.(*HubServer).pushGitHubConfigToSpoke":         machine(reasonSpokeToHub),
	"hub.(*HubServer).pushVisibilityToSpoke":           machine(reasonSpokeToHub),
	"hub.(*HubServer).relayWebhookToSpoke":             machine(reasonSpokeToHub),
	"hub.closeHubGitHubIssue":                          machine(reasonStateOnly),
	"hub.commitBenchmarkFile":                          machine(reasonFixedMessage),
	"hub.createOCIExport":                              machine(reasonPlatform),
	"hub.createOCIFileSystem":                          machine(reasonPlatform),
	"hub.npsRelayAck":                                  machine(reasonSpokeToHub),
	"hub.postHubGitHubIssueLabels":                     machine(reasonStateOnly),
	"hub.relayMailOnce":                                machine(reasonSpokeToHub),
	"hub.topRepoRequest":                               machine(reasonStateOnly),
	"hubbackup.(*ObjectStore).do":                      machine(reasonPlatform),
	"intent.(*AlignmentReviewer).reviewUncached":       machine(reasonModelPrompt),
	"ioscan.(*openAIChatClient).completeOnce":          machine(reasonModelPrompt),
	"jev.(*Client).Decide":                             machine(reasonModelPrompt),
	"jev.callEndpoint":                                 machine(reasonModelPrompt),
	"knowledge.(*Client).postJSON":                     machine(reasonInternalAPI),
	"knowledge.(*Promoter).ingestToLayer":              machine(reasonInternalAPI),
	"kubejob.(*Launcher).do":                           machine(reasonPlatform),
	"linearagent.postTokenForm":                        machine(reasonAuth),
	"mailin.(*HubClient).do":                           machine(reasonSpokeToHub),
	"main.(*delegateDeciderRunner).RunDeciderTurn":     machine(reasonModelPrompt),
	"main.(*knowledgeAdviserLLM).complete":             machine(reasonModelPrompt),
	"main.customersHubMatcher.Match":                   machine(reasonSpokeToHub),
	"main.kbPost":                                      machine(reasonInternalAPI),
	"managedagents.(*Adapter).doJSON":                  machine(reasonModelPrompt),
	"meetings.(*Client).CompanionAudio":                machine(reasonTranscribe),
	"meetllm.complete":                                 machine(reasonModelPrompt),
	"meetllm.completeAnthropic":                        machine(reasonModelPrompt),
	"openbao.(*Provider).raw":                          machine(reasonPlatform),
	"openrouter.ExchangeCode":                          machine(reasonAuth),
	"proxy.(*GitHubProxy).handleInferenceRequest":      machine(reasonModelPrompt),
	"proxy.(*GitHubProxy).inferenceTranslatorHandler":  machine(reasonModelPrompt),
	"proxy.(*GitHubProxy).retryInferenceOnAuthFailure": machine(reasonModelPrompt),
	"proxy.forwardToInference":                         machine(reasonModelPrompt),
	"proxy.retryInferenceOnOverload":                   machine(reasonModelPrompt),
	"retro.(*openAIChatClient).Complete":               machine(reasonModelPrompt),
	"reviewpreflight.(*Client).do":                     machine(reasonInternalAPI),
	"rotation.KiroProber.Probe":                        machine(reasonModelPrompt),
	"sandbox.(*routerBridge).do":                       machine(reasonPlatform),
	"spoke.SendUpgradingHeartbeat":                     machine(reasonSpokeToHub),
	"spoke.StartTaskStatusPush":                        machine(reasonSpokeToHub),
	"spoke.k8sAPIPatchOnceWithContentType":             machine(reasonPlatform),
	"spoke.postActivityUpdateAck":                      machine(reasonSpokeToHub),
	"spoke.postHeartbeatToHub":                         machine(reasonSpokeToHub),
	"spoke.postHeartbeatToMirror":                      machine(reasonSpokeToHub),
	"spoke.reportUpgradeOutcome":                       machine(reasonSpokeToHub),
	"trajectory.(*Reviewer).Review":                    machine(reasonModelPrompt),
	"transcribe.(*Deepgram).Transcribe":                machine(reasonTranscribe),
	"transcribe.(*Gemini).do":                          machine(reasonTranscribe),
	"watsonx.(*TokenMinter).mint":                      machine(reasonAuth),
	"worksource.(*githubProjectsSource).graphql":       machine(reasonStateOnly),
}

// requiredCalls pins the boundaries that are not raw HTTP sends: file
// (relative to src/) -> the function in it that must call the named helper.
var requiredCalls = []struct{ file, fn, call string }{
	{"crux/proxy/github_proxy_mitm.go", "proxy.(*GitHubProxy).proxyHTTPHost", "sanitizeAgentWriteProse"},
	{"crux/pushbroker/pushbroker.go", "pushbroker.(*Broker).Run", "sanitizeHeadCommitMessage"},
	{"pkg/github/write_surface.go", "github.(*Client).relayBody", "Sanitize"},
	{"pkg/github/pullrequest.go", "github.(*Client).CreatePR", "Sanitize"},
	{"pkg/github/pullrequest.go", "github.(*Client).MergePRVia", "Sanitize"},
	{"pkg/notify/notify.go", "notify.(*Notifier).Send", "Sanitize"},
}

// safeMethods are the request methods that carry no body to filter.
var safeMethods = map[string]bool{
	"MethodGet": true, "MethodHead": true, "MethodDelete": true, "MethodOptions": true,
	`"GET"`: true, `"HEAD"`: true, `"DELETE"`: true, `"OPTIONS"`: true,
}

// scannedFunc is what the scan records for one function declaration.
type scannedFunc struct {
	pos          string
	sends        bool
	callsFilter  bool
	calledIdents map[string]bool
}

func TestOutboundSitesInventory(t *testing.T) {
	srcRoot := srcRootDir(t)
	funcs := scanFuncs(t, srcRoot)

	var missing []string
	for name, f := range funcs {
		if !f.sends {
			continue
		}
		if _, ok := outboundSites[name]; !ok {
			missing = append(missing, name+" ("+f.pos+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("outbound send site %s is not in outboundSites: route its text through prosefilter (Sanitize, SanitizeTitle or SanitizeJSON) and add it as filtered(), or classify it with a reason", m)
	}

	for name, site := range outboundSites {
		f, ok := funcs[name]
		if !ok || !f.sends {
			t.Errorf("outboundSites entry %s no longer names an outbound send site; remove it", name)
			continue
		}
		switch site.class {
		case classFiltered:
			if !f.callsFilter {
				t.Errorf("%s (%s) is classed filtered but does not call prosefilter", name, f.pos)
			}
		case classVia:
			v, ok := funcs[site.via]
			if !ok || !v.callsFilter {
				t.Errorf("%s says its text is filtered by %s, which does not exist or does not call prosefilter", name, site.via)
			}
		case classTemplate:
			if !slices.Contains(emitDirs, site.source) {
				t.Errorf("%s names template source %s, which is not under the emit ratchet (emitDirs)", name, site.source)
			}
		case classMachine:
		default:
			t.Errorf("%s has unknown class %q", name, site.class)
		}
		if site.class != classFiltered && strings.TrimSpace(site.reason) == "" {
			t.Errorf("%s is classed %s without a reason", name, site.class)
		}
	}

	for _, rc := range requiredCalls {
		f, ok := funcs[rc.fn]
		if !ok {
			t.Errorf("%s: function %s not found", rc.file, rc.fn)
			continue
		}
		if !f.calledIdents[rc.call] {
			t.Errorf("%s (%s) must call %s: it is the outbound dash boundary for its path", rc.fn, f.pos, rc.call)
		}
	}
}

func srcRootDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// scanFuncs parses every non-test Go file under src/pkg, src/cmd and
// src/crux and records, per function, whether it sends a body over HTTP and
// whether it calls the shared filter.
func scanFuncs(t *testing.T, srcRoot string) map[string]*scannedFunc {
	t.Helper()
	funcs := map[string]*scannedFunc{}
	fset := token.NewFileSet()
	for _, dir := range []string{"pkg", "cmd", "crux"} {
		root := filepath.Join(srcRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "vendor", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			f, perr := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			rel, _ := filepath.Rel(srcRoot, path)
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := f.Name.Name + "." + funcName(fd)
				sf := funcs[name]
				if sf == nil {
					sf = &scannedFunc{pos: rel + ":" + strconv.Itoa(fset.Position(fd.Pos()).Line), calledIdents: map[string]bool{}}
					funcs[name] = sf
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fn := call.Fun.(type) {
					case *ast.Ident:
						sf.calledIdents[fn.Name] = true
					case *ast.SelectorExpr:
						sf.calledIdents[fn.Sel.Name] = true
						if x, ok := fn.X.(*ast.Ident); ok && x.Name == "prosefilter" {
							sf.callsFilter = true
						}
						if isBodySend(fn, call) {
							sf.sends = true
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return funcs
}

// isBodySend reports whether call sends an HTTP request that can carry a
// body.
func isBodySend(fn *ast.SelectorExpr, call *ast.CallExpr) bool {
	x, isIdent := fn.X.(*ast.Ident)
	fromHTTP := isIdent && x.Name == "http"
	switch fn.Sel.Name {
	case "NewRequest", "NewRequestWithContext":
		if !fromHTTP {
			return false
		}
		i := 0
		if fn.Sel.Name == "NewRequestWithContext" {
			i = 1
		}
		if len(call.Args) <= i {
			return true
		}
		return !safeMethods[exprString(call.Args[i])]
	case "PostForm":
		return fromHTTP
	case "Post":
		if fromHTTP {
			return true
		}
		if len(call.Args) != 3 {
			return false
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		return ok && lit.Kind == token.STRING
	}
	return false
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && x.Name == "http" {
			return v.Sel.Name
		}
	}
	return ""
}

func funcName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	typ := d.Recv.List[0].Type
	star := false
	if s, ok := typ.(*ast.StarExpr); ok {
		star, typ = true, s.X
	}
	switch g := typ.(type) {
	case *ast.IndexExpr:
		typ = g.X
	case *ast.IndexListExpr:
		typ = g.X
	}
	recv := "?"
	if id, ok := typ.(*ast.Ident); ok {
		recv = id.Name
	}
	if star {
		return "(*" + recv + ")." + d.Name.Name
	}
	return recv + "." + d.Name.Name
}
