package dashboard

import (
	"strings"
	"testing"
)

func TestOpsContributionCardUsesProfileSignInPrompt(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`function ccRenderMineSignIn(status,username){`,
		`renderMeSignIn(body,username);`,
		`if(r.status===401||r.status===403)return {__ccMineStatus:r.status};`,
		`function ccRenderMineIdentityState(status){`,
		`if(d&&d.__ccMineStatus){ccRenderMineIdentityState(d.__ccMineStatus);return;}`,
		`ccRenderMineError(e);`,
		`the Profile tab uses instead of silently hiding the card`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Operations contribution card is missing signed-out/no-profile handling %q", want)
		}
	}
}

func TestOpsMyWorkIsScopedToViewer(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`function workBelongsToViewer(w){`,
		`uname.toLowerCase()===ccMeUsername.toLowerCase()`,
		`ccCompletedWorkItems(30,ccMeUsername)`,
		`if(want&&String(e.username||'').toLowerCase()!==want)continue;`,
		`shown=shown.filter(workBelongsToViewer);`,
		`if(!ccMeUsername){renderWorkSignInEmpty(el);return;}`,
		`<div class="me-signin"><b>Sign in</b> to see your work.</div>`,
		`function ccLoadViewerIdentity(){`,
		`try{ccLoadViewerIdentity();}catch(e){console.error('viewer identity load failed',e);}`,
		`if(typeof lastWork!=='undefined'){try{renderWork(lastWork);}catch(e){}}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Operations My work panel is missing viewer scoping marker %q", want)
		}
	}
}
