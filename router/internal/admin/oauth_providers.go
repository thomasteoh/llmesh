package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// The providers this router can federate sign-in to. Adding one means adding a
// descriptor here, a pair of columns in the users table, and a card on the
// settings page; the flow itself is provider-agnostic.
const (
	providerGitHub = "github"
	providerGoogle = "google"
	providerOIDC   = "oidc"
)

// oauthProviderOrder is the order providers appear on the login and settings
// pages, so the two cannot drift apart.
var oauthProviderOrder = []string{providerGitHub, providerGoogle, providerOIDC}

var oauthProviders = map[string]oauthProvider{
	providerGitHub: {
		key:          providerGitHub,
		name:         "GitHub",
		authorizeURL: "https://github.com/login/oauth/authorize",
		tokenURL:     "https://github.com/login/oauth/access_token",
		userInfoURL:  "https://api.github.com/user",
		// read:user is all this needs: an account id and a handle to show. No
		// email scope, because the identity is matched on the linked account id
		// and never on an address.
		scope: "read:user",
		// A sign-up offer would be misleading: a GitHub account this router has
		// never seen cannot sign in to it.
		extraAuthParams: map[string]string{"allow_signup": "false"},
		identity:        githubIdentity,
		get:             func(u User) oauthIdentity { return oauthIdentity{ID: u.GitHubUserID, Label: u.GitHubLogin} },
		set: func(u *User, i oauthIdentity) {
			u.GitHubUserID = i.ID
			u.GitHubLogin = i.Label
		},
	},
	providerGoogle: {
		key:          providerGoogle,
		name:         "Google",
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		userInfoURL:  "https://openidconnect.googleapis.com/v1/userinfo",
		// openid+email yields the stable subject id and an address to show.
		// No profile scope: a name this router would only ever display is not
		// worth asking a user to hand over.
		scope: "openid email",
		// select_account, because a browser signed in to several Google
		// accounts would otherwise pick one silently, and a user linking an
		// identity needs to see which one they are about to attach.
		extraAuthParams: map[string]string{"prompt": "select_account"},
		// Google requires the grant type; GitHub neither needs nor minds it,
		// so it is set per-provider rather than globally.
		extraTokenParams: map[string]string{"grant_type": "authorization_code"},
		identity:         googleIdentity,
		get:              func(u User) oauthIdentity { return oauthIdentity{ID: u.GoogleUserID, Label: u.GoogleEmail} },
		set: func(u *User, i oauthIdentity) {
			u.GoogleUserID = i.ID
			u.GoogleEmail = i.Label
		},
	},
	// The OIDC descriptor is a template: its name and endpoints are the
	// admin's, and providerFor fills them in from settings.
	providerOIDC: {
		key:  providerOIDC,
		name: defaultOIDCName,
		// profile adds preferred_username, the label shown when a provider
		// reports no verified address. Nothing else in it is read.
		scope:            "openid email profile",
		extraTokenParams: map[string]string{"grant_type": "authorization_code"},
		pkce:             true,
		identity:         oidcIdentity,
		get:              func(u User) oauthIdentity { return oauthIdentity{ID: u.OIDCSubject, Label: u.OIDCLabel} },
		set: func(u *User, i oauthIdentity) {
			u.OIDCSubject = i.ID
			u.OIDCLabel = i.Label
		},
	},
}

// githubIdentity reads GitHub's user object. The numeric id is immutable; the
// login is the handle at this moment and is kept only to display.
func githubIdentity(body []byte) (oauthIdentity, error) {
	var v struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return oauthIdentity{}, fmt.Errorf("decode github user: %w", err)
	}
	if v.ID == 0 {
		return oauthIdentity{}, nil
	}
	return oauthIdentity{ID: strconv.FormatInt(v.ID, 10), Label: v.Login}, nil
}

// googleIdentity reads an OpenID Connect userinfo document.
//
// The subject is the identity; the address is shown and nothing more. That
// distinction matters here more than it does for GitHub, because a Google
// address can move between accounts and a Workspace admin can reassign one —
// matching on it would hand an account to whoever inherits the mailbox.
//
// For the same reason an unverified address is not displayed: it would read as
// a claim this router had checked, and it has not.
func googleIdentity(body []byte) (oauthIdentity, error) {
	var v struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return oauthIdentity{}, fmt.Errorf("decode google userinfo: %w", err)
	}
	label := ""
	if v.EmailVerified {
		label = NormalizeEmail(v.Email)
	}
	if label == "" {
		// Something has to identify the link in the portal; the subject is
		// opaque but it is at least the thing the match is actually on.
		label = v.Sub
	}
	return oauthIdentity{ID: v.Sub, Label: label}, nil
}
