package main

// Derivation: which role runs which statement, read from the code rather than from a reader.
//
// A statement runs under the role of the connection its transaction was opened on, and in this
// repository that is decided in exactly two kinds of place:
//
//   - a scope wrapper in internal/db (WithTenantScope, WithProviderScope, WithProviderSnapshot,
//     WithResolutionScope), whose pool type fixes the role, and whose body is a function value;
//   - a raw transaction on a Transactor held by a struct, whose role is decided by what the
//     composition root handed it. These are the declared boundaries below, and the only declared
//     input to this tool.
//
// From each wrapper call site the tool walks everything the body can reach -- through static
// calls, interface calls and function values, using a VTA call graph -- and collects every string
// constant that is a SQL statement. From each boundary it does the same with the boundary's role.
//
// It refuses rather than guesses. A SQL call whose statement is not a constant, a SQL constant no
// root reaches, a raw transaction outside a wrapper or a declared boundary, and a wrapper body it
// cannot resolve are all problems, because each is a place where the matrix would otherwise be
// silently incomplete.

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

const (
	module   = "github.com/anshacerbia2/organization-control"
	platform = "github.com/anshacerbia2/foundation-platform"
	dbPkg    = module + "/internal/db"

	tenantRole     = "organization_rt"
	providerRole   = "organization_provider_rt"
	resolutionRole = "organization_resolution_rt"
)

// Roles are the runtime roles whose privileges this tool derives. organization_dispatch_rt is
// absent on purpose: the dispatcher's statements live in foundation-platform and run in
// foundation-reference, so no code path in this repository exercises that role.
var Roles = []string{tenantRole, providerRole, resolutionRole}

// wrappers are the scope entry points, by SSA function name, and the role their pool connects as.
var wrappers = map[string]string{
	dbPkg + ".WithTenantScope":      tenantRole,
	dbPkg + ".WithProviderScope":    providerRole,
	dbPkg + ".WithProviderSnapshot": providerRole,
	dbPkg + ".WithResolutionScope":  resolutionRole,
}

// wrapperInternals are the unexported helpers the wrappers share. They forward the body as a
// parameter, so their own wrapper calls carry no resolvable body and are not call sites.
var wrapperInternals = map[string]bool{
	dbPkg + ".withProviderScope": true,
	dbPkg + ".withRecordedScope": true,
}

// boundaries own a raw transaction on a Transactor the composition root chose. The role is what
// cmd/organization-control/main.go hands each one; a change there must change this table, and
// TDD-organization-control-001 §Grant derivation says so.
var boundaries = map[string]string{
	// access.New(providerConns)
	"(*" + module + "/internal/access.Recorder).RecordProviderAccess": providerRole,
	// db.NewClaimStore(tenantConns)
	"(*" + dbPkg + ".ClaimStore).Complete": tenantRole,
	// projection.NewFrontierReader(providerConns)
	"(*" + module + "/internal/projection.FrontierReader).FrontierFor": providerRole,
}

// Statement is one SQL constant and where it was reached.
type Statement struct {
	SQL   string
	Name  string   // the declaring constant, when there is one
	Sites []string // functions it was reached in
}

// Derivation is the matrix: role -> SQL -> statement.
type Derivation struct {
	ByRole   map[string]map[string]*Statement
	Problems []string
}

func (d *Derivation) problem(format string, args ...any) {
	d.Problems = append(d.Problems, fmt.Sprintf(format, args...))
}

