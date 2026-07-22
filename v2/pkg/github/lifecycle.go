package github

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// UpsertLifecycleIssue creates or updates the one GitHub issue carrying the
// repository-scoped lifecycle marker. Searching by marker before creation makes
// retries safe even when GitHub accepted a write before Hive persisted it.
func (c *Client) UpsertLifecycleIssue(ctx context.Context, repository, marker, title, body string, labels []string) (int, string, bool, error) {
	writer, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, "", false, err
	}
	return c.upsertLifecycleIssueOwned(ctx, repository, marker, writer, 0, title, body, labels)
}

// UpsertLifecycleIssueOwned keeps issue ownership bound to the immutable
// writer recorded before the first remote mutation. The authenticated operator
// may change later, but Hive never adopts an issue authored by another user.
func (c *Client) UpsertLifecycleIssueOwned(ctx context.Context, repository, marker, writerLogin string, writerID int64, title, body string, labels []string) (int, string, bool, error) {
	if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
		return 0, "", false, fmt.Errorf("immutable lifecycle issue writer ID and login are required")
	}
	return c.upsertLifecycleIssueOwned(ctx, repository, marker, writerLogin, writerID, title, body, labels)
}

func (c *Client) upsertLifecycleIssueOwned(ctx context.Context, repository, marker, writer string, writerID int64, title, body string, labels []string) (int, string, bool, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", false, err
	}
	if strings.TrimSpace(marker) == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" {
		return 0, "", false, fmt.Errorf("lifecycle marker, title, and body are required")
	}
	issueNumber, err := c.findLifecycleIssue(ctx, owner, repo, marker, writer, writerID)
	if err != nil {
		return 0, "", false, err
	}
	if issueNumber == 0 {
		if legacyMarker := legacyVisualHiveMarker(body); legacyMarker != "" {
			issueNumber, err = c.findLifecycleIssue(ctx, owner, repo, legacyMarker, writer, writerID)
			if err != nil {
				return 0, "", false, err
			}
		}
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr("open")}
	if issueNumber > 0 {
		issue, _, err := c.client.Issues.Edit(ctx, owner, repo, issueNumber, request)
		if err != nil {
			return 0, "", false, fmt.Errorf("update lifecycle issue #%d: %w", issueNumber, err)
		}
		canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, writerID, request, issue)
		if err != nil {
			return 0, "", false, err
		}
		return canonical.GetNumber(), canonical.GetHTMLURL(), false, nil
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repo, request)
	if err != nil {
		return 0, "", false, fmt.Errorf("create lifecycle issue: %w", err)
	}
	if writerID > 0 && (issue.GetUser().GetID() != writerID || !strings.EqualFold(strings.TrimSpace(issue.GetUser().GetLogin()), strings.TrimSpace(writer))) {
		return 0, "", false, fmt.Errorf("created lifecycle issue author does not match immutable writer %s/%d", writer, writerID)
	}
	canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, writerID, request, issue)
	if err != nil {
		return 0, "", false, err
	}
	return canonical.GetNumber(), canonical.GetHTMLURL(), canonical.GetNumber() == issue.GetNumber(), nil
}

func legacyVisualHiveMarker(body string) string {
	const prefix = "<!-- visual-hive-issue dedupe:"
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "-->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start : start+end+3])
}

func (c *Client) UpdateLifecycleIssue(ctx context.Context, repository string, number int, title, body, state string, labels []string) (int, string, error) {
	writer, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, "", err
	}
	return c.updateLifecycleIssueOwned(ctx, repository, number, lifecycleMarkerFromBody(body), writer, 0, title, body, state, labels)
}

func (c *Client) UpdateLifecycleIssueOwned(ctx context.Context, repository string, number int, marker, writerLogin string, writerID int64, title, body, state string, labels []string) (int, string, error) {
	if writerID <= 0 || strings.TrimSpace(writerLogin) == "" || !isExactHiveLifecycleMarker(marker) {
		return 0, "", fmt.Errorf("exact lifecycle marker and immutable writer identity are required")
	}
	return c.updateLifecycleIssueOwned(ctx, repository, number, marker, writerLogin, writerID, title, body, state, labels)
}

