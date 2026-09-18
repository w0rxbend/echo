// Package obstest provides source-level guards shared by the packages that
// record recovered callback panics under a name.
//
// Per-name Prometheus series are registered by iterating a published name list,
// so a name recorded at a site that no list enumerates is counted in Health()
// -- whose map is unbounded -- and absent from /metrics, which iterates the
// fixed list. Such a name is graphable nowhere and alertable nowhere. A runtime
// guard catches that only for names a test actually drives, and several names
// are reachable only through paths no unit test enters. These helpers read the
// package source instead, so an unenumerated name fails whether or not any test
// exercises the path that records it.
package obstest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CallbackSite is one Run/RecoverFrom call that tags a panic with a callback
// name constant.
type CallbackSite struct {
	ConstName string
	Pos       string
}

// AssertCallbackSitesAreListed checks both directions of the correspondence
// between the panic-tagging sites in the package rooted at dir and the names
// that listed enumerates: every recorded name must be listed, and every listed
// name must be recorded somewhere. listSource names the list for the failure
// message, e.g. "SchedulerObservabilityCallbackNames".
func AssertCallbackSitesAreListed(t testing.TB, dir, listSource string, listed []string) {
	t.Helper()

	values, sites := ParseCallbackSites(t, dir)

	isListed := make(map[string]bool, len(listed))
	for _, name := range listed {
		isListed[name] = true
	}

	recorded := make(map[string]bool, len(sites))
	for _, site := range sites {
		value, ok := values[site.ConstName]
		if !ok {
			t.Fatalf("%s: panic site tags %q, which is not an observability callback constant", site.Pos, site.ConstName)
		}
		recorded[value] = true
		if !isListed[value] {
			t.Fatalf("%s records callback panics under %q, which %s does not list: it would be counted in Health() and have no /metrics series", site.Pos, value, listSource)
		}
	}

	// A name that no site records is a stale list entry: it registers a series
	// that can only ever read zero, which reads as "this never panics" rather
	// than "nothing can report here".
	for name := range isListed {
		if !recorded[name] {
			t.Errorf("%q is listed by %s for per-name panic metrics but no site in %s records under it", name, listSource, dir)
		}
	}

	if len(sites) == 0 {
		t.Fatalf("found no callback panic sites in %s; the scan stopped matching and this guard is no longer guarding anything", dir)
	}
}

// AssertNamesAreDistinct fails if names contains an empty or repeated entry. A
// repeat would register the same Prometheus series twice, and prometheus.Register
// rejects the second, so the whole list silently loses its metrics.
func AssertNamesAreDistinct(t testing.TB, listName string, names []string) {
	t.Helper()

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" {
			t.Fatalf("%s contains an empty name", listName)
		}
		if seen[name] {
			t.Fatalf("%s lists %q twice", listName, name)
		}
		seen[name] = true
	}
}

// ParseCallbackSites returns the string value of every ObservabilityCallback*
// constant declared in the package rooted at dir (resolving unexported aliases
// through to the exported literals) and every Run/RecoverFrom call that tags a
// panic with one of them. Test files are skipped: a name only a test records is
// not one production can report under.
func ParseCallbackSites(t testing.TB, dir string) (map[string]string, []CallbackSite) {
	t.Helper()

	// Each file is parsed individually rather than through parser.ParseDir,
	// which is deprecated: it ignores build tags when grouping files into
	// packages. Grouping is not wanted here anyway -- every non-test .go file in
	// the directory is a site that can record a name, whatever package clause or
	// build tag it carries.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package source in %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("found no non-test Go files in %s; this guard is no longer guarding anything", dir)
	}

	isCallbackConst := func(name string) bool {
		return strings.HasPrefix(name, "ObservabilityCallback") || strings.HasPrefix(name, "observabilityCallback")
	}

	values := make(map[string]string)
	aliases := make(map[string]string)
	var sites []CallbackSite

	for _, file := range files {
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
				sites = append(sites, CallbackSite{
					ConstName: arg.Name,
					Pos:       fset.Position(node.Pos()).String(),
				})
			}
			return true
		})
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
