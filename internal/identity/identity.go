// Package identity asks the Identity Control API one question: does a Principal hold an address as
// a proven address (ADR-ORG-007 §5.3, TDD-organization-control-004 §Acceptance Is the Whole Join).
//
// It is the one package of this repository that calls out over net/http, and arch.json excepts it
// for that alone. foundation-platform has no client for this route, so the client is built here, once:
// one *http.Client with a timeout, one request form, one path. internal/httpapi's outbound walk holds
// every other package to constructing none, and holds this one to its single request.
//
// The answer is a boolean and nothing else. The address leaves this service in the request body and
// is never logged or placed in an error, so a failed match cannot leak it through a log line.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

// TokenSource is this service's workload credential. clientauth.Tokens is the production one, the
// same source the delivery dispatchers present (TDD-organization-control-004 §Configuration).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// errUnavailable is every failure to get an answer: the API unreachable, a token not issued, a status
// other than 200, or a body that is not the answer. Acceptance treats all of them as unknown, which
// is not no (ADR-ORG-007 §5.3).
var errUnavailable = errors.New("identity: the Identity Control API did not answer the match")

// Config is one Identity Control API.
type Config struct {
	// BaseURL is the API's absolute http(s) URL, without the /v1 path.
	BaseURL string

	Tokens TokenSource

	// Timeout bounds one request, connection and body included. "The timeout includes connection
	// time, any redirects, and reading the response body" (net/http, Client.Timeout).
	Timeout time.Duration
}

// Client asks the match question.
type Client struct {
	base   *url.URL
	tokens TokenSource
	http   *http.Client
}

// New builds the client. "Clients and Transports are safe for concurrent use by multiple goroutines
// and for efficiency should only be created once and re-used" (net/http), so the composition root
// builds one and every acceptance shares it.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	switch {
	case err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https"):
		return nil, errors.New("identity: the base URL must be an absolute http(s) URL")
	case base.RawQuery != "" || base.Fragment != "":
		return nil, errors.New("identity: the base URL carries no query or fragment")
	case cfg.Tokens == nil:
		return nil, errors.New("identity: the workload token source is required")
	case cfg.Timeout <= 0:
		return nil, errors.New("identity: the request timeout must be positive")
	}
	return &Client{base: base, tokens: cfg.Tokens, http: &http.Client{
		Timeout: cfg.Timeout,
		// A redirect would carry the workload token to wherever the response pointed. The route
		// never redirects, so one is refused rather than followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// maxAnswer bounds the response read: the answer is {"match": true}, and a larger body is not it.
const maxAnswer = 4 << 10

type matchRequest struct {
	Address string `json:"address"`
}

type matchAnswer struct {
	Match *bool `json:"match"`
}

// Match reports whether the Principal holds the address as a proven address: an `active` address
// with `verified_at` set, of an active Principal, compared without case (ADR-ORG-007 §5.3).
//
// A 401 drops the cached token and asks once more with a fresh one: a token revoked or rotated
// before its expiry is refused, and the next one is not. Anything else that is not 200 with the
// answer is errUnavailable, and so is a second 401.
func (c *Client) Match(ctx context.Context, principal id.UUID, address string) (bool, error) {
	if principal.IsNil() || strings.TrimSpace(address) == "" {
		return false, errors.New("identity: a principal and an address are required")
	}
	body, err := json.Marshal(matchRequest{Address: address})
	if err != nil {
		return false, fmt.Errorf("identity: encode the match: %w", err)
	}
	// A UUID and two literals, so nothing in the path comes from the request. `identifiers:match` is
	// the custom-method form identity-control serves (`/v1/principals/{principal_id}/identifiers:match`).
	endpoint := c.base.JoinPath("v1", "principals", principal.String(), "identifiers:match")

	for attempt := 0; attempt < 2; attempt++ {
		match, status, err := c.ask(ctx, endpoint.String(), body)
		if err != nil {
			return false, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			c.tokens.Invalidate()
			continue
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("%w: status %d", errUnavailable, status)
		}
		return match, nil
	}
	return false, fmt.Errorf("%w: the workload token was refused twice", errUnavailable)
}

// ask sends one request. It returns the answer only with status 200; the status otherwise.
func (c *Client) ask(ctx context.Context, endpoint string, body []byte) (bool, int, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		// The token source's error never carries the token; it may carry the endpoint's status.
		return false, 0, fmt.Errorf("%w: no workload token: %w", errUnavailable, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, 0, fmt.Errorf("%w: build the request: %w", errUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		// url.Error names the method and the URL, which hold a principal_id and no address.
		return false, 0, fmt.Errorf("%w: %w", errUnavailable, err)
	}
	// "The caller must close the response body when finished with it" (net/http).
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer+1))
	if err != nil {
		return false, 0, fmt.Errorf("%w: read the answer: %w", errUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		return false, response.StatusCode, nil
	}
	if len(raw) > maxAnswer {
		return false, 0, fmt.Errorf("%w: the answer is larger than %d bytes", errUnavailable, maxAnswer)
	}
	var answer matchAnswer
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Match == nil {
		return false, 0, fmt.Errorf("%w: the answer is not {\"match\": boolean}", errUnavailable)
	}
	return *answer.Match, http.StatusOK, nil
}
