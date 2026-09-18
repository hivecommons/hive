package recommend

import (
	"fmt"
	"net/url"
)

// bucketQueryURL builds the GitHub search that reproduces a section in the
// reader's own browser.
//
// The digest caps each section, so without this a maintainer who wants the
// rest has to reconstruct a search by hand -- and a recommendation they
// cannot verify is one they have to take on faith. Linking the exact query
// makes every section auditable: click it and you see the same set, straight
// from GitHub, with no hive in the middle.
//
// Queries use only qualifiers GitHub evaluates server-side. Where a bucket's
// definition cannot be expressed as a query (conflicts and failing CI are
// computed from the merge state and check runs, not searchable), the link is
// the closest honest superset and the section says so rather than pretending
// the query is exact.
func (r Report) bucketQueryURL(b Bucket) string {
	if r.Repo == "" {
		return ""
	}
	base := "https://github.com/" + r.Repo + "/pulls?q="
	switch b {
	case BucketReady:
		return base + url.QueryEscape("is:pr is:open draft:false status:success sort:created-asc")
	case BucketConflicts:
		// Conflict state is not a search qualifier; this is every open,
		// non-draft PR, oldest first, which is the set the hive filtered.
		return base + url.QueryEscape("is:pr is:open draft:false sort:created-asc")
	case BucketRedCI:
		return base + url.QueryEscape("is:pr is:open draft:false status:failure sort:created-asc")
	case BucketDecision:
		if r.HumanDecisionLabel == "" {
			return ""
		}
		return base + url.QueryEscape(fmt.Sprintf("is:pr is:open label:%q sort:created-asc", r.HumanDecisionLabel))
	case BucketStaleDraft:
		return base + url.QueryEscape("is:pr is:open draft:true sort:created-asc")
	default:
		return ""
	}
}