func (c *Client) updateLifecycleIssueOwned(ctx context.Context, repository string, number int, marker, writer string, writerID int64, title, body, state string, labels []string) (int, string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", err
	}
	if number <= 0 || (state != "open" && state != "closed") {
		return 0, "", fmt.Errorf("valid lifecycle issue number and state are required")
	}
	if marker == "" || lifecycleMarkerFromBody(body) != marker {
		return 0, "", fmt.Errorf("lifecycle issue update is missing its exact Hive marker")
	}
	existing, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return 0, "", fmt.Errorf("read lifecycle issue #%d before update: %w", number, err)
	}
	if !isOwnedLifecycleIssue(existing, marker, writer, writerID) {
		return 0, "", fmt.Errorf("lifecycle issue #%d is not authored by immutable Hive writer %s/%d with the exact marker", number, writer, writerID)
	}
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: &labels, State: gh.Ptr(state)}
	issue, _, err := c.client.Issues.Edit(ctx, owner, repo, number, request)
	if err != nil {
		return 0, "", fmt.Errorf("update lifecycle issue #%d: %w", number, err)
	}
	canonical, err := c.reconcileLifecycleIssues(ctx, owner, repo, marker, writer, writerID, request, issue)
	if err != nil {
		return 0, "", err
	}
	return canonical.GetNumber(), canonical.GetHTMLURL(), nil
}

// MigrateLifecycleIssueMarker performs the one narrowly scoped ownership
// transition from an exact legacy Hive marker to a stable root marker. Unlike
// UpsertLifecycleIssue it never searches for or adopts a Visual Hive dedupe
// marker: the persisted repository, issue number, immutable author, and old
// Hive marker must all match before GitHub receives a mutation.
func (c *Client) MigrateLifecycleIssueMarker(ctx context.Context, repository string, number int, previousMarker, marker, title, body, state string, labels []string) (int, string, error) {
	writer, writerID, err := c.authenticatedLifecycleWriter(ctx)
	if err != nil {
		return 0, "", err
	}
	return c.migrateLifecycleIssueMarkerOwned(ctx, repository, number, previousMarker, marker, writer, writerID, title, body, state, labels)
}

func (c *Client) MigrateLifecycleIssueMarkerOwned(ctx context.Context, repository string, number int, previousMarker, marker, writerLogin string, writerID int64, title, body, state string, labels []string) (int, string, error) {
	if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
		return 0, "", fmt.Errorf("immutable lifecycle issue writer ID and login are required")
	}
	return c.migrateLifecycleIssueMarkerOwned(ctx, repository, number, previousMarker, marker, writerLogin, writerID, title, body, state, labels)
}

