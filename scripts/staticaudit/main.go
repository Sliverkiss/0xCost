// staticaudit is a stdlib-only AST pass over this repository's own code. It looks
// for the defect shapes this codebase has actually produced, not for general style:
//
//  1. an error result thrown away — `_ = f()` or a bare call — for calls to
//     known error-returning functions used as statements;
//  2. a `strings.Cut`/`CutPrefix`/`CutSuffix` whose boolean ("found") is discarded,
//     which is how a gate silently stops being a gate;
//  3. `defer` or `time.After` inside a loop body (lifetime of a resource or timer
//     tied to the function, not the iteration).
//
// Usage: go run staticaudit.go <dir>...
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// errorFuncs is deliberately narrow: it carries the calls whose discarded error
// hides a wrong answer, not every method that can return one. Left out on purpose,
// each after reading every site in this repo: Encode/Marshal on a response writer
// after WriteHeader (nothing left to do about a failed write), Close and io.Copy into
// io.Discard (deliberate), and the setters (Set on a header or form has no error).
var errorFuncs = map[string]bool{
	"Unmarshal": true, "WriteFile": true, "ReadFile": true, "Copy": true,
	"ReadAll": true, "NewRequest": true, "NewRequestWithContext": true,
	"ParseInt": true, "Atoi": true, "Exec": true, "Remove": true,
	"Rename": true, "MkdirAll": true, "Create": true, "Open": true,
	"OpenFile": true, "ListenAndServe": true, "Ping": true, "Run": true,
}

var cutFuncs = map[string]bool{"Cut": true, "CutPrefix": true, "CutSuffix": true}

// notErrors are method names that look like the map above but never return an error
// by themselves in this codebase (cache getters, option setters).
var notErrors = map[string]bool{
	"Unwrap": true, "Error": true, "String": true, "Close": true,
}

type finding struct {
	pos  token.Position
	kind string
	text string
}

func main() {
	dirs := os.Args[1:]
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	var findings []finding
	fset := token.NewFileSet()
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if perr != nil {
				findings = append(findings, finding{fset.Position(token.Pos(1)), "parse-error", perr.Error()})
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				audit(fset, n, &findings)
				return true
			})
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "walk:", err)
		}
	}
	if len(findings) == 0 {
		fmt.Println("clean: no findings")
		return
	}
	for _, fd := range findings {
		fmt.Printf("%s:%d:%d  [%s]  %s\n", fd.pos.Filename, fd.pos.Line, fd.pos.Column, fd.kind, fd.text)
	}
	fmt.Printf("\n%d finding(s)\n", len(findings))
}

func audit(fset *token.FileSet, n ast.Node, out *[]finding) {
	switch node := n.(type) {
	case *ast.AssignStmt:
		for i, rhs := range node.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			if isCut(call) {
				if len(node.Lhs) < 2 {
					*out = append(*out, finding{fset.Position(call.Pos()), "cut-under-consumed",
						"`" + short(call) + "` used as a single value: the found flag is gone"})
					continue
				}
				for _, lhs := range node.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
						*out = append(*out, finding{fset.Position(call.Pos()), "cut-under-consumed",
							"`" + short(call) + "`: one of its results is discarded"})
						break
					}
				}
				continue
			}
			if !returnsError(call) {
				continue
			}
			if i < len(node.Lhs) {
				if id, ok := node.Lhs[i].(*ast.Ident); ok && id.Name == "_" {
					*out = append(*out, finding{fset.Position(call.Pos()), "discarded-error", "`_ = " + short(call) + "`"})
				}
			}
		}
	case *ast.ExprStmt:
		call, ok := node.X.(*ast.CallExpr)
		if !ok || isCut(call) || !returnsError(call) {
			return
		}
		*out = append(*out, finding{fset.Position(call.Pos()), "discarded-error", "bare call `" + short(call) + "`"})
	case *ast.ForStmt, *ast.RangeStmt:
		ast.Inspect(loopBody(n), func(inner ast.Node) bool {
			switch in := inner.(type) {
			case *ast.DeferStmt:
				*out = append(*out, finding{fset.Position(in.Pos()), "defer-in-loop", "defers to function exit, not iteration end"})
			case *ast.CallExpr:
				if sel, ok := in.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "After" {
					*out = append(*out, finding{fset.Position(in.Pos()), "timer-in-loop", "time.After's timer outlives the iteration that created it"})
				}
			}
			return true
		})
	}
}

func loopBody(n ast.Node) ast.Node {
	switch v := n.(type) {
	case *ast.ForStmt:
		return v.Body
	case *ast.RangeStmt:
		return v.Body
	}
	return n
}

func isCut(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && cutFuncs[sel.Sel.Name]
}

func returnsError(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		if id, ok := call.Fun.(*ast.Ident); ok {
			return errorFuncs[id.Name]
		}
		return false
	}
	if notErrors[sel.Sel.Name] {
		return false
	}
	return errorFuncs[sel.Sel.Name]
}

func short(call *ast.CallExpr) string {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok {
			return pkg.Name + "." + sel.Sel.Name + "(...)"
		}
		return sel.Sel.Name + "(...)"
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		return id.Name + "(...)"
	}
	return "call(...)"
}
