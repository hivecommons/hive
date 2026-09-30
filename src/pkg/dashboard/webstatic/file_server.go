package webstatic

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// indexPage is the file net/http's file server serves for a directory URL.
const indexPage = "index.html"

// FileServer serves immutable embedded files with content validators. Their URLs
// are not versioned, so browsers must revalidate them after a binary upgrade.
// Every response carries Cache-Control: no-cache; served files also carry the
// shared ETagFor validator, and a matching If-None-Match is answered with a
// 304 via WriteCacheHeaders (the same helpers the hub and spoke HTML handlers
// use, hivecommons/hive#9674).
// Do not use it for mutable files: validators are cached for the handler's life.
func FileServer(files fs.FS) http.Handler {
	server := http.FileServerFS(files)
	var etags sync.Map // served file name -> ETag string
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := servedFileName(files, r.URL.Path)
		if !ok {
			// Missing files, directory listings and net/http's canonicalizing
			// redirects get no validator, only the revalidation directive.
			w.Header().Set("Cache-Control", "no-cache")
			server.ServeHTTP(w, r)
			return
		}
		etag, ok := cachedETag(&etags, files, name)
		if !ok {
			w.Header().Set("Cache-Control", "no-cache")
			server.ServeHTTP(w, r)
			return
		}
		if WriteCacheHeaders(w, r, etag) {
			return
		}
		// net/http still handles HEAD, ranges (If-Range sees the ETag set
		// above) and content types, even though embed.FS has no mtime.
		server.ServeHTTP(w, r)
	})
}

// cachedETag returns the memoized ETagFor validator for name, reading and
// hashing the file once per handler.
func cachedETag(etags *sync.Map, files fs.FS, name string) (string, bool) {
	if v, found := etags.Load(name); found {
		etag, ok := v.(string)
		return etag, ok
	}
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return "", false
	}
	v, _ := etags.LoadOrStore(name, ETagFor(data))
	etag, ok := v.(string)
	return etag, ok
}

// servedFileName maps a request path to the file net/http's file server
// would write for it. It reports false when the server would instead
// redirect (".../index.html" to ".../", a directory without its trailing
// slash, or a file with one) or the path does not exist, so a 304 is never
// sent in place of a redirect or an error.
func servedFileName(files fs.FS, urlPath string) (string, bool) {
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	if strings.HasSuffix(urlPath, "/"+indexPage) {
		return "", false
	}
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	if name == "" {
		name = "."
	}
	info, err := fs.Stat(files, name)
	if err != nil {
		return "", false
	}
	trailingSlash := strings.HasSuffix(urlPath, "/")
	if info.IsDir() {
		if !trailingSlash {
			return "", false
		}
		return path.Join(name, indexPage), true
	}
	if trailingSlash {
		return "", false
	}
	return name, true
}