func (c *Client) migrateLifecycleIssueMarkerOwned(ctx context.Context, repository string, number int, previousMarker, marker, writer string, writerID int64, title, body, state string, labels []string) (int, string, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return 0, "", err
	}
	if number <= 0 || strings.TrimSpace(title) == "" || strings.TrimSpace(body) == "" || (state != "open" && state != "closed") {
		return 0, "", fmt.Errorf("valid lifecycle marker migration issue, title, body, and state are required")
	}
	if previousMarker == marker || !isExactHiveLifecycleMarker(previousMarker) || !isExactHiveLifecycleMarker(marker) {
		return 0, "", fmt.Errorf("marker migration requires distinct exact old and new Hive markers")
	}
	desiredMarkers := hiveLifecycleMarkers(body)
	if len(desiredMarkers) != 1 || desiredMarkers[0] != marker || len(markerMigrationReceipts(body)) != 0 {
		return 0, "", fmt.Errorf("marker migration body must contain exactly the new Hive marker")
	}
	existing, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return 0, "", fmt.Errorf("read lifecycle issue #%d before marker migration: %w", number, err)
	}
	if existing.GetNumber() != number || existing.IsPullRequest() || existing.GetUser().GetID() != writerID || !strings.EqualFold(strings.TrimSpace(existing.GetUser().GetLogin()), strings.TrimSpace(writer)) {
		return 0, "", fmt.Errorf("lifecycle issue #%d is not the exact issue authored by the authenticated Hive writer", number)
	}
	existingMarkers := hiveLifecycleMarkers(existing.GetBody())
	receipt := markerMigrationReceipt(previousMarker, marker)
	if len(existingMarkers) == 1 && existingMarkers[0] == marker {
		receipts := markerMigrationReceipts(existing.GetBody())
		if len(receipts) != 1 || receipts[0] != receipt {
			return 0, "", fmt.Errorf("lifecycle issue #%d has the new marker without the exact migration receipt", number)
		}
	} else if len(existingMarkers) != 1 || existingMarkers[0] != previousMarker || len(markerMigrationReceipts(existing.GetBody())) != 0 {
		return 0, "", fmt.Errorf("lifecycle issue #%d does not contain exactly the expected old Hive marker", number)
	}
	newOwners, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer, writerID)
	if err != nil {
		return 0, "", err
	}
	for _, candidate := range newOwners {
		if candidate.GetNumber() != number {
			return 0, "", fmt.Errorf("stable lifecycle marker is already owned by issue #%d", candidate.GetNumber())
		}
	}
	migrationBody := strings.TrimSpace(body) + "\n" + receipt
	request := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(migrationBody), Labels: &labels, State: gh.Ptr(state)}
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, request); err != nil {
		return 0, "", fmt.Errorf("migrate lifecycle issue #%d marker: %w", number, err)
	}
	confirmed, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return 0, "", fmt.Errorf("confirm lifecycle issue #%d marker migration: %w", number, err)
	}
	confirmedReceipts := markerMigrationReceipts(confirmed.GetBody())
	if confirmed.GetNumber() != number || !isOwnedLifecycleIssue(confirmed, marker, writer, writerID) || len(hiveLifecycleMarkers(confirmed.GetBody())) != 1 || len(confirmedReceipts) != 1 || confirmedReceipts[0] != receipt {
		return 0, "", fmt.Errorf("lifecycle issue #%d marker migration could not be confirmed", number)
	}
	return number, confirmed.GetHTMLURL(), nil
}

func (c *Client) authenticatedLifecycleWriter(ctx context.Context) (string, int64, error) {
	if c == nil || c.client == nil {
		return "", 0, fmt.Errorf("GitHub client is required")
	}
	user, _, err := c.client.Users.Get(ctx, "")
	if err != nil {
		return "", 0, fmt.Errorf("read authenticated GitHub identity: %w", err)
	}
	login, id := strings.TrimSpace(user.GetLogin()), user.GetID()
	if login == "" || id <= 0 {
		return "", 0, fmt.Errorf("authenticated GitHub identity has no immutable user identity")
	}
	return login, id, nil
}

// ResolveLifecycleIssueWriter recovers the immutable issue author from an
// exact persisted issue or exact-marker create-response recovery window. With
// no remote issue it returns the current authenticated identity so the caller
// can durably bind it before creation.
func (c *Client) ResolveLifecycleIssueWriter(ctx context.Context, repository, marker string, issueNumber int) (string, int64, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return "", 0, err
	}
	if issueNumber < 0 || !isExactHiveLifecycleMarker(marker) {
		return "", 0, fmt.Errorf("exact lifecycle marker and non-negative issue number are required")
	}
	if issueNumber > 0 {
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, issueNumber)
		if err != nil {
			return "", 0, fmt.Errorf("read lifecycle issue #%d while resolving immutable writer: %w", issueNumber, err)
		}
		if issue.GetNumber() != issueNumber || issue.IsPullRequest() || !issueHasExactLifecycleMarker(issue, marker) {
			return "", 0, fmt.Errorf("lifecycle issue #%d does not retain its exact Hive marker", issueNumber)
		}
		return exactLifecycleIssueAuthor(issue)
	}

	matches := []*gh.Issue{}
	page := 1
	for page > 0 {
		issues, response, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{State: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100}})
		if err != nil {
			return "", 0, fmt.Errorf("list lifecycle issues while resolving immutable writer: %w", err)
		}
		if response == nil {
			return "", 0, fmt.Errorf("list lifecycle issues while resolving immutable writer returned no pagination metadata")
		}
		for _, issue := range issues {
			if !issue.IsPullRequest() && issueHasExactLifecycleMarker(issue, marker) {
				matches = append(matches, issue)
			}
		}
		page = response.NextPage
	}
	if len(matches) == 0 {
		return c.authenticatedLifecycleWriter(ctx)
	}
	login, id, err := exactLifecycleIssueAuthor(matches[0])
	if err != nil {
		return "", 0, err
	}
	for _, issue := range matches[1:] {
		candidateLogin, candidateID, authorErr := exactLifecycleIssueAuthor(issue)
		if authorErr != nil {
			return "", 0, authorErr
		}
		if candidateID != id || !strings.EqualFold(candidateLogin, login) {
			return "", 0, fmt.Errorf("exact lifecycle marker is claimed by multiple immutable issue writers")
		}
	}
	return login, id, nil
}

