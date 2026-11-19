package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux"
)

const sessionUploadPrefixBytes = 4

// sanitizeUploadName reduces a client-supplied file name to a safe base name:
// no path, no leading dots, only [A-Za-z0-9._-], bounded length.
func sanitizeUploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimLeft(b.String(), ".")
	if len(out) > sessionFileNameMax {
		out = out[len(out)-sessionFileNameMax:]
	}
	if out == "" {
		return "upload"
	}
	return out
}

// handleSessionUpload stores one multipart file ("file") under the session's
// uploads directory and returns its absolute path; the UI then types that path
// into the session through /input. The file is written 0600, owned by the
// session's uid, and never executed or interpreted by the hive.
func (s *Server) handleSessionUpload(w http.ResponseWriter, r *http.Request) {
	rt, st, ok := s.sessionsGate(w)
	if !ok {
		return
	}
	actor := s.sessionActorOf(r)
	if !actor.write {
		jsonError(w, "uploading requires owner or read-write role", http.StatusForbidden)
		return
	}
	rec, ok := s.sessionForUse(w, st, r.PathValue("id"), actor)
	if !ok {
		return
	}
	lim := s.sessionLimits()
	r.Body = http.MaxBytesReader(w, r.Body, lim.MaxUploadByte+sessionsMultipartOverheadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, "expected a multipart upload", http.StatusBadRequest)
		return
	}
	var part *multipart.Part
	for {
		p, err := mr.NextPart()
		if err != nil {
			if isUploadTooLarge(err) {
				jsonError(w, "upload is too large", http.StatusRequestEntityTooLarge)
				return
			}
			jsonError(w, "no file field in the upload", http.StatusBadRequest)
			return
		}
		if p.FormName() == sessionUploadFormName && p.FileName() != "" {
			part = p
			break
		}
	}
	prefix := make([]byte, sessionUploadPrefixBytes)
	if _, err := rand.Read(prefix); err != nil {
		jsonError(w, "could not name the upload", http.StatusInternalServerError)
		return
	}
	fileName := hex.EncodeToString(prefix) + "-" + sanitizeUploadName(part.FileName())
	// The runtime writes the file AS THE SESSION'S UID into the session's own
	// sandbox (exclusive create, no symlink following), so the hive never
	// opens a path inside a directory the session controls.
	dest, n, err := rt.StoreUpload(rec.ID, fileName, part, lim.MaxUploadByte, lim.MaxUploads)
	switch {
	case errors.Is(err, crux.ErrUserSessionUploadLimit):
		jsonError(w, "this session already has the maximum of "+strconv.Itoa(lim.MaxUploads)+" uploaded files", http.StatusTooManyRequests)
		return
	case errors.Is(err, crux.ErrUserSessionUploadTooLarge), err != nil && isUploadTooLarge(err):
		jsonError(w, fmt.Sprintf("upload exceeds %d bytes", lim.MaxUploadByte), http.StatusRequestEntityTooLarge)
		return
	case err != nil:
		if s.logger != nil {
			s.logger.Warn("sessions: storing upload failed", "id", rec.ID, "error", err)
		}
		jsonError(w, "could not store the upload", http.StatusInternalServerError)
		return
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		abs = dest
	}
	s.touchSession(st, rec.ID)
	// Who uploaded and how much, into which session; the name is not recorded.
	s.auditSession(actor.user, auditSessionUpload, auditDetail("session", rec.ID, "bytes", strconv.FormatInt(n, 10)))
	jsonResponse(w, map[string]any{"ok": true, "path": abs})
}

func isUploadTooLarge(err error) bool {
	var tooBig *http.MaxBytesError
	return errors.As(err, &tooBig)
}
