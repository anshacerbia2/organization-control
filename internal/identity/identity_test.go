package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anshacerbia2/foundation-platform/id"
)

type tokens struct {
	issued      atomic.Int32
	invalidated atomic.Int32
	err         error
}

func (t *tokens) Token(context.Context) (string, error) {
	if t.err != nil {
		return "", t.err
	}
	return "token-" + string(rune('0'+t.issued.Add(1))), nil
}

func (t *tokens) Invalidate() { t.invalidated.Add(1) }

func client(t *testing.T, server *httptest.Server, source TokenSource) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: server.URL + "/", Tokens: source, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func principal(t *testing.T) id.UUID {
	t.Helper()
	p, err := id.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The request is the contract identity-control serves: the path, the workload bearer, and the address
// in the body and nowhere else.
func TestMatchAsksTheRouteWithTheWorkloadToken(t *testing.T) {
	t.Parallel()
	who := principal(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/principals/"+who.String()+"/identifiers:match" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("a query %q: the address must not be in the URL", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Errorf("authorization %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["address"] != "Person@Example.test" || len(body) != 1 {
			t.Errorf("body %v, %v", body, err)
		}
		_, _ = io.WriteString(w, `{"match": true}`)
	}))
	defer server.Close()

	match, err := client(t, server, &tokens{}).Match(context.Background(), who, "Person@Example.test")
	if err != nil || !match {
		t.Fatalf("match %v, %v", match, err)
	}
}

func TestMatchFalseIsNo(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"match": false}`)
	}))
	defer server.Close()
	match, err := client(t, server, &tokens{}).Match(context.Background(), principal(t), "a@example.test")
	if err != nil || match {
		t.Fatalf("match %v, %v", match, err)
	}
}

// Unknown is not no: every answer that is not 200 with the boolean is errUnavailable, and none of the
// errors carries the address.
func TestEveryOtherAnswerIsUnavailable(t *testing.T) {
	t.Parallel()
	cases := map[string]func(http.ResponseWriter){
		"500":           func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"403":           func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) },
		"404":           func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
		"no field":      func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{}`) },
		"not json":      func(w http.ResponseWriter) { _, _ = io.WriteString(w, `yes`) },
		"not a boolean": func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"match": "true"}`) },
		"too large": func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"match": true, "x": "`+strings.Repeat("a", maxAnswer)+`"}`)
		},
		"redirect": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://elsewhere.test/")
			w.WriteHeader(http.StatusFound)
		},
		"401 then 401": func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) },
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { answer(w) }))
			defer server.Close()
			_, err := client(t, server, &tokens{}).Match(context.Background(), principal(t), "secret.person@example.test")
			if !errors.Is(err, errUnavailable) {
				t.Fatalf("err %v, want errUnavailable", err)
			}
			if strings.Contains(err.Error(), "secret.person") {
				t.Fatalf("the error carries the address: %v", err)
			}
		})
	}
}

func TestAnUnreachableAPIIsUnavailable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	c, err := New(Config{BaseURL: url, Tokens: &tokens{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Match(context.Background(), principal(t), "a@example.test"); !errors.Is(err, errUnavailable) {
		t.Fatalf("err %v", err)
	}
}

func TestNoTokenIsUnavailable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent without a token")
	}))
	defer server.Close()
	_, err := client(t, server, &tokens{err: errors.New("token endpoint 400")}).Match(context.Background(), principal(t), "a@example.test")
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("err %v", err)
	}
}

// A refused token is dropped and the question asked once more with a fresh one.
func TestA401RenewsTheTokenOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-2" {
			t.Errorf("the retry carried %q", got)
		}
		_, _ = io.WriteString(w, `{"match": true}`)
	}))
	defer server.Close()
	source := &tokens{}
	match, err := client(t, server, source).Match(context.Background(), principal(t), "a@example.test")
	if err != nil || !match || source.invalidated.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("match %v, err %v, invalidated %d, calls %d", match, err, source.invalidated.Load(), calls.Load())
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"relative":  {BaseURL: "/identity", Tokens: &tokens{}, Timeout: time.Second},
		"ftp":       {BaseURL: "ftp://identity", Tokens: &tokens{}, Timeout: time.Second},
		"query":     {BaseURL: "http://identity?x=1", Tokens: &tokens{}, Timeout: time.Second},
		"no tokens": {BaseURL: "http://identity", Timeout: time.Second},
		"timeout":   {BaseURL: "http://identity", Tokens: &tokens{}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatchRefusesAnEmptyQuestion(t *testing.T) {
	t.Parallel()
	c, err := New(Config{BaseURL: "http://identity.test", Tokens: &tokens{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Match(context.Background(), id.UUID{}, "a@example.test"); err == nil {
		t.Error("a nil principal was asked")
	}
	if _, err := c.Match(context.Background(), principal(t), " "); err == nil {
		t.Error("a blank address was asked")
	}
}
