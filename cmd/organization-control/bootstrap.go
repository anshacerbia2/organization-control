package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
	"github.com/anshacerbia2/foundation-platform/id"

	"github.com/anshacerbia2/organization-control/internal/authority"
)

// bootstrapCommand makes the first provider grant (ADR-ORG-001 §5.11, TDD-organization-control-001
// §Caller Authority).
const bootstrapCommand = "bootstrap-provider"

// bootstrapProvider runs the bootstrap: one transaction on the provider connections, refused when
// any grant exists, recording the operator and the reason in a row no runtime role can change.
//
// It reads ORGANIZATION_PROVIDER_DATABASE_URL and nothing else from the environment. The rest of the
// service's configuration is irrelevant to one INSERT, and requiring it would make an operator
// assemble a full deployment environment to run a command that touches one table.
func bootstrapProvider(args []string) error {
	flags := flag.NewFlagSet(bootstrapCommand, flag.ContinueOnError)
	principal := flags.String("principal-id", "", "the Principal to grant, minted by the Identity Control API's ceremony")
	operator := flags.String("operator", "", "your name, recorded as the operator who ran the bootstrap")
	reason := flags.String("reason", "", "why, recorded with the grant")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	principalID, err := id.Parse(strings.TrimSpace(*principal))
	if err != nil {
		return errors.New("-principal-id must be the Principal's principal_id, a UUID")
	}
	dsn := strings.TrimSpace(os.Getenv("ORGANIZATION_PROVIDER_DATABASE_URL"))
	if dsn == "" {
		return errors.New("ORGANIZATION_PROVIDER_DATABASE_URL is required: the grant is written as the provider role")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	conns, err := fdb.Open(ctx, fdb.Config{Name: "organization-control-bootstrap", DSN: dsn, MaxConns: 1})
	if err != nil {
		return fmt.Errorf("provider connection: %w", err)
	}
	defer conns.Close()
	grants, err := authority.NewGrants(conns)
	if err != nil {
		return err
	}
	grant, err := grants.Bootstrap(ctx, authority.Bootstrap{
		Principal: principalID, Operator: *operator, Reason: *reason,
	})
	if err != nil {
		return err
	}

	if grant.Existing {
		fmt.Printf("already granted: %s holds %s since %s (grant %s, bootstrapped by %s); nothing written\n",
			grant.Principal, grant.Scope, grant.GrantedAt.UTC().Format(time.RFC3339), grant.ID, grant.Operator)
		return nil
	}
	fmt.Printf("granted: %s holds %s (grant %s, bootstrapped by %s)\n",
		grant.Principal, grant.Scope, grant.ID, grant.Operator)
	return nil
}
