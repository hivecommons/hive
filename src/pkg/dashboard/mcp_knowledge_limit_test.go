package dashboard

import (
	"encoding/json"
	"testing"
)

// publicKnowledgeToolCall calls one public knowledge tool and returns the
// HTTP status, the JSON-RPC error (nil when the call was accepted) and the
// tool's text.
func publicKnowledgeToolCall(t *testing.T, s *Server, tool, args string) (int, *jsonRPCError, string) {
	t.Helper()
	rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	resp := decodeRPC(t, rec)
	if resp.Error != nil {
		return rec.Code, resp.Error, ""
	}
	text, _ := rpcToolText(t, resp)
	return rec.Code, nil, text
}

// The tool schemas declare limit an integer. A client whose JSON stack writes
// every number as a float sends 1.0 for 1, which JSON Schema counts as an
// integer, and both tools refused the whole call as invalid arguments.
func TestPublicKnowledgeToolsAcceptAWholeFloatLimit(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServer(t)

	for _, limit := range []string{"1", "1.0", "1e0", "10E-1"} {
		status, rpcErr, text := publicKnowledgeToolCall(t, s, "knowledge_search", `{"query":"caching","limit":`+limit+`}`)
		if rpcErr != nil {
			t.Errorf("knowledge_search limit %s: HTTP %d, refused with %q", limit, status, rpcErr.Message)
			continue
		}
		var found struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal([]byte(text), &found); err != nil || found.Count != 1 {
			t.Errorf("knowledge_search limit %s: count = %d (err %v), want the limit of 1 applied: %s", limit, found.Count, err, text)
		}

		status, rpcErr, text = publicKnowledgeToolCall(t, s, "knowledge_toc", `{"limit":`+limit+`}`)
		if rpcErr != nil {
			t.Errorf("knowledge_toc limit %s: HTTP %d, refused with %q", limit, status, rpcErr.Message)
			continue
		}
		var toc struct {
			Returned int `json:"returned"`
		}
		if err := json.Unmarshal([]byte(text), &toc); err != nil || toc.Returned != 1 {
			t.Errorf("knowledge_toc limit %s: returned = %d (err %v), want the limit of 1 applied: %s", limit, toc.Returned, err, text)
		}
	}
}

// A whole number far above the maximum was accepted and clamped when limit was
// an int, and still is, whichever way it is written. A negative one still
// means the default.
func TestPublicKnowledgeToolsStillClampAHugeLimit(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServer(t)

	for _, limit := range []string{"10000000000", "1e10", "4611686018427387904", "-10000000000"} {
		for _, call := range []struct{ tool, args string }{
			{"knowledge_search", `{"query":"caching","limit":` + limit + `}`},
			{"knowledge_toc", `{"limit":` + limit + `}`},
		} {
			status, rpcErr, text := publicKnowledgeToolCall(t, s, call.tool, call.args)
			if rpcErr != nil {
				t.Errorf("%s limit %s: HTTP %d, refused with %q", call.tool, limit, status, rpcErr.Message)
				continue
			}
			var got struct {
				Count    int `json:"count"`
				Returned int `json:"returned"`
			}
			if err := json.Unmarshal([]byte(text), &got); err != nil || got.Count+got.Returned == 0 {
				t.Errorf("%s limit %s: no entries (err %v), want the facts under the clamped limit: %s", call.tool, limit, err, text)
			}
		}
	}
}

// A limit that is not a whole number is still refused, as are the other
// malformed forms.
func TestPublicKnowledgeToolsStillRefuseAMalformedLimit(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServer(t)

	for _, limit := range []string{"1.5", `"1"`, "true", "[1]", "1e99", "-1e99", "9223372036854775808"} {
		for _, call := range []struct{ tool, args string }{
			{"knowledge_search", `{"query":"caching","limit":` + limit + `}`},
			{"knowledge_toc", `{"limit":` + limit + `}`},
		} {
			status, rpcErr, text := publicKnowledgeToolCall(t, s, call.tool, call.args)
			if rpcErr == nil || rpcErr.Code != jsonRPCInvalidParams {
				t.Errorf("%s limit %s: HTTP %d error %+v text %q, want an invalid-params error", call.tool, limit, status, rpcErr, text)
			}
		}
	}
}
