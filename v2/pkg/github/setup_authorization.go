package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	setupAuthorizationStatusPrefix  = "hive/setup-authorized/"
	setupAuthorizationStatusPages   = 10
	setupAuthorizationStatusPerPage = 100
)

var (
	setupAuthorizationStatusSHA     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	setupAuthorizationStatusContext = regexp.MustCompile(`^` + regexp.QuoteMeta(setupAuthorizationStatusPrefix) + `[a-f0-9]{64}$`)
)

// AuthenticatedUserIdentity is the stable user principal allowed to authorize
// a setup status. Hive deliberately rejects bot/App identities here: a
// repository pull_request GITHUB_TOKEN is a GitHub Actions App token and must
// never be able to become the out-of-band authorizer.
type AuthenticatedUserIdentity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// SetupAuthorizationStatusRequest creates one exact status on one immutable
// setup head. ExpectedCreatorID must come from AuthenticatedNumericUser before
// the managed workflow containing that ID is committed.
type SetupAuthorizationStatusRequest struct {
	Repository           string
	HeadSHA              string
	Context              string
	TargetURL            string
	Description          string
	ExpectedCreatorID    int64
	ExpectedCreatorLogin string
	ExpectedCreatorType  string
}

// SetupAuthorizationStatusResult is the read-back proof. Recovered indicates
// that the POST returned an ambiguous error but the exact status was found;
// Reused indicates that no POST was needed.
type SetupAuthorizationStatusResult struct {
	StatusID     int64     `json:"status_id"`
	Context      string    `json:"context"`
	CreatorID    int64     `json:"creator_id"`
	CreatorLogin string    `json:"creator_login"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	Reused       bool      `json:"reused"`
	Recovered    bool      `json:"recovered"`
}

// AuthenticatedNumericUser returns a human GitHub user with a stable numeric
// ID. App installation tokens and github-actions[bot] fail closed.
func (c *Client) AuthenticatedNumericUser(ctx context.Context) (AuthenticatedUserIdentity, error) {
	if c == nil || c.client == nil {
		return AuthenticatedUserIdentity{}, fmt.Errorf("GitHub client is required to read the setup authorizer")
	}
	user, _, err := c.client.Users.Get(ctx, "")
	if err != nil {
		return AuthenticatedUserIdentity{}, fmt.Errorf("read authenticated setup authorizer: %w", err)
	}
	identity := AuthenticatedUserIdentity{ID: user.GetID(), Login: strings.TrimSpace(user.GetLogin()), Type: strings.TrimSpace(user.GetType())}
	if identity.ID <= 0 || identity.Login == "" || !strings.EqualFold(identity.Type, "User") || strings.EqualFold(identity.Login, "github-actions[bot]") {
		return AuthenticatedUserIdentity{}, fmt.Errorf("setup authorization requires a numeric human GitHub user identity; got login=%q id=%d type=%q", identity.Login, identity.ID, identity.Type)
	}
	return identity, nil
}

// ResolveHumanNumericUser resolves an explicitly named transfer target to its
// stable numeric GitHub identity. Bots, Apps, empty identities, and a response
// for a different login fail closed.
func (c *Client) ResolveHumanNumericUser(ctx context.Context, login string) (AuthenticatedUserIdentity, error) {
	if c == nil || c.client == nil {
		return AuthenticatedUserIdentity{}, fmt.Errorf("GitHub client is required to resolve the setup authorizer transfer target")
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return AuthenticatedUserIdentity{}, fmt.Errorf("setup authorizer transfer target login is required")
	}
	user, _, err := c.client.Users.Get(ctx, login)
	if err != nil {
		return AuthenticatedUserIdentity{}, fmt.Errorf("resolve setup authorizer transfer target %q: %w", login, err)
	}
	identity := AuthenticatedUserIdentity{ID: user.GetID(), Login: strings.TrimSpace(user.GetLogin()), Type: strings.TrimSpace(user.GetType())}
	if identity.ID <= 0 || identity.Login == "" || !strings.EqualFold(identity.Type, "User") || strings.EqualFold(identity.Login, "github-actions[bot]") || !strings.EqualFold(identity.Login, login) {
		return AuthenticatedUserIdentity{}, fmt.Errorf("setup authorizer transfer requires an exact numeric human GitHub User; requested=%q got login=%q id=%d type=%q", login, identity.Login, identity.ID, identity.Type)
	}
	return identity, nil
}

// VerifySetupAuthorizationStatus reads the latest status for one exact
// head/context and proves its human creator and pull-request target without
// creating or changing a status. It supports crash recovery after Hive posted
// authorization but before the durable transfer checkpoint was advanced.
func (c *Client) VerifySetupAuthorizationStatus(ctx context.Context, request SetupAuthorizationStatusRequest) (SetupAuthorizationStatusResult, error) {
	if c == nil || c.client == nil {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("GitHub client is required to verify setup authorization")
	}
	owner, repository, err := splitFullRepository(request.Repository)
	if err != nil {
		return SetupAuthorizationStatusResult{}, err
	}
	request.HeadSHA = strings.ToLower(strings.TrimSpace(request.HeadSHA))
	request.Context = strings.ToLower(strings.TrimSpace(request.Context))
	request.TargetURL = strings.TrimSpace(request.TargetURL)
	expected := expectedSetupStatusCreator(request)
	if !setupAuthorizationStatusSHA.MatchString(request.HeadSHA) || !setupAuthorizationStatusContext.MatchString(request.Context) || expected.ID <= 0 || request.TargetURL == "" {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("setup status verification requires an exact head SHA, canonical authorization context, pull-request target, and expected creator ID")
	}
	latest, found, err := c.latestSetupAuthorizationStatus(ctx, owner, repository, request.HeadSHA, request.Context)
	if err != nil {
		return SetupAuthorizationStatusResult{}, err
	}
	if !found {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("latest exact-head setup authorization status is absent")
	}
	if err := validLatestSetupAuthorizationStatus(latest, request.Context, request.TargetURL, expected); err != nil {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("latest exact-head setup authorization status is invalid: %w", err)
	}
	identity := AuthenticatedUserIdentity{ID: latest.GetCreator().GetID(), Login: latest.GetCreator().GetLogin(), Type: latest.GetCreator().GetType()}
	return setupAuthorizationStatusResult(latest, identity, true, false), nil
}

// EnsureSetupAuthorizationStatus creates and independently reads back a
// success status on the exact head SHA. The list endpoint is reverse
// chronological; only the latest matching context is accepted. This prevents
// an older valid status from masking a later foreign, pending, or failed one.
func (c *Client) EnsureSetupAuthorizationStatus(ctx context.Context, request SetupAuthorizationStatusRequest) (SetupAuthorizationStatusResult, error) {
	if c == nil || c.client == nil {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("GitHub client is required to authorize setup")
	}
	owner, repository, err := splitFullRepository(request.Repository)
	if err != nil {
		return SetupAuthorizationStatusResult{}, err
	}
	request.HeadSHA = strings.ToLower(strings.TrimSpace(request.HeadSHA))
	request.Context = strings.ToLower(strings.TrimSpace(request.Context))
	request.TargetURL = strings.TrimSpace(request.TargetURL)
	request.Description = strings.TrimSpace(request.Description)
	expected := expectedSetupStatusCreator(request)
	if !setupAuthorizationStatusSHA.MatchString(request.HeadSHA) || !setupAuthorizationStatusContext.MatchString(request.Context) || expected.ID <= 0 {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("setup status requires an exact head SHA, canonical authorization context, and expected creator ID")
	}
	if request.TargetURL == "" || len(request.Description) == 0 || len(request.Description) > 140 {
		return SetupAuthorizationStatusResult{}, fmt.Errorf("setup status requires a target URL and a 1-140 character description")
	}
	identity := expected
	if strings.EqualFold(expected.Type, "Bot") {
		if strings.TrimSpace(expected.Login) == "" || !c.verifiedAppWriter(expected) {
			return SetupAuthorizationStatusResult{}, fmt.Errorf("setup status App writer is not bound to the verified dashboard handoff")
		}
	} else {
		var err error
		identity, err = c.AuthenticatedNumericUser(ctx)
		if err != nil {
			return SetupAuthorizationStatusResult{}, err
		}
		if identity.ID != expected.ID || (expected.Login != "" && !strings.EqualFold(identity.Login, expected.Login)) {
			return SetupAuthorizationStatusResult{}, fmt.Errorf("authenticated setup authorizer ID %d does not match workflow-bound ID %d", identity.ID, expected.ID)
		}
	}

	latest, found, err := c.latestSetupAuthorizationStatus(ctx, owner, repository, request.HeadSHA, request.Context)
	if err != nil {
		return SetupAuthorizationStatusResult{}, err
	}
	if found && validLatestSetupAuthorizationStatus(latest, request.Context, request.TargetURL, identity) == nil {
		return setupAuthorizationStatusResult(latest, identity, true, false), nil
	}

	created, response, createErr := c.client.Repositories.CreateStatus(ctx, owner, repository, request.HeadSHA, &gh.RepoStatus{
		State: gh.Ptr("success"), Context: gh.Ptr(request.Context), TargetURL: gh.Ptr(request.TargetURL), Description: gh.Ptr(request.Description),
	})
	if createErr == nil {
		if response == nil || response.StatusCode != http.StatusCreated {
			createErr = fmt.Errorf("create exact setup authorization status returned no HTTP 201 response")
		} else if validationErr := validLatestSetupAuthorizationStatus(created, request.Context, request.TargetURL, identity); validationErr != nil {
			// The mutation may have succeeded even if an intermediary stripped or
			// damaged the response body. Treat it as ambiguous and recover only
			// through an exact-head status read-back.
			createErr = fmt.Errorf("created setup authorization status response was not exact: %w", validationErr)
		}
	}

	readBack, readFound, readErr := c.latestSetupAuthorizationStatus(ctx, owner, repository, request.HeadSHA, request.Context)
	if readErr != nil {
		if createErr != nil {
			return SetupAuthorizationStatusResult{}, errors.Join(fmt.Errorf("create exact setup authorization status: %w", createErr), fmt.Errorf("recover setup authorization status: %w", readErr))
		}
		return SetupAuthorizationStatusResult{}, readErr
	}
	if !readFound {
		if createErr != nil {
			return SetupAuthorizationStatusResult{}, fmt.Errorf("create exact setup authorization status: %w; exact status was absent during recovery", createErr)
		}
		return SetupAuthorizationStatusResult{}, fmt.Errorf("created setup authorization status was absent during exact-head read-back")
	}
	if err := validLatestSetupAuthorizationStatus(readBack, request.Context, request.TargetURL, identity); err != nil {
		if createErr != nil {
			return SetupAuthorizationStatusResult{}, errors.Join(fmt.Errorf("create exact setup authorization status: %w", createErr), fmt.Errorf("recover exact setup authorization status: %w", err))
		}
		return SetupAuthorizationStatusResult{}, fmt.Errorf("latest setup authorization status changed before read-back: %w", err)
	}
	return setupAuthorizationStatusResult(readBack, identity, false, createErr != nil), nil
}

func (c *Client) latestSetupAuthorizationStatus(ctx context.Context, owner, repository, headSHA, expectedContext string) (*gh.RepoStatus, bool, error) {
	options := &gh.ListOptions{Page: 1, PerPage: setupAuthorizationStatusPerPage}
	for scannedPages := 0; scannedPages < setupAuthorizationStatusPages; scannedPages++ {
		statuses, response, err := c.client.Repositories.ListStatuses(ctx, owner, repository, headSHA, options)
		if err != nil {
			return nil, false, fmt.Errorf("list exact-head setup authorization statuses: %w", err)
		}
		for _, status := range statuses {
			if status != nil && strings.EqualFold(strings.TrimSpace(status.GetContext()), expectedContext) {
				return status, true, nil
			}
		}
		if response == nil || response.NextPage == 0 {
			return nil, false, nil
		}
		options.Page = response.NextPage
	}
	return nil, false, fmt.Errorf("setup authorization status discovery exceeded %d bounded pages", setupAuthorizationStatusPages)
}

func validLatestSetupAuthorizationStatus(status *gh.RepoStatus, expectedContext, expectedTargetURL string, expectedCreator AuthenticatedUserIdentity) error {
	if status == nil {
		return fmt.Errorf("status is absent")
	}
	if status.GetID() <= 0 {
		return fmt.Errorf("status has no immutable numeric ID")
	}
	if !strings.EqualFold(strings.TrimSpace(status.GetContext()), expectedContext) {
		return fmt.Errorf("status context %q does not match %q", status.GetContext(), expectedContext)
	}
	if !strings.EqualFold(strings.TrimSpace(status.GetState()), "success") {
		return fmt.Errorf("latest exact status state is %q, not success", status.GetState())
	}
	if strings.TrimSpace(status.GetTargetURL()) != strings.TrimSpace(expectedTargetURL) {
		return fmt.Errorf("latest exact status target URL %q does not match %q", status.GetTargetURL(), expectedTargetURL)
	}
	expectedType := strings.TrimSpace(expectedCreator.Type)
	if expectedType == "" {
		expectedType = "User"
	}
	if status.Creator == nil || status.Creator.GetID() != expectedCreator.ID || strings.TrimSpace(status.Creator.GetLogin()) == "" ||
		(expectedCreator.Login != "" && !strings.EqualFold(strings.TrimSpace(status.Creator.GetLogin()), expectedCreator.Login)) ||
		!strings.EqualFold(strings.TrimSpace(status.Creator.GetType()), expectedType) {
		creatorID, creatorLogin, creatorType := int64(0), "", ""
		if status.Creator != nil {
			creatorID, creatorLogin, creatorType = status.Creator.GetID(), status.Creator.GetLogin(), status.Creator.GetType()
		}
		return fmt.Errorf("latest exact status creator login=%q id=%d type=%q does not match expected writer login=%q id=%d type=%q", creatorLogin, creatorID, creatorType, expectedCreator.Login, expectedCreator.ID, expectedType)
	}
	return nil
}

func expectedSetupStatusCreator(request SetupAuthorizationStatusRequest) AuthenticatedUserIdentity {
	return AuthenticatedUserIdentity{ID: request.ExpectedCreatorID, Login: strings.TrimSpace(request.ExpectedCreatorLogin), Type: strings.TrimSpace(request.ExpectedCreatorType)}
}

func setupAuthorizationStatusResult(status *gh.RepoStatus, identity AuthenticatedUserIdentity, reused, recovered bool) SetupAuthorizationStatusResult {
	result := SetupAuthorizationStatusResult{
		StatusID: status.GetID(), Context: strings.ToLower(strings.TrimSpace(status.GetContext())), CreatorID: identity.ID, CreatorLogin: identity.Login,
		Reused: reused, Recovered: recovered,
	}
	if status.CreatedAt != nil {
		result.CreatedAt = status.CreatedAt.Time
	}
	return result
}
