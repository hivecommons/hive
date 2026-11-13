package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
// into the session through /input. The file is written 0600 and never
// executed or interpreted by the hive.
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
	rec, found := st.get(r.PathValue("id"))
	if !found {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}
	if rec.Status != sessionStatusRunning {
		jsonError(w, "session is not running", http.StatusConflict)
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
	dir := filepath.Join(rt.WorkDir(rec.ID), sessionUploadsDirName)
	if err := os.MkdirAll(dir, sessionUploadDirMode); err != nil {
		jsonError(w, "could not prepare the upload directory", http.StatusInternalServerError)
		return
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) >= lim.MaxUploads {
		jsonError(w, "this session already has the maximum of "+strconv.Itoa(lim.MaxUploads)+" uploaded files", http.StatusTooManyRequests)
		return
	}
	prefix := make([]byte, sessionUploadPrefixBytes)
	if _, err := rand.Read(prefix); err != nil {
		jsonError(w, "could not name the upload", http.StatusInternalServerError)
		return
	}
	dest := filepath.Join(dir, hex.EncodeToString(prefix)+"-"+sanitizeUploadName(part.FileName()))
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sessionUploadFileMode)
	if err != nil {
		jsonError(w, "could not store the upload", http.StatusInternalServerError)
		return
	}
	n, copyErr := io.Copy(f, io.LimitReader(part, lim.MaxUploadByte+1))
	closeErr := f.Close()
	switch {
	case copyErr != nil && isUploadTooLarge(copyErr), copyErr == nil && n > lim.MaxUploadByte:
		_ = os.Remove(dest)
		jsonError(w, fmt.Sprintf("upload exceeds %d bytes", lim.MaxUploadByte), http.StatusRequestEntityTooLarge)
		return
	case copyErr != nil, closeErr != nil:
		_ = os.Remove(dest)
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
