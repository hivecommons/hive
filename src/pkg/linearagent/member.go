package linearagent

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Workspace member lookup for the Customers profile fill (#19302). Linear
// exposes no company or location for a member, so only the name and avatar a
// member set in the connected workspace are read, and a member is found only
// by an email address the hive already holds for the person.

// ErrRateLimited is returned when Linear refused a request with 429.
var ErrRateLimited = errors.New("linear: rate limited")

// ErrMemberNotFound is returned when the connected workspace has no member
// with the email address.
var ErrMemberNotFound = errors.New("linear: no workspace member with that email")

// Member is the public part of a Linear workspace member.
type Member struct {
	Name        string
	DisplayName string
	AvatarURL   string
}

// memberByEmailQuery asks for at most one member with the exact email.
const memberByEmailQuery = `query MemberByEmail($email: String!) {
  users(filter: { email: { eq: $email } }, first: 1) { nodes { name displayName avatarUrl } }
}`

// FetchMemberByEmail makes ONE GraphQL request for the workspace member with
// this email address, with an explicit access token (the connected
// workspace's, from Client.AccessToken). A nil hc uses the package's default
// client, as FetchIdentity does.
func FetchMemberByEmail(ctx context.Context, hc *http.Client, graphqlURL, accessToken, email string) (Member, error) {
	email = strings.TrimSpace(email)
	if accessToken == "" {
		return Member{}, errors.New("linear: no access token")
	}
	if email == "" || !strings.Contains(email, "@") {
		return Member{}, errors.New("linear: invalid email")
	}
	if hc == nil {
		hc = &http.Client{Timeout: tokenHTTPTimeout}
	}
	if graphqlURL == "" {
		graphqlURL = GraphQLURL
	}
	var out struct {
		Users struct {
			Nodes []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				AvatarURL   string `json:"avatarUrl"`
			} `json:"nodes"`
		} `json:"users"`
	}
	if err := doGraphQL(ctx, hc, graphqlURL, accessToken, memberByEmailQuery, map[string]interface{}{"email": email}, &out); err != nil {
		if errors.Is(err, ErrRateLimited) {
			return Member{}, ErrRateLimited
		}
		return Member{}, errors.New("linear: member lookup failed")
	}
	if len(out.Users.Nodes) == 0 {
		return Member{}, ErrMemberNotFound
	}
	n := out.Users.Nodes[0]
	return Member{Name: n.Name, DisplayName: n.DisplayName, AvatarURL: n.AvatarURL}, nil
}
