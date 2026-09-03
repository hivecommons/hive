package dashboard

// csp_base_script_src.go — the dashboard-side half of the CSP script-src
// machinery. The pure hash/extraction helpers live in pkg/dashboard/webstatic
// (extracted under #5565); this file keeps only the startup composition that
// depends on dashboard-owned bytes: the embedded SPA (staticFS) and the
// device-flow login page const.

import (
	"io/fs"
	"sync"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

var (
	baseScriptSrcElemOnce    sync.Once
	baseScriptSrcElemSources string
)

// baseScriptSrcElem returns the startup-computed script-src-elem source list
// covering the two documents whose bytes are fixed for the life of the
// process: the embedded SPA (static/index.html, served verbatim by both
// webstatic.IndexDocument and the plain file server) and the device-flow login
// page (a const, served to any unauthenticated browser path). Computed once,
// like the #3863 gzip/ETag precomputation it must stay compatible with.
func baseScriptSrcElem() string {
	baseScriptSrcElemOnce.Do(func() {
		var docs []byte
		if raw, err := fs.ReadFile(staticFS, "static/index.html"); err == nil {
			docs = append(docs, raw...)
		}
		docs = append(docs, []byte(loginPage)...)
		baseScriptSrcElemSources = webstatic.ScriptSrcElemSources(docs)
	})
	return baseScriptSrcElemSources
}
