package consent

// Guardrail 4 and 5 (docs/security/m2-incremental-scope.md): the pure evaluator must not import a
// database, HTTP or journal package, and #22-#24 code must reference no journal, erasure, restore
// or witness interface. These tests are the "separation demonstrated" gate.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsentPackageIsPure(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"database/sql", "github.com/jackc/pgx", "net/http", "internal/platform"}
	for _, imp := range append(append([]string{}, pkg.Imports...), pkg.TestImports...) {
		for _, b := range banned {
			if imp == b || strings.HasPrefix(imp, b+"/") {
				t.Fatalf("internal/consent must stay pure, but imports %q", imp)
			}
		}
	}
}

// TestNoJournalOrErasureSymbols parses the non-test sources of this package and of the platform
// consent adapter, and fails if any identifier names a journal, erasure, restore, witness or recovery
// operation. Comments and string literals are ignored, so prose about the boundary does not trip it.
// A lifecycle status field is state, not an operation, and is allowed.
func TestNoJournalOrErasureSymbols(t *testing.T) {
	forbidden := []string{"journal", "erasure", "erase", "witness", "restore", "legalhold", "recovery"}
	names := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := filepath.Glob("../platform/consent*.go")
	if err != nil || len(adapter) == 0 {
		t.Fatalf("platform consent adapter not found: %v", err)
	}
	files = append(files, adapter...)
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok {
				names[id.Name] = true
			}
			return true
		})
	}
	for name := range names {
		low := strings.ToLower(name)
		for _, f := range forbidden {
			if strings.Contains(low, f) {
				t.Fatalf("consent code references a forbidden symbol %q (contains %q)", name, f)
			}
		}
	}
	_ = os.Stdout
}
