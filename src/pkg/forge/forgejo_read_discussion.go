package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The reviews, comments and job logs half of ReadAPI (hivecommons/hive#20277).

// Review is one pull request review as an agent reads it.
type Review struct {
	ID          int64
	Author      string
	State       string
	Body        string
	CommitID    string
	Stale       bool
	Dismissed   bool
	Official    bool
	SubmittedAt time.Time
	URL         string
	// CommentCount is how many inline comments Forgejo reports.
	CommentCount int
	// CommentsRead is false when the review's comments were left unread
	// (the maxCommentReads budget of PullReviews ran out).
	CommentsRead bool
	Comments     []ReviewComment
}

// ReviewComment is one inline comment of a review.
type ReviewComment struct {
	ID        int64
	Author    string
	Path      string
	Line      int
	Body      string
	CommitID  string
	URL       string
	CreatedAt time.Time
}

// IssueComment is one comment on an issue or pull request.
type IssueComment struct {
	ID        int64
	Author    string
	Body      string
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ActionJob is one job of a Forgejo Actions run.
type ActionJob struct {
	ID         int64
	RunID      int64
	Name       string
	Status     string
	Conclusion string
	HeadSHA    string
	URL        string
}

func gtLogin(u *gtUser) string {
	if u == nil {
		return ""
	}
	return u.Login
}

func (f *forgejoForge) PullReviews(ctx context.Context, repo string, number, maxCommentReads int) ([]Review, error) {
	base := fmt.Sprintf("%s/pulls/%d/reviews", f.repoPath(repo), number)
	var reviews []Review
	err := f.paginate(ctx, base, nil, func(body []byte) (int, error) {
		var page []struct {
			ID            int64     `json:"id"`
			User          *gtUser   `json:"user"`
			State         string    `json:"state"`
			Body          string    `json:"body"`
			CommitID      string    `json:"commit_id"`
			Stale         bool      `json:"stale"`
			Dismissed     bool      `json:"dismissed"`
			Official      bool      `json:"official"`
			SubmittedAt   time.Time `json:"submitted_at"`
			HTMLURL       string    `json:"html_url"`
			CommentsCount int       `json:"comments_count"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding reviews: %w", err)
		}
		for _, r := range page {
			reviews = append(reviews, Review{
				ID: r.ID, Author: gtLogin(r.User), State: r.State, Body: r.Body, CommitID: r.CommitID,
				Stale: r.Stale, Dismissed: r.Dismissed, Official: r.Official, SubmittedAt: r.SubmittedAt,
				URL: r.HTMLURL, CommentCount: r.CommentsCount,
			})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: reviews %s#%d: %w", f.label, repo, number, err)
	}
	reads := 0
	for i := range reviews {
		r := &reviews[i]
		if r.CommentCount == 0 {
			r.CommentsRead = true
			continue
		}
		if reads >= maxCommentReads {
			continue
		}
		reads++
		var page []struct {
			ID               int64     `json:"id"`
			User             *gtUser   `json:"user"`
			Path             string    `json:"path"`
			Position         int       `json:"position"`
			OriginalPosition int       `json:"original_position"`
			Body             string    `json:"body"`
			CommitID         string    `json:"commit_id"`
			OriginalCommitID string    `json:"original_commit_id"`
			HTMLURL          string    `json:"html_url"`
			CreatedAt        time.Time `json:"created_at"`
		}
		if err := f.getJSON(ctx, fmt.Sprintf("%s/%d/comments", base, r.ID), nil, &page); err != nil {
			return nil, fmt.Errorf("%s: comments of review %d on %s#%d: %w", f.label, r.ID, repo, number, err)
		}
		r.CommentsRead = true
		for _, c := range page {
			line, commit := c.Position, c.CommitID
			if line == 0 {
				line = c.OriginalPosition
			}
			if commit == "" {
				commit = c.OriginalCommitID
			}
			r.Comments = append(r.Comments, ReviewComment{
				ID: c.ID, Author: gtLogin(c.User), Path: c.Path, Line: line, Body: c.Body, CommitID: commit,
				URL: c.HTMLURL, CreatedAt: c.CreatedAt,
			})
		}
	}
	return reviews, nil
}

func (f *forgejoForge) IssueComments(ctx context.Context, repo string, number int) ([]IssueComment, error) {
	var comments []IssueComment
	err := f.paginate(ctx, fmt.Sprintf("%s/issues/%d/comments", f.repoPath(repo), number), nil, func(body []byte) (int, error) {
		var page []struct {
			ID        int64     `json:"id"`
			User      *gtUser   `json:"user"`
			Body      string    `json:"body"`
			HTMLURL   string    `json:"html_url"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding comments: %w", err)
		}
		for _, c := range page {
			comments = append(comments, IssueComment{ID: c.ID, Author: gtLogin(c.User), Body: c.Body, URL: c.HTMLURL, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: comments %s#%d: %w", f.label, repo, number, err)
	}
	return comments, nil
}

func (f *forgejoForge) RunJobs(ctx context.Context, repo string, runID int64) ([]ActionJob, error) {
	var jobs []ActionJob
	err := f.paginate(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs", f.repoPath(repo), runID), nil, func(body []byte) (int, error) {
		var page struct {
			Jobs []struct {
				ID         int64  `json:"id"`
				RunID      int64  `json:"run_id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HeadSHA    string `json:"head_sha"`
				HTMLURL    string `json:"html_url"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding jobs: %w", err)
		}
		for _, j := range page.Jobs {
			jobs = append(jobs, ActionJob{ID: j.ID, RunID: j.RunID, Name: j.Name, Status: j.Status, Conclusion: j.Conclusion, HeadSHA: j.HeadSHA, URL: j.HTMLURL})
		}
		return len(page.Jobs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: jobs of run %d on %s: %w", f.label, runID, repo, err)
	}
	return jobs, nil
}

func (f *forgejoForge) JobLog(ctx context.Context, repo string, jobID int64, limit int) (string, bool, error) {
	if limit <= 0 {
		return "", false, fmt.Errorf("%s: job log: limit must be > 0", f.label)
	}
	endpoint := fmt.Sprintf("%s/actions/jobs/%d/logs", f.repoPath(repo), jobID)
	log, truncated, err := f.getTail(ctx, endpoint, limit)
	if err != nil {
		return "", false, fmt.Errorf("%s: log of job %d on %s: %w", f.label, jobID, repo, err)
	}
	return log, truncated, nil
}

// tailChunk is the read size of getTail.
const tailChunk = 32 << 10

// getTail issues a GET and keeps only the last limit bytes of the body, so a
// log of any size is read in bounded memory. A cut tail starts after its
// first newline, so it begins on a whole line.
func (f *forgejoForge) getTail(ctx context.Context, endpoint string, limit int) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+giteaAPIPath+endpoint, nil)
	if err != nil {
		return "", false, fmt.Errorf("building request: %w", err)
	}
	f.setAuth(req)
	req.Header.Set("Accept", "text/plain, */*")
	resp, err := f.http.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", false, &giteaStatusError{label: f.label, endpoint: endpoint, status: resp.StatusCode, body: truncate(string(data), 200)}
	}
	buf := make([]byte, 0, limit+tailChunk)
	chunk := make([]byte, tailChunk)
	truncated := false
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			if len(buf) > limit {
				truncated = true
				copy(buf, buf[len(buf)-limit:])
				buf = buf[:limit]
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", false, fmt.Errorf("reading body from %s: %w", endpoint, rerr)
		}
	}
	if truncated {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 && i+1 < len(buf) {
			buf = buf[i+1:]
		}
	}
	return string(buf), truncated, nil
}
