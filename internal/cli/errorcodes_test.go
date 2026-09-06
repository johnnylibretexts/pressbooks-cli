package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestErrorCodesCoverEveryEmittedCode parses this package's own non-test
// source and collects every string literal assigned to a field or variable
// named Code — every agentInputError{Code: "..."} literal, every
// payload.Code = "..." assignment in classifyAgentError, and every
// agentErrorCodeDoc{Code: "..."} entry in currentAgentSchema's own list —
// then checks that currentAgentSchema().ErrorCodes documents every one of
// them. This is the drift guard the "error_codes" minor fix promised: the
// README, SKILL.md, spec.yaml and tools-manifest.json are meant to be
// generated from that same list by hand, so this test is what keeps the
// list itself from silently losing a code a source change added elsewhere
// in the package — the exact failure mode that let eleven documented codes
// and sixteen actually-emitted ones drift three-out-of-eleven apart before
// this fix.
func TestErrorCodesCoverEveryEmittedCode(t *testing.T) {
	emitted := map[string]bool{}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				if ident, ok := node.Key.(*ast.Ident); ok && ident.Name == "Code" {
					if code, ok := stringLiteral(node.Value); ok {
						emitted[code] = true
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Code" || i >= len(node.Rhs) {
						continue
					}
					if code, ok := stringLiteral(node.Rhs[i]); ok {
						emitted[code] = true
					}
				}
			}
			return true
		})
	}
	if len(emitted) == 0 {
		t.Fatal("test setup: found no Code literals to check — did the parse glob or field name change?")
	}

	documented := map[string]bool{}
	for _, doc := range currentAgentSchema().ErrorCodes {
		if documented[doc.Code] {
			t.Errorf("error_codes lists %q more than once", doc.Code)
		}
		documented[doc.Code] = true
	}

	for code := range emitted {
		if !documented[code] {
			t.Errorf("code %q is emitted somewhere in the package but missing from currentAgentSchema().ErrorCodes", code)
		}
	}
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}