func exactLifecycleIssueAuthor(issue *gh.Issue) (string, int64, error) {
	login, id := strings.TrimSpace(issue.GetUser().GetLogin()), issue.GetUser().GetID()
	if login == "" || id <= 0 {
		return "", 0, fmt.Errorf("lifecycle issue #%d has no immutable author identity", issue.GetNumber())
	}
	return login, id, nil
}

func issueHasExactLifecycleMarker(issue *gh.Issue, marker string) bool {
	if issue == nil || issue.IsPullRequest() {
		return false
	}
	markers := hiveLifecycleMarkers(issue.GetBody())
	return len(markers) == 1 && markers[0] == marker
}

func (c *Client) findLifecycleIssue(ctx context.Context, owner, repo, marker, writer string, writerID int64) (int, error) {
	issues, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer, writerID)
	if err != nil || len(issues) == 0 {
		return 0, err
	}
	return issues[0].GetNumber(), nil
}

func (c *Client) findLifecycleIssues(ctx context.Context, owner, repo, marker, writer string, writerID int64) ([]*gh.Issue, error) {
	matches := []*gh.Issue{}
	page := 1
	for page > 0 {
		issues, response, err := c.client.Issues.ListByRepo(ctx, owner, repo, &gh.IssueListByRepoOptions{
			State: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, fmt.Errorf("list lifecycle issues: %w", err)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			if isOwnedLifecycleIssue(issue, marker, writer, writerID) {
				matches = append(matches, issue)
			}
		}
		if response == nil {
			break
		}
		page = response.NextPage
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].GetNumber() < matches[j].GetNumber() })
	return matches, nil
}

// reconcileLifecycleIssues is the compensating half of issue creation. GitHub
// has no conditional-create primitive for issues, so two independent Hive
// state directories can both win a list-then-create race. Every create/update
// deterministically elects the lowest exact-marker issue, reopens or closes it
// to the requested state, and closes every higher-numbered duplicate. A later
// production update repeats this reconciliation, covering API list visibility
// lag after concurrent creates.
func (c *Client) reconcileLifecycleIssues(ctx context.Context, owner, repo, marker, writer string, writerID int64, desired *gh.IssueRequest, fallback *gh.Issue) (*gh.Issue, error) {
	matches, err := c.findLifecycleIssues(ctx, owner, repo, marker, writer, writerID)
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		if fallback == nil || fallback.GetNumber() <= 0 || !isOwnedLifecycleIssue(fallback, marker, writer, writerID) {
			return nil, fmt.Errorf("GitHub did not return the lifecycle issue after mutation")
		}
		return fallback, nil
	}
	canonical := matches[0]
	canonical, _, err = c.client.Issues.Edit(ctx, owner, repo, canonical.GetNumber(), desired)
	if err != nil {
		return nil, fmt.Errorf("reconcile canonical lifecycle issue #%d: %w", matches[0].GetNumber(), err)
	}
	for _, duplicate := range matches[1:] {
		duplicateBody := duplicate.GetBody()
		if start := strings.Index(duplicateBody, "\n\n---\nHive automatically closed this duplicate lifecycle issue."); start >= 0 {
			duplicateBody = duplicateBody[:start]
		}
		duplicateBody = strings.TrimSpace(duplicateBody) + fmt.Sprintf("\n\n---\nHive automatically closed this duplicate lifecycle issue. Canonical issue: #%d.\n<!-- hive-duplicate-of: %d -->", canonical.GetNumber(), canonical.GetNumber())
		labels := duplicateLifecycleLabels(desired.GetLabels())
		request := &gh.IssueRequest{
			Title: gh.Ptr(duplicate.GetTitle()), Body: gh.Ptr(duplicateBody), Labels: &labels, State: gh.Ptr("closed"),
		}
		if _, _, err := c.client.Issues.Edit(ctx, owner, repo, duplicate.GetNumber(), request); err != nil {
			return nil, fmt.Errorf("close duplicate lifecycle issue #%d in favor of #%d: %w", duplicate.GetNumber(), canonical.GetNumber(), err)
		}
		if c.logger != nil {
			c.logger.Warn("closed duplicate Hive lifecycle issue", "canonical", canonical.GetNumber(), "duplicate", duplicate.GetNumber())
		}
	}
	return canonical, nil
}