func derive(dir string, patterns ...string) (*Derivation, error) {
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: dir, Tests: false}
	initial, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	if n := packages.PrintErrors(initial); n > 0 {
		return nil, fmt.Errorf("load: %d package errors", n)
	}

	prog, _ := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	all := ssautil.AllFunctions(prog)
	graph := vta.CallGraph(all, cha.CallGraph(prog))

	d := &Derivation{ByRole: map[string]map[string]*Statement{}}
	for _, role := range Roles {
		d.ByRole[role] = map[string]*Statement{}
	}
	names := constantNames(initial)

	byName := map[string]*ssa.Function{}
	for fn := range all {
		byName[fn.String()] = fn
	}
	for name := range wrappers {
		if byName[name] == nil {
			d.problem("wrapper %s is declared but does not exist; this tool's table is stale", name)
		}
	}
	for name := range boundaries {
		if byName[name] == nil {
			d.problem("boundary %s is declared but does not exist; this tool's table is stale", name)
		}
	}

	// Every wrapper call site outside the wrappers themselves, and the body it runs.
	type root struct {
		role string
		fn   *ssa.Function
	}
	var roots []root
	bodies := map[*ssa.Function]bool{}
	for fn := range all {
		if !ours(fn) || wrapperInternals[fn.String()] || wrappers[fn.String()] != "" {
			continue
		}
		forEachCall(fn, func(call ssa.CallInstruction) {
			callee := call.Common().StaticCallee()
			if callee == nil {
				return
			}
			role, ok := wrappers[callee.String()]
			if !ok {
				return
			}
			args := call.Common().Args
			body := functionValue(args[len(args)-1])
			if body == nil {
				d.problem("%s: the body passed to %s is not a function this tool can resolve",
					position(prog.Fset, call.Pos()), callee.Name())
				return
			}
			bodies[body] = true
			roots = append(roots, root{role, body})
		})
	}
	for name, role := range wrappers {
		if fn := byName[name]; fn != nil {
			roots = append(roots, root{role, fn})
		}
	}
	for name, role := range boundaries {
		if fn := byName[name]; fn != nil {
			roots = append(roots, root{role, fn})
		}
	}

	reached := map[*ssa.Function]map[string]bool{} // function -> roles it was reached under
	for _, r := range roots {
		walk(prog.Fset, graph, r.role, r.fn, bodies, reached, d, names)
	}

	// SQL no root reaches, and raw transactions nobody declared.
	for fn := range all {
		if !ownedByModule(fn) {
			continue
		}
		if len(reached[fn]) == 0 {
			for _, sql := range sqlConstants(fn) {
				d.problem("%s: SQL reached from no scope wrapper or declared boundary, so no role is "+
					"known for it: %s", fn, preview(sql))
			}
		}
		if fn.Pkg != nil && fn.Pkg.Pkg.Path() == dbPkg && (wrappers[fn.String()] != "" || wrapperInternals[fn.String()]) {
			continue
		}
		if _, declared := boundaries[fn.String()]; declared {
			continue
		}
		if containsInTx(fn) && !insideDeclared(fn) {
			d.problem("%s opens a raw transaction outside a scope wrapper and is not a declared boundary; "+
				"its statements run under whatever role the composition root gave it", fn)
		}
	}

	d.Problems = unique(d.Problems)
	return d, nil
}

// walk collects every SQL constant reachable from fn under role.
func walk(fset *token.FileSet, graph *callgraph.Graph, role string, start *ssa.Function,
	bodies map[*ssa.Function]bool, reached map[*ssa.Function]map[string]bool, d *Derivation,
	names map[string]string) {

	queue := []*ssa.Function{start}
	seen := map[*ssa.Function]bool{}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if seen[fn] || !ours(fn) {
			continue
		}
		seen[fn] = true
		if reached[fn] == nil {
			reached[fn] = map[string]bool{}
		}
		reached[fn][role] = true

		for _, sql := range sqlConstants(fn) {
			statement := d.ByRole[role][sql]
			if statement == nil {
				statement = &Statement{SQL: sql, Name: names[sql]}
				d.ByRole[role][sql] = statement
			}
			statement.Sites = appendUnique(statement.Sites, fn.String())
		}

		node := graph.Nodes[fn]
		forEachCall(fn, func(call ssa.CallInstruction) {
			common := call.Common()
			if callee := common.StaticCallee(); callee != nil {
				if _, ok := wrappers[callee.String()]; ok {
					return // its own root, under its own role
				}
				if _, ok := boundaries[callee.String()]; ok {
					return // its own root, on its own connection
				}
			}
			if isSQLCall(common) {
				if !readableStatement(graph, common.Args[1], map[ssa.Value]bool{}) {
					d.problem("%s: a SQL call whose statement is not a constant; this tool cannot "+
						"derive what it touches", position(fset, call.Pos()))
				}
			}
			// Function values handed to a callee run on the caller's transaction, whether the
			// callee is InTx, a helper, or code outside this module.
			for _, arg := range common.Args {
				if body := functionValue(arg); body != nil && !bodies[body] {
					queue = append(queue, body)
				}
			}
			if isInTx(common) {
				return // entered through its function argument above, never through InTx itself
			}
			if node == nil {
				return
			}
			for _, edge := range node.Out {
				if edge.Site != call || edge.Callee.Func == nil {
					continue
				}
				if callee := edge.Callee.Func; !bodies[callee] {
					queue = append(queue, callee)
				}
			}
		})
	}
}

