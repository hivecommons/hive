package adminmcp

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrRequestPath is returned for a request path the admin MCP will not send.
var ErrRequestPath = errors.New("admin MCP request path refused")

// CheckRequestPath refuses a request path that has an empty, "." or ".."
// element.
//
// Every path the admin MCP sends is a fixed route with names from the caller
// in it: an agent, a run, a repository, a contributor, a plan. Those names are
// escaped with url.PathEscape, which leaves "." and ".." as they are. The
// hive's router does not serve a path that holds such an element: it answers
// with a redirect to the cleaned path, and the cleaned path is another route.
// "/api/kick/../status" is "/api/status", and "/api/model/../<name>" is
// "/api/<name>" for any name. The stdio client follows that redirect with the
// same method, body and credentials, so a name of ".." turned a listed
// operation into a request for an endpoint the admin MCP does not offer,
// including ones it excludes. Over HTTP the redirect is not followed and the
// caller saw only "HTTP 307".
//
// No agent, run, repository, contributor or plan is named "", "." or "..", so
// such a path is refused before anything is sent. Both transports call this
// for every read and every write.
func CheckRequestPath(path string) error {
	rawPath, _, _ := strings.Cut(path, "?")
	if !strings.HasPrefix(rawPath, "/") {
		return fmt.Errorf("%w: %q is not an absolute path", ErrRequestPath, rawPath)
	}
	for _, element := range strings.Split(rawPath[1:], "/") {
		decoded := element
		if unescaped, err := url.PathUnescape(element); err == nil {
			decoded = unescaped
		}
		if decoded == "" || decoded == "." || decoded == ".." {
			return fmt.Errorf("%w: %q has an empty, \".\" or \"..\" element; an agent, run, repository, contributor or plan name cannot be empty, \".\" or \"..\"", ErrRequestPath, rawPath)
		}
	}
	return nil
}
