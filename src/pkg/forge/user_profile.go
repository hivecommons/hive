package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Public user profiles (hivecommons/hive#19302). The Customers profile fill
// reads the name, company, location, website and avatar a person published on
// their own forge profile. The lookups go through the adapter's own
// http.Client and token, so they share the transport, proxy environment and
// timeouts of every other forge call; there is no second client.
//
// Errors never echo the request URL or the response body: the URL holds the
// login being looked up and callers log errors.

// UserProfile is the public part of a forge user profile that the Customers
// profile fill reads. Every field is what the person typed themselves.
type UserProfile struct {
	Login     string
	Name      string
	Company   string
	Location  string
	Website   string
	AvatarURL string
}

// UserProfiler is the optional extension a Forge implements when it can look
// up a user's public profile. Callers type-assert for it.
type UserProfiler interface {
	// UserProfile makes at most UserProfileMaxRequests requests for the
	// public profile of login. It returns ErrUserNotFound when the forge knows no such
	// user and ErrRateLimited when the forge refused for a rate-limit reason.
	UserProfile(ctx context.Context, login string) (UserProfile, error)
}

var (
	// ErrUserNotFound is returned when the forge has no user with the login.
	ErrUserNotFound = errors.New("forge: user not found")
	// ErrRateLimited is returned when the forge refused a request with 429,
	// or with 403 and an exhausted rate-limit header.
	ErrRateLimited = errors.New("forge: rate limited")
	// errInvalidLogin is returned for a login that cannot be a forge username.
	errInvalidLogin = errors.New("forge: invalid login")
)

const (
	// UserProfileMaxRequests is the most requests one UserProfile call makes
	// on any adapter (GitLab needs a username search, then the profile).
	UserProfileMaxRequests = 2
	// userProfileMaxBody caps how much of one profile response is read.
	userProfileMaxBody = 64 << 10
	// rateLimitRemainingHeader is the header GitLab and Gitea set on a
	// throttled response; "0" means the window is spent.
	rateLimitRemainingHeader = "RateLimit-Remaining"
	// gitLabUserSearchPerPage is the page size of the username lookup: a
	// username is unique, so one result is enough.
	gitLabUserSearchPerPage = 1
)

// validForgeLogin reports whether login is safe to put in a URL path segment:
// no whitespace, slash, colon, query or fragment characters.
func validForgeLogin(login string) bool {
	return login != "" && !strings.ContainsAny(login, " \t\r\n/:?#%\\")
}

// classifyProfileStatus maps a non-2xx status to a fixed error.
func classifyProfileStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrUserNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrRateLimited
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get(rateLimitRemainingHeader) == "0":
		return ErrRateLimited
	default:
		return fmt.Errorf("forge: profile lookup returned status %d", resp.StatusCode)
	}
}

// getProfileJSON is one GET through client with the auth set by setAuth. It
// decodes at most userProfileMaxBody bytes of a 2xx body into out.
func getProfileJSON(ctx context.Context, client *http.Client, endpoint string, setAuth func(*http.Request), out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("forge: profile lookup: bad request")
	}
	setAuth(req)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("forge: profile lookup: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, userProfileMaxBody)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, body)
		return classifyProfileStatus(resp)
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return errors.New("forge: profile lookup: undecodable response")
	}
	return nil
}

// glUserSummary is the username-search result: only the numeric id is read.
type glUserSummary struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

// glUserDetail is the subset of GET /users/:id the profile fill reads.
type glUserDetail struct {
	Username     string `json:"username"`
	Name         string `json:"name"`
	Organization string `json:"organization"`
	Location     string `json:"location"`
	WebsiteURL   string `json:"website_url"`
	AvatarURL    string `json:"avatar_url"`
}

func (f *gitLabForge) setProfileAuth(req *http.Request) {
	if f.token != "" {
		req.Header.Set(gitLabTokenHeader, f.token)
	}
}

// UserProfile looks a GitLab username up (GET /users?username=, one result)
// and reads the user's public profile (GET /users/:id): two requests. The
// username search answers only with the summary, which carries no
// organization or location.
func (f *gitLabForge) UserProfile(ctx context.Context, login string) (UserProfile, error) {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if !validForgeLogin(login) {
		return UserProfile{}, errInvalidLogin
	}
	q := url.Values{"username": {login}, "per_page": {strconv.Itoa(gitLabUserSearchPerPage)}}
	var found []glUserSummary
	if err := getProfileJSON(ctx, f.http, f.baseURL+gitLabAPIPath+"/users?"+q.Encode(), f.setProfileAuth, &found); err != nil {
		return UserProfile{}, err
	}
	if len(found) == 0 || found[0].ID <= 0 || !strings.EqualFold(found[0].Username, login) {
		return UserProfile{}, ErrUserNotFound
	}
	var u glUserDetail
	if err := getProfileJSON(ctx, f.http, f.baseURL+gitLabAPIPath+"/users/"+strconv.Itoa(found[0].ID), f.setProfileAuth, &u); err != nil {
		return UserProfile{}, err
	}
	return UserProfile{Login: u.Username, Name: u.Name, Company: u.Organization, Location: u.Location,
		Website: u.WebsiteURL, AvatarURL: u.AvatarURL}, nil
}

// gtUserDetail is the subset of GET /users/{username} the profile fill reads.
// Gitea and Forgejo have no company field.
type gtUserDetail struct {
	Login     string `json:"login"`
	FullName  string `json:"full_name"`
	Location  string `json:"location"`
	Website   string `json:"website"`
	AvatarURL string `json:"avatar_url"`
}

// UserProfile reads a Gitea/Forgejo user's public profile with one request
// (GET /users/{username}).
func (f *giteaForge) UserProfile(ctx context.Context, login string) (UserProfile, error) {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if !validForgeLogin(login) {
		return UserProfile{}, errInvalidLogin
	}
	var u gtUserDetail
	if err := getProfileJSON(ctx, f.http, f.baseURL+giteaAPIPath+"/users/"+url.PathEscape(login), f.setAuth, &u); err != nil {
		return UserProfile{}, err
	}
	return UserProfile{Login: u.Login, Name: u.FullName, Location: u.Location, Website: u.Website, AvatarURL: u.AvatarURL}, nil
}