// ours limits the walk to code whose statements run on this repository's connections.
// foundation-platform is included: outbox.Append and the idempotency store execute SQL inside the
// caller's transaction.
func ours(fn *ssa.Function) bool {
	pkg := packageOf(fn)
	return strings.HasPrefix(pkg, module) || strings.HasPrefix(pkg, platform)
}

func ownedByModule(fn *ssa.Function) bool {
	return strings.HasPrefix(packageOf(fn), module)
}

func packageOf(fn *ssa.Function) string {
	for f := fn; f != nil; f = f.Parent() {
		if f.Pkg != nil {
			return f.Pkg.Pkg.Path()
		}
		if f.Origin() != nil && f.Origin().Pkg != nil {
			return f.Origin().Pkg.Pkg.Path()
		}
	}
	return ""
}

func insideDeclared(fn *ssa.Function) bool {
	for f := fn.Parent(); f != nil; f = f.Parent() {
		if _, ok := boundaries[f.String()]; ok {
			return true
		}
		if wrappers[f.String()] != "" || wrapperInternals[f.String()] {
			return true
		}
	}
	return false
}

func forEachCall(fn *ssa.Function, visit func(ssa.CallInstruction)) {
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			if call, ok := instr.(ssa.CallInstruction); ok {
				visit(call)
			}
		}
	}
}

func functionValue(v ssa.Value) *ssa.Function {
	switch value := v.(type) {
	case *ssa.MakeClosure:
		if fn, ok := value.Fn.(*ssa.Function); ok {
			return fn
		}
	case *ssa.Function:
		return value
	case *ssa.ChangeType:
		return functionValue(value.X)
	case *ssa.MakeInterface:
		return functionValue(value.X)
	}
	return nil
}

// isSQLCall matches Exec, Query and QueryRow on the driver's transaction interface, which is what
// foundation-platform's db.Tx is an alias of.
func isSQLCall(common *ssa.CallCommon) bool {
	var method *types.Func
	if common.IsInvoke() {
		method = common.Method
	} else if callee := common.StaticCallee(); callee != nil {
		method, _ = callee.Object().(*types.Func)
	}
	if method == nil || method.Pkg() == nil {
		return false
	}
	switch method.Name() {
	case "Exec", "Query", "QueryRow":
	default:
		return false
	}
	return strings.HasPrefix(method.Pkg().Path(), "github.com/jackc/pgx") && len(common.Args) >= 2
}

func isInTx(common *ssa.CallCommon) bool {
	if common.IsInvoke() {
		return common.Method.Name() == "InTx"
	}
	callee := common.StaticCallee()
	return callee != nil && callee.Name() == "InTx"
}

func containsInTx(fn *ssa.Function) bool {
	found := false
	forEachCall(fn, func(call ssa.CallInstruction) {
		if isInTx(call.Common()) {
			found = true
		}
	})
	return found
}

