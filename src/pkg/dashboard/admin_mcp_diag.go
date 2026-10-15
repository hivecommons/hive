package dashboard

import (
	"net/http"
	"net/http/httptest"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// adminMCPOwnerVerified reports whether this hive's own authentication marked
// r as a verified owner (dashboard token, internal token or a verified owner
// session). authenticate deletes any inbound marker before deciding, so a
// client cannot set it, and a role claimed in X-Hive-Role alone never counts.
func adminMCPOwnerVerified(r *http.Request) bool {
	return isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true"
}

// readDiag serves one diagnostic read (hivecommons/hive#11943) through the
// /api/diag routes as the caller. The results bound themselves, so CapResult
// and the shared limit do not apply. The configuration read is refused with
// the typed adminmcp.ErrOwnerUnverified before any request unless the outer
// request was verified as owner by this hive; only then is the verified-owner
// marker copied onto the inner request.
func (p dashboardAdminMCPProvider) readDiag(tool string, args map[string]any) (any, error) {
	path, err := adminmcp.DiagReadPath(tool, args)
	if err != nil {
		return nil, err
	}
	owner := adminmcp.DiagOwnerTool(tool)
	if owner && !p.ownerVerified {
		return nil, adminmcp.ErrOwnerUnverified
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if p.user != "" {
		req.Header.Set("X-Hive-User", p.user)
	}
	if p.role != "" {
		req.Header.Set("X-Hive-Role", p.role)
	}
	if owner {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	p.server.mux.ServeHTTP(rec, req)
	if rec.Code < http.StatusOK || rec.Code >= http.StatusMultipleChoices {
		return nil, adminmcp.DiagHTTPError(tool, rec.Code)
	}
	return decodeAdminMCPJSON(rec.Body.Bytes())
}
