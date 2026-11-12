package approve

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The approve source can write to GHE only through GH.Approve: one exec site
// (GH.run), called only by PR, Reviews and Approve; no forge.GH (which can
// comment, push and merge); and the ebac-approve binary links nothing but
// this package and the plugin protocol.
func TestNoOtherGHEWriteIsReachable(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	execSites, runCallers := 0, map[string]bool{}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "os/exec" {
				t.Errorf("%s imports os/exec: the only exec is forge.ExecCommandContext in GH.run", f)
			}
		}
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "forge" {
						switch x.Sel.Name {
						case "ExecCommandContext":
							execSites++
							if fd.Name.Name != "run" {
								t.Errorf("%s: exec outside GH.run (in %s)", f, fd.Name.Name)
							}
						case "NewCred", "Cred", "CredGitHub", "CredTeams":
						default:
							t.Errorf("%s: forge.%s used (in %s): only the credential and the exec hook are allowed", f, x.Sel.Name, fd.Name.Name)
						}
					}
				case *ast.CallExpr:
					if s, ok := x.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "run" {
						runCallers[fd.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	if checked < 4 {
		t.Fatalf("only %d source files checked: the test is blind", checked)
	}
	if execSites != 1 {
		t.Fatalf("%d exec sites, want exactly 1 (GH.run)", execSites)
	}
	var callers []string
	for c := range runCallers {
		callers = append(callers, c)
	}
	sort.Strings(callers)
	if strings.Join(callers, ",") != "Approve,PR,Reviews" {
		t.Fatalf("GH.run called from %v, want only Approve, PR, Reviews", callers)
	}

	main, err := parser.ParseFile(fset, "../../../cmd/ebac-approve/main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var imps []string
	for _, im := range main.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		imps = append(imps, p)
	}
	sort.Strings(imps)
	if strings.Join(imps, ",") != "github.com/gluck/ebac/internal/plugin,github.com/gluck/ebac/internal/source/approve" {
		t.Fatalf("ebac-approve imports %v", imps)
	}
}