func isOwnedLifecycleIssue(issue *gh.Issue, marker, writer string, writerID int64) bool {
	if issue == nil || issue.IsPullRequest() || !strings.EqualFold(strings.TrimSpace(issue.GetUser().GetLogin()), strings.TrimSpace(writer)) {
		return false
	}
	if writerID > 0 && issue.GetUser().GetID() != writerID {
		return false
	}
	count := 0
	for _, line := range strings.Split(strings.ReplaceAll(issue.GetBody(), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == marker {
			count++
		}
	}
	return count == 1
}

func lifecycleMarkerFromBody(body string) string {
	const prefix = "<!-- hive-visual-fingerprint:"
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "-->")
	if end < 0 {
		return ""
	}
	marker := strings.TrimSpace(body[start : start+end+3])
	if strings.Count(body, marker) != 1 {
		return ""
	}
	return marker
}

func hiveLifecycleMarkers(body string) []string {
	const prefix = "<!-- hive-visual-fingerprint:"
	markers := make([]string, 0, 1)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			markers = append(markers, line)
		}
	}
	return markers
}

func isExactHiveLifecycleMarker(marker string) bool {
	markers := hiveLifecycleMarkers(marker)
	if len(markers) != 1 || markers[0] != strings.TrimSpace(marker) || lifecycleMarkerFromBody(marker) != markers[0] {
		return false
	}
	const prefix = "<!-- hive-visual-fingerprint: "
	const suffix = " -->"
	if !strings.HasPrefix(markers[0], prefix) || !strings.HasSuffix(markers[0], suffix) {
		return false
	}
	fingerprint := strings.TrimSuffix(strings.TrimPrefix(markers[0], prefix), suffix)
	if len(fingerprint) != 64 {
		return false
	}
	for _, value := range fingerprint {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func markerMigrationReceipt(previousMarker, marker string) string {
	sum := sha256.Sum256([]byte(previousMarker + "\x00" + marker))
	return fmt.Sprintf("<!-- hive-marker-migration: %x -->", sum[:])
}

func markerMigrationReceipts(body string) []string {
	const prefix = "<!-- hive-marker-migration:"
	receipts := make([]string, 0, 1)
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			receipts = append(receipts, line)
		}
	}
	return receipts
}

func duplicateLifecycleLabels(desired []string) []string {
	values := map[string]bool{"hive/managed": true, "hive/resolved": true}
	for _, label := range desired {
		label = strings.TrimSpace(label)
		if label != "" && label != "hive/active" && label != "visual-hive/ready-for-hive" && label != "visual-hive/still-active" {
			values[label] = true
		}
	}
	labels := make([]string, 0, len(values))
	for label := range values {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

func splitFullRepository(repository string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("repository must be owner/name")
	}
	return parts[0], parts[1], nil
}
