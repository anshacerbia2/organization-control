package httpapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// outboundForms are the ways this repository could acquire an outbound HTTP client.
//
// ADR-ORG-001 §5.4 gives this service no network route to the identity kernel, and `arch.json` used
// to enforce that by denying `net/http` outright. Serving requires the import, so the denial now
// carries two exceptions — and an import-level rule cannot tell a server from a client. This walk is
// what the exception was traded for.
//
// The list names constructions rather than a general shape because the general shape is not
// decidable: any of these is an outbound call, and code that reaches the network without touching
// one of them would have to do so through a transport this repository also does not import.
var outboundForms = []struct {
	selector string
	why      string
}{
	{"http.Client", "constructs an outbound client"},
	{"http.DefaultClient", "uses the package-level outbound client"},
	{"http.Transport", "constructs an outbound transport"},
	{"http.DefaultTransport", "uses the package-level outbound transport"},
	{"http.Get", "issues an outbound request"},
	{"http.Post", "issues an outbound request"},
	{"http.PostForm", "issues an outbound request"},
	{"http.Head", "issues an outbound request"},
	{"http.NewRequest", "builds an outbound request"},
	{"http.NewRequestWithContext", "builds an outbound request"},
	{"http.ProxyFromEnvironment", "configures outbound proxying"},
}

// TestNoOutboundHTTPClient walks the two packages excepted from the net/http denial.
//
// Test files are walked too, with one deliberate carve-out: `httptest.NewRequest` builds an inbound
// request for a handler and reaches no network. Excluding all test files instead would leave the
// easiest place to introduce a client unwatched.
func TestNoOutboundHTTPClient(t *testing.T) {
	t.Parallel()

	roots := []string{".", filepath.Join("..", "..", "cmd", "organization-control")}

	scanned := 0
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("resolve %s: %v", root, err)
		}

		walkErr := filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				// The composition root may not exist yet in a working tree mid-change. A missing
				// directory is skipped rather than fatal, and the count assertion below is what
				// catches a walk that skipped everything.
				if entry == nil {
					return nil
				}
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}

			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			scanned++

			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				qualified := pkg.Name + "." + selector.Sel.Name

				for _, form := range outboundForms {
					if qualified != form.selector {
						continue
					}
					position := fileSet.Position(selector.Pos())
					t.Errorf("%s:%d uses %s, which %s. ADR-ORG-001 §5.4 gives this service no "+
						"network route to another domain, and the net/http exception in arch.json "+
						"was granted for serving only",
						filepath.Base(path), position.Line, qualified, form.why)
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", root, walkErr)
		}
	}

	// A walk that parsed nothing would pass while checking nothing — the exact failure this file
	// exists to prevent, and the one that let a governance rule in the estate report success for
	// months without ever running.
	if scanned < 5 {
		t.Fatalf("the walk parsed only %d files, so it is not covering the excepted packages", scanned)
	}
}

// TestTheIdentityClientIsTheOnlyCaller holds internal/identity, the one package arch.json excepts to
// call out (ADR-ORG-007 §5.3), to one client construction and one request, built in its non-test
// files, to the address match path. Its tests serve with httptest and are not walked: a test server
// is inbound.
func TestTheIdentityClientIsTheOnlyCaller(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", "identity"))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	matchPath := false
	scanned := 0
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		scanned++
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CompositeLit:
				// http.Client is counted where one is constructed, not where the type is named.
				if selector, ok := n.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "http" && selector.Sel.Name == "Client" {
						counts["http.Client"]++
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name+"."+n.Sel.Name != "http.Client" {
					counts[pkg.Name+"."+n.Sel.Name]++
				}
			case *ast.BasicLit:
				if n.Value == `"identifiers:match"` {
					matchPath = true
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if scanned == 0 {
		t.Fatal("internal/identity has no source file to check")
	}
	allowed := map[string]int{"http.Client": 1, "http.NewRequestWithContext": 1}
	for _, form := range outboundForms {
		if got, want := counts[form.selector], allowed[form.selector]; got != want {
			t.Errorf("internal/identity uses %s %d times, want %d: it %s", form.selector, got, want, form.why)
		}
	}
	if !matchPath {
		t.Error("internal/identity does not name the identifiers:match path it is excepted for")
	}
}
