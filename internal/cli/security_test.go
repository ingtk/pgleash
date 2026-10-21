package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// This structural check is the explicitly agreed exception to CLI-only tests.
// It scans our production sources, not test tooling or dependency examples.
func TestProductionHasNoExecutionOrEnvironmentOverrideAPIs(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate production sources")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	forbiddenImports := map[string]bool{"os/exec": true, "plugin": true, "C": true}
	forbiddenReferences := map[string]map[string]bool{
		"os": {
			"StartProcess": true, "Getenv": true, "LookupEnv": true,
			"ExpandEnv": true, "UserHomeDir": true,
		},
		"os/user": {"Current": true}, // LookupId avoids environment-backed fallbacks.
		"syscall": {
			"Exec": true, "ForkExec": true, "StartProcess": true,
			"Syscall": true, "Syscall6": true, "RawSyscall": true, "RawSyscall6": true,
		},
		"golang.org/x/sys/unix": {
			"Exec": true, "ForkExec": true, "StartProcess": true,
			"Syscall": true, "Syscall6": true, "RawSyscall": true, "RawSyscall6": true,
		},
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		imports := make(map[string]string)
		for _, imp := range file.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if forbiddenImports[pkg] {
				t.Errorf("%s: forbidden production import %s", fset.Position(imp.Pos()), pkg)
			}
			alias := filepath.Base(pkg)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			if alias == "." && forbiddenReferences[pkg] != nil {
				t.Errorf("%s: dot import hides guarded APIs", fset.Position(imp.Pos()))
			}
			imports[alias] = pkg
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			alias, ok := selector.X.(*ast.Ident)
			if ok && forbiddenReferences[imports[alias.Name]][selector.Sel.Name] && !aclMetadataSyscall(file, selector, imports) {
				t.Errorf("%s: forbidden production API %s.%s", fset.Position(selector.Pos()), imports[alias.Name], selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The only raw system call permitted is Darwin's descriptor-based metadata
// read, with a literal syscall selector. No dynamic syscall number is allowed.
func aclMetadataSyscall(file *ast.File, selector *ast.SelectorExpr, imports map[string]string) bool {
	alias, ok := selector.X.(*ast.Ident)
	if !ok || imports[alias.Name] != "syscall" || selector.Sel.Name != "Syscall6" {
		return false
	}
	allowed := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || call.Fun != selector || len(call.Args) != 7 {
			return true
		}
		constant, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := constant.X.(*ast.Ident)
		allowed = ok && imports[pkg.Name] == "golang.org/x/sys/unix" && constant.Sel.Name == "SYS_FGETATTRLIST"
		return true
	})
	return allowed
}
