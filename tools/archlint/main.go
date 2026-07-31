// Command archlint enforces Veyronix's architecture: horizontal module
// isolation, vertical hexagon-layer dependencies, the sdk contract boundary,
// and the platform/composition-root zones. It scans the module from disk with
// go/parser (no external deps), then delegates the actual rule checking to the
// pure engine package.
//
// Usage:
//
//	archlint [root]   # root defaults to "."
//
// Exit codes: 0 clean, 1 violations found, 2 scan error.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickemma/veyronix/tools/archlint/engine"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	cfg := engine.DefaultConfig()
	if mod := modulePath(root); mod != "" {
		cfg.ModuleRoot = mod
	}

	pkgs, err := scan(root, cfg.ModuleRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archlint: scan failed:", err)
		os.Exit(2)
	}

	violations := engine.Check(cfg, pkgs)
	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].From != violations[j].From {
				return violations[i].From < violations[j].From
			}
			return violations[i].To < violations[j].To
		})
		for _, v := range violations {
			fmt.Println(v.String())
		}
		fmt.Printf("\narchlint: %d violation(s) across %d package(s)\n", len(violations), len(pkgs))
		os.Exit(1)
	}
	fmt.Printf("✓ architecture clean (%d packages)\n", len(pkgs))
}

// modulePath reads the `module` directive from go.mod. Returns "" if not found.
func modulePath(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// scan walks root, parses the imports of every non-test .go file, and groups
// them by package import path.
func scan(root, modPath string) ([]engine.Package, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	imports := map[string]map[string]struct{}{}
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != absRoot && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // skip files we can't parse rather than failing the run
		}

		rel, rerr := filepath.Rel(absRoot, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		pkgPath := modPath
		if rel != "." {
			pkgPath = modPath + "/" + filepath.ToSlash(rel)
		}

		set := imports[pkgPath]
		if set == nil {
			set = map[string]struct{}{}
			imports[pkgPath] = set
		}
		for _, spec := range f.Imports {
			set[strings.Trim(spec.Path.Value, `"`)] = struct{}{}
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	pkgs := make([]engine.Package, 0, len(imports))
	for path, set := range imports {
		imps := make([]string, 0, len(set))
		for imp := range set {
			imps = append(imps, imp)
		}
		sort.Strings(imps)
		pkgs = append(pkgs, engine.Package{Path: path, Imports: imps})
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Path < pkgs[j].Path })
	return pkgs, nil
}

func skipDir(name string) bool {
	switch name {
	case "vendor", "testdata", "bin", "node_modules", "web", "gen":
		return true
	}
	return strings.HasPrefix(name, ".")
}
