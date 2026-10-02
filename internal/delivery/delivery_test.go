package delivery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu           sync.Mutex
	checks       map[string]int
	runs         map[string]int
	registeredAt int // the check on which a consumer reads as registered
	checkErr     error
	runErr       error
}

func (r *recorder) registered(_ context.Context, consumer string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[consumer]++
	if r.checkErr != nil {
		return false, r.checkErr
	}
	return r.checks[consumer] >= r.registeredAt, nil
}

func (r *recorder) run(ctx context.Context, target Target) error {
	r.mu.Lock()
	r.runs[target.Consumer]++
	err := r.runErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *recorder) count(m map[string]int, consumer string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return m[consumer]
}

func config(r *recorder, targets ...Target) Config {
	return Config{
		Targets: targets, Timeout: time.Second, RetryInterval: 5 * time.Millisecond,
		Logger:     slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		registered: r.registered, run: r.run,
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A consumer registered after this service started is delivered to once it is: the dispatcher waits
// for the registration rather than failing the process.
func TestADispatcherWaitsForItsConsumersRegistration(t *testing.T) {
	r := &recorder{checks: map[string]int{}, runs: map[string]int{}, registeredAt: 3}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, config(r, Target{Consumer: "identity-control", Endpoint: "https://ic/v1/deliveries"}))
	}()

	eventually(t, "the dispatcher to start", func() bool { return r.count(r.runs, "identity-control") == 1 })
	if checks := r.count(r.checks, "identity-control"); checks != 3 {
		t.Errorf("the dispatcher started after %d checks, want 3", checks)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

// A dispatcher that stops with an error is started again, and one failing target does not stop
// another.
func TestAStoppedDispatcherIsStartedAgainAndOthersKeepRunning(t *testing.T) {
	r := &recorder{checks: map[string]int{}, runs: map[string]int{}, registeredAt: 1, runErr: errors.New("prerequisites unmet")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Run(ctx, config(r, Target{Consumer: "a", Endpoint: "https://a"}, Target{Consumer: "b", Endpoint: "https://b"}))
	}()
	eventually(t, "both dispatchers to be restarted", func() bool {
		return r.count(r.runs, "a") >= 3 && r.count(r.runs, "b") >= 3
	})
}

// A registration that cannot be read is not read as unregistered or as registered: the dispatcher
// waits and checks again, and never starts on a guess.
func TestAFailedRegistrationCheckStartsNothing(t *testing.T) {
	r := &recorder{checks: map[string]int{}, runs: map[string]int{}, registeredAt: 1, checkErr: errors.New("connection refused")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Run(ctx, config(r, Target{Consumer: "a", Endpoint: "https://a"})) }()
	eventually(t, "repeated checks", func() bool { return r.count(r.checks, "a") >= 3 })
	if runs := r.count(r.runs, "a"); runs != 0 {
		t.Errorf("a dispatcher started %d times on a failed check", runs)
	}
}

func TestRunRefusesAnIncompleteConfiguration(t *testing.T) {
	r := &recorder{checks: map[string]int{}, runs: map[string]int{}}
	for name, mutate := range map[string]func(*Config){
		"no logger":          func(c *Config) { c.Logger = nil },
		"no retry interval":  func(c *Config) { c.RetryInterval = 0 },
		"no timeout":         func(c *Config) { c.Timeout = 0 },
		"a nameless target":  func(c *Config) { c.Targets = []Target{{Endpoint: "https://a"}} },
		"a duplicate target": func(c *Config) { c.Targets = []Target{{"a", "https://a"}, {"a", "https://b"}} },
		"no pool":            func(c *Config) { c.registered, c.run = nil, nil },
	} {
		cfg := config(r, Target{Consumer: "a", Endpoint: "https://a"})
		mutate(&cfg)
		if err := Run(context.Background(), cfg); err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}
