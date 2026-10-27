package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDC runs the Authorization Code + PKCE flow against TT_OIDC_ISSUER (ADR-016). Discovery is lazy and retried
// until it succeeds once, so the api starts even when the identity provider is briefly unreachable.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// HTTPClient is used for discovery, JWKS and the token exchange (nil = http.DefaultClient).
	HTTPClient *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

// Claims are the identity of a verified ID token.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

func (o *OIDC) ctx(ctx context.Context) context.Context {
	if o.HTTPClient != nil {
		return oidc.ClientContext(ctx, o.HTTPClient)
	}
	return ctx
}

func (o *OIDC) init(ctx context.Context) (*oidc.Provider, *oauth2.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		p, err := oidc.NewProvider(o.ctx(ctx), o.Issuer)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery: %w", err)
		}
		o.provider = p
	}
	scopes := o.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	return o.provider, &oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL,
		Endpoint: o.provider.Endpoint(), Scopes: scopes}, nil
}

// AuthURL is the provider's authorization URL for a login (S256 PKCE challenge of verifier, nonce bound to the ID token).
func (o *OIDC) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	_, cfg, err := o.init(ctx)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), nil
}

// ErrInvalidToken is returned when the ID token is missing, unverifiable or carries the wrong nonce.
var ErrInvalidToken = errors.New("invalid id token")

// Exchange redeems the code (with the PKCE verifier) and verifies the ID token (signature, issuer, audience, expiry, nonce).
func (o *OIDC) Exchange(ctx context.Context, code, verifier, nonce string) (Claims, error) {
	p, cfg, err := o.init(ctx)
	if err != nil {
		return Claims{}, err
	}
	tok, err := cfg.Exchange(o.ctx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Claims{}, fmt.Errorf("oidc token exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Claims{}, fmt.Errorf("%w: no id_token in the token response", ErrInvalidToken)
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.ClientID}).Verify(o.ctx(ctx), raw)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if idt.Nonce != nonce {
		return Claims{}, fmt.Errorf("%w: nonce mismatch", ErrInvalidToken)
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
		PreferredUser string `json:"preferred_username"`
	}
	if err := idt.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	out := Claims{Subject: idt.Subject, Email: c.Email, Name: c.Name}
	// Providers that omit email_verified (e.g. some enterprise IdPs) only issue verified addresses.
	out.EmailVerified = c.EmailVerified == nil || *c.EmailVerified
	if out.Name == "" {
		out.Name = c.PreferredUser
	}
	if out.Email == "" {
		return Claims{}, fmt.Errorf("%w: no email claim (request the email scope)", ErrInvalidToken)
	}
	return out, nil
}
