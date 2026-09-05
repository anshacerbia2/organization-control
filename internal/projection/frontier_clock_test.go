package projection

// The frontier's clock domain, asserted at the source rather than at runtime.
//
// A behavioural test cannot reach this defect. The reader used to stamp `observed_at` from the Go
// process and subtract the database's `created_at` from it, and on a machine where both clocks agree —
// every developer laptop, and every CI runner where the database is a container beside the job — that
// arithmetic produces the same answer as the correct one. The bug only appears where it cannot be
// arranged in a test: a production reader whose clock has drifted from its database's.
//
// So the property under test is structural: every instant in this file comes from the database, and
// this package holds no clock of its own. Removing `clock_timestamp()` from the statement, or
// reintroducing `time.Now`, fails here. That is the same reasoning internal/httpapi's outbound_test.go
// uses for the no-outbound-HTTP rule — a prohibition enforced where it can be seen, rather than
// written down and trusted.
//
// It is a gate on frontier.go alone. Other files in this package legitimately read the process clock.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestTheFrontierTakesItsInstantFromTheDatabase(t *testing.T) {
	if !strings.Contains(frontierStatement, "clock_timestamp()") {
		t.Error("the frontier statement no longer reads the database clock, so observed_at and the " +
			"ages it reports can no longer be on the same clock as created_at")
	}

	// One reading, reused. Called separately per fact, `clock_timestamp()` advances between calls and
	// the ages stop being consistent with the observation instant they are reported against.
	if count := strings.Count(frontierStatement, "clock_timestamp()"); count != 1 {
		t.Errorf("the statement reads the database clock %d times; one reading, held in a CTE, is "+
			"what keeps observed_at and the ages consistent with each other", count)
	}

	for _, expected := range []string{"observed.at - owed.oldest", "observed.at - debt.oldest"} {
		if !strings.Contains(frontierStatement, expected) {
			t.Errorf("no age is computed as %q; an age measured from anything but the observation "+
				"instant is measured from a second clock", expected)
		}
	}
}

func TestTheFrontierReaderHoldsNoClockOfItsOwn(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "frontier.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing frontier.go: %v", err)
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "time" {
			return true
		}
		if selector.Sel.Name == "Now" || selector.Sel.Name == "Since" {
			t.Errorf("frontier.go calls time.%s; the process clock has no business in a frontier "+
				"whose ages are measured against database timestamps", selector.Sel.Name)
		}
		return true
	})
}
