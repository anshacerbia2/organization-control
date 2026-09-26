// Command grantcheck derives which runtime role runs which SQL statement, and has PostgreSQL judge
// whether the grants match. See TDD-organization-control-001 §Grant derivation.
//
//	go run ./tools/grantcheck -dsn "$TEST_DATABASE_URL"
//
// The DSN must be a disposable, fully migrated database whose owner can revoke: the CI database.
// Without a DSN only the derivation runs, which is useful locally and proves nothing about grants.
//
// Findings, each of which fails the run:
//
//	PROBLEM  the derivation cannot attribute a statement to a role
//	MISSING  a derived statement cannot run under the role that runs it
//	UNUSED   a grant no derived statement needs, and which the baseline does not list
//	STALE    a baseline entry that is no longer an unused grant
//
// The baseline (unused-baseline.txt) lists grants known to be unused when this tool arrived. It is
// debt written down, not an allowance: an entry can only be removed, by revoking the grant or by
// code starting to need it, and the run fails until the file says which. -write-baseline rewrites
// it from the current database.
//
// Exit status: 0 clean, 1 findings, 2 the tool could not run.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	fdb "github.com/anshacerbia2/foundation-platform/db"
)

const baselinePath = "tools/grantcheck/unused-baseline.txt"

func main() { os.Exit(run()) }

func run() int {
	dsn := flag.String("dsn", os.Getenv("TEST_DATABASE_URL"), "owner DSN of a disposable, migrated database")
	matrix := flag.Bool("matrix", false, "print the derived matrix, with the relations each statement touches")
	writeBaseline := flag.Bool("write-baseline", false, "rewrite "+baselinePath+" from the current unused grants")
	flag.Parse()

	d, err := derive(".", "./cmd/organization-control")
	if err != nil {
		fmt.Fprintln(os.Stderr, "grantcheck:", err)
		return 2
	}
	for _, role := range Roles {
		fmt.Printf("derived  %-28s %d statements\n", role, len(d.ByRole[role]))
	}
	findings := len(d.Problems)
	for _, p := range d.Problems {
		fmt.Println("PROBLEM  " + p)
	}
	if *dsn == "" {
		fmt.Println("no -dsn and no TEST_DATABASE_URL: derivation only, grants not checked")
		return exit(findings)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := fdb.Open(ctx, fdb.Config{Name: "grantcheck", DSN: *dsn, MaxConns: 2})
	if err != nil {
		fmt.Fprintln(os.Stderr, "grantcheck:", err)
		return 2
	}
	defer pool.Close()

	// The unused check revokes grants inside transactions that are rolled back. That is safe, but it
	// takes locks, so it runs only against a database named as a test database.
	var database string
	if err := pool.InTx(ctx, func(ctx context.Context, tx fdb.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_database()`).Scan(&database)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "grantcheck:", err)
		return 2
	}
	if !strings.HasSuffix(database, "_test") {
		fmt.Fprintf(os.Stderr, "grantcheck: refusing to run against %q; point -dsn at a disposable "+
			"database whose name ends in _test, such as the one make ci-db builds\n", database)
		return 2
	}

	verdict, err := verify(ctx, pool, d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grantcheck:", err)
		return 2
	}

	if *matrix {
		printMatrix(verdict)
	}
	for _, p := range verdict.Planned {
		if p.Refused != "" {
			findings++
			fmt.Printf("MISSING  %s cannot run %s: %s\n  reached in %v\n  %s\n",
				p.Role, name(p.Statement), p.Refused, p.Statement.Sites, preview(p.Statement.SQL))
		}
	}

	unused := map[string]bool{}
	for _, g := range verdict.Unused {
		unused[g.String()] = true
	}
	if *writeBaseline {
		if err := saveBaseline(unused); err != nil {
			fmt.Fprintln(os.Stderr, "grantcheck:", err)
			return 2
		}
		fmt.Printf("wrote    %s with %d entries\n", baselinePath, len(unused))
		return exit(findings)
	}
	baseline, err := loadBaseline()
	if err != nil {
		fmt.Fprintln(os.Stderr, "grantcheck:", err)
		return 2
	}
	for _, g := range verdict.Unused {
		if !baseline[g.String()] {
			findings++
			fmt.Printf("UNUSED   %s: no statement this repository runs as %s needs it; revoke it, or "+
				"say in grants.sql which path needs it\n", g, g.Role)
		}
	}
	var stale []string
	for entry := range baseline {
		if !unused[entry] {
			stale = append(stale, entry)
		}
	}
	sort.Strings(stale)
	for _, entry := range stale {
		findings++
		fmt.Printf("STALE    %s is in %s but is no longer an unused grant; remove the line\n", entry, baselinePath)
	}

	fmt.Printf("checked  %d statements; %d unused grants, %d of them in the baseline\n",
		len(verdict.Planned), len(verdict.Unused), len(verdict.Unused)-countNew(verdict.Unused, baseline))
	return exit(findings)
}

func countNew(unused []Grant, baseline map[string]bool) int {
	n := 0
	for _, g := range unused {
		if !baseline[g.String()] {
			n++
		}
	}
	return n
}

func loadBaseline() (map[string]bool, error) {
	f, err := os.Open(baselinePath)
	if err != nil {
		return nil, fmt.Errorf("reading the baseline: %w", err)
	}
	defer f.Close()
	out := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out, scanner.Err()
}

func saveBaseline(unused map[string]bool) error {
	var lines []string
	for entry := range unused {
		lines = append(lines, entry)
	}
	sort.Strings(lines)
	header := "# Grants no statement in this repository needs, recorded when grantcheck arrived.\n" +
		"# Debt, not an allowance: remove a line by revoking the grant in internal/controldb/grants.sql,\n" +
		"# or because code now needs it. grantcheck fails on any unused grant missing from this file and\n" +
		"# on any line here that is no longer an unused grant. Rewrite with: go run ./tools/grantcheck -write-baseline\n"
	return os.WriteFile(baselinePath, []byte(header+strings.Join(lines, "\n")+"\n"), 0o644)
}

func exit(findings int) int {
	if findings > 0 {
		return 1
	}
	return 0
}

func name(s *Statement) string {
	if s.Name != "" {
		return s.Name
	}
	return "an unnamed statement"
}

func printMatrix(v *Verdict) {
	for _, p := range v.Planned {
		var relations []string
		for r := range p.Relations {
			relations = append(relations, r.String())
		}
		sort.Strings(relations)
		fmt.Printf("matrix   %-28s %-50s %v\n", p.Role, name(p.Statement), relations)
	}
}
