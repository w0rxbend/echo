package matrix

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// The per-name Prometheus series are registered by iterating the two published
// name lists, so a callback name recorded at a site neither list enumerates is
// counted in Health() and absent from /metrics -- graphable nowhere, alertable
// nowhere. assertSchedulerCallbackNamesAreListed catches that only for names a
// test actually drives, and reconnect_delay, probe_failure and reconnect_failure
// are reachable only through waitReady and the reconnect loop. This reads the
// package source instead, so an unenumerated name fails here whether or not any
// test exercises the path that records it.
func TestObservabilityCallbackSitesAreAllListed(t *testing.T) {
	values, sites := parseObservabilityCallbacks(t)

	listed := make(map[string]bool)
	for _, name := range SchedulerObservabilityCallbackNames() {
		listed[name] = true
	}
	for _, name := range TCPClientObservabilityCallbackNames() {
		listed[name] = true
	}

	recorded := make(map[string]bool, len(sites))
	for _, site := range sites {
		value, ok := values[site.constName]
		if !ok {
			t.Fatalf("%s: panic site tags %q, which is not an observability callback constant", site.pos, site.constName)
		}
		recorded[value] = true
		if !listed[value] {
			t.Fatalf("%s records callback panics under %q, which neither SchedulerObservabilityCallbackNames nor TCPClientObservabilityCallbackNames lists: it would be counted in Health() and have no /metrics series", site.pos, value)
		}
	}

	// A name that no site records is a stale list entry: it registers a series
	// that can only ever read zero, which reads as "this never panics" rather
	// than "nothing can report here".
	for name := range listed {
		if !recorded[name] {
			t.Errorf("%q is listed for per-name panic metrics but no site in this package records under it", name)
		}
	}

	if len(sites) == 0 {
		t.Fatal("found no callback panic sites; the scan stopped matching and this guard is no longer guarding anything")
	}
}

func TestTCPClientObservabilityCallbackNamesAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, name := range TCPClientObservabilityCallbackNames() {
		if name == "" {
			t.Fatal("TCPClientObservabilityCallbackNames contains an empty name")
		}
		if seen[name] {
			t.Fatalf("TCPClientObservabilityCallbackNames lists %q twice", name)
		}
		seen[name] = true
	}
}

type callbackSite struct {
	constName string
	pos       string
}

// parseObservabilityCallbacks returns the string value of every
// observabilityCallback* constant in the package (resolving the unexported
// aliases through to the exported literals) and every Run/RecoverFrom call that
// tags a panic with one of them.
func parseObservabilityCallbacks(t *testing.T) (map[string]string, []callbackSite) {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package source: %v", err)
	}

	isCallbackConst := func(name string) bool {
		return strings.HasPrefix(name, "ObservabilityCallback") || strings.HasPrefix(name, "observabilityCallback")
	}

	values := make(map[string]string)
	aliases := make(map[string]string)
	var sites []callbackSite

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.ValueSpec:
					for i, ident := range node.Names {
						if !isCallbackConst(ident.Name) || i >= len(node.Values) {
							continue
						}
						switch value := node.Values[i].(type) {
						case *ast.BasicLit:
							if value.Kind == token.STRING {
								unquoted, err := strconv.Unquote(value.Value)
								if err != nil {
									t.Fatalf("%s: unquote %s: %v", fset.Position(value.Pos()), ident.Name, err)
								}
								values[ident.Name] = unquoted
							}
						case *ast.Ident:
							aliases[ident.Name] = value.Name
						}
					}
				case *ast.CallExpr:
					sel, ok := node.Fun.(*ast.SelectorExpr)
					if !ok || len(node.Args) == 0 {
						return true
					}
					if sel.Sel.Name != "Run" && sel.Sel.Name != "RecoverFrom" {
						return true
					}
					arg, ok := node.Args[0].(*ast.Ident)
					if !ok || !isCallbackConst(arg.Name) {
						return true
					}
					sites = append(sites, callbackSite{
						constName: arg.Name,
						pos:       fset.Position(node.Pos()).String(),
					})
				}
				return true
			})
		}
	}

	for alias, target := range aliases {
		value, ok := values[target]
		if !ok {
			t.Fatalf("constant %s aliases %s, which has no string value", alias, target)
		}
		values[alias] = value
	}

	return values, sites
}
