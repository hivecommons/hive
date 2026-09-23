package dashboard

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

// reviewOutcomesMaxWindowDays caps the summary window so a typo cannot ask
// for a scan far beyond what the ledger retains (OutcomeRetention).
const reviewOutcomesMaxWindowDays = 90

// handleReviewOutcomes serves GET /api/review/outcomes?days=N: the reviewed
// vs unreviewed merge statistics from the review-outcome ledger. This is the
// number that answers "does reviewing PRs shrink the queue?" — verdict counts
// alone cannot.
func (s *Server) handleReviewOutcomes(w http.ResponseWriter, r *http.Request) {
	window := review.OutcomeSummaryDefaultWindow
	if raw := r.URL.Query().Get("days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > reviewOutcomesMaxWindowDays {
			jsonError(w, "days must be an integer between 1 and "+strconv.Itoa(reviewOutcomesMaxWindowDays), http.StatusBadRequest)
			return
		}
		window = time.Duration(days) * 24 * time.Hour
	}
	ledger, err := review.LoadOutcomeLedger("")
	if err != nil {
		jsonError(w, "review outcomes ledger unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, ledger.Summary(time.Now(), window))
}

// reviewOutcomePrometheusSeries flattens the 30-day summary into the two
// series the metrics endpoint emits: PR counts by cohort × outcome, and the
// median first-seen → merged hours by cohort.
type reviewOutcomePrometheusSeries struct {
	Cohort  string
	Outcome string
	Count   int
	Median  float64
}

func prometheusReviewOutcomeSeries(now time.Time) (counts []reviewOutcomePrometheusSeries, medians []reviewOutcomePrometheusSeries) {
	ledger, err := review.LoadOutcomeLedger("")
	if err != nil || ledger == nil || len(ledger.Items) == 0 {
		return nil, nil
	}
	sum := ledger.Summary(now, review.OutcomeSummaryDefaultWindow)
	for _, c := range []struct {
		name string
		g    review.OutcomeGroup
	}{{"reviewed", sum.Reviewed}, {"unreviewed", sum.Unreviewed}} {
		counts = append(counts,
			reviewOutcomePrometheusSeries{Cohort: c.name, Outcome: review.OutcomeMerged, Count: c.g.Merged},
			reviewOutcomePrometheusSeries{Cohort: c.name, Outcome: review.OutcomeClosed, Count: c.g.Closed},
			reviewOutcomePrometheusSeries{Cohort: c.name, Outcome: review.OutcomeOpen, Count: c.g.Open},
		)
		medians = append(medians, reviewOutcomePrometheusSeries{Cohort: c.name, Median: c.g.MedianHoursSeenToMerge})
	}
	return counts, medians
}