// readableStatement accepts a statement whose every possible value is a constant: a constant, a
// choice between constants, a parameter every caller fills with one, or the result of a function
// whose every return is one. The constants themselves are collected where they appear, so
// accepting the value here loses nothing from the matrix.
func readableStatement(graph *callgraph.Graph, v ssa.Value, visiting map[ssa.Value]bool) bool {
	if visiting[v] {
		return true
	}
	visiting[v] = true
	switch value := v.(type) {
	case *ssa.Const:
		return true
	case *ssa.Phi:
		for _, edge := range value.Edges {
			if !readableStatement(graph, edge, visiting) {
				return false
			}
		}
		return true
	case *ssa.Parameter:
		fn := value.Parent()
		index := -1
		for i, p := range fn.Params {
			if p == value {
				index = i
			}
		}
		node := graph.Nodes[fn]
		if index < 0 || node == nil || len(node.In) == 0 {
			return false
		}
		for _, edge := range node.In {
			common := edge.Site.Common()
			args := common.Args
			if common.IsInvoke() {
				return false
			}
			if index >= len(args) || !readableStatement(graph, args[index], visiting) {
				return false
			}
		}
		return true
	case *ssa.Call:
		callee := value.Common().StaticCallee()
		if callee == nil || len(callee.Blocks) == 0 {
			return false
		}
		for _, block := range callee.Blocks {
			for _, instr := range block.Instrs {
				if ret, ok := instr.(*ssa.Return); ok {
					if len(ret.Results) != 1 || !readableStatement(graph, ret.Results[0], visiting) {
						return false
					}
				}
			}
		}
		return true
	}
	return false
}

func sqlConstants(fn *ssa.Function) []string {
	var out []string
	for _, block := range fn.Blocks {
		for _, instr := range block.Instrs {
			var operands [8]*ssa.Value
			for _, operand := range instr.Operands(operands[:0]) {
				if operand == nil {
					continue
				}
				c, ok := (*operand).(*ssa.Const)
				if !ok || c.Value == nil || c.Value.Kind() != constant.String {
					continue
				}
				if text := constant.StringVal(c.Value); isSQL(text) {
					out = append(out, text)
				}
			}
		}
	}
	return out
}

func isSQL(text string) bool {
	body := stripLeadingComments(text)
	upper := strings.ToUpper(body)
	for _, keyword := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "WITH"} {
		if strings.HasPrefix(upper, keyword) && len(upper) > len(keyword) &&
			strings.ContainsAny(upper[len(keyword):len(keyword)+1], " \t\r\n") {
			return true
		}
	}
	return false
}

func stripLeadingComments(text string) string {
	body := strings.TrimSpace(text)
	for strings.HasPrefix(body, "--") {
		newline := strings.IndexByte(body, '\n')
		if newline < 0 {
			return ""
		}
		body = strings.TrimSpace(body[newline+1:])
	}
	return body
}

// constantNames maps a SQL string back to the constant that declares it, for the report.
func constantNames(initial []*packages.Package) map[string]string {
	names := map[string]string{}
	packages.Visit(initial, nil, func(pkg *packages.Package) {
		if (!strings.HasPrefix(pkg.PkgPath, module) && !strings.HasPrefix(pkg.PkgPath, platform)) || pkg.Types == nil {
			return
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			c, ok := scope.Lookup(name).(*types.Const)
			if !ok || c.Val().Kind() != constant.String {
				continue
			}
			if text := constant.StringVal(c.Val()); isSQL(text) {
				short := strings.TrimPrefix(strings.TrimPrefix(pkg.PkgPath, module+"/"), "github.com/anshacerbia2/")
				names[text] = short + "." + name
			}
		}
	})
	return names
}

func position(fset *token.FileSet, pos token.Pos) string {
	if !pos.IsValid() {
		return "(no position)"
	}
	p := fset.Position(pos)
	file := p.Filename
	if index := strings.Index(file, "organization-control"); index >= 0 {
		file = file[index+len("organization-control")+1:]
	}
	return fmt.Sprintf("%s:%d", strings.ReplaceAll(file, "\\", "/"), p.Line)
}

func preview(sql string) string {
	flat := strings.Join(strings.Fields(sql), " ")
	if len(flat) > 90 {
		flat = flat[:90] + "..."
	}
	return flat
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func unique(list []string) []string {
	sort.Strings(list)
	out := list[:0]
	for i, value := range list {
		if i == 0 || value != list[i-1] {
			out = append(out, value)
		}
	}
	return out
}
