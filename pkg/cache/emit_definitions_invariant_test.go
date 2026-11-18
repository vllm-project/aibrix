/*
Copyright 2026 The Aibrix Team.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vllm-project/aibrix/pkg/metrics"
)

// emitSite is one call to metrics.EmitMetricToPrometheus, either with a
// statically resolvable metric name (metricName holds the resolved value) or
// with a name computed at runtime (metricName empty).
type emitSite struct {
	metricName string
	pos        string
}

// pendingDefinitions holds metric names that this tree emits without a
// definition entry in metrics.Metrics, mapped to the pull request that is
// adding their definition. An in-flight sibling PR counts: what matters is
// that every entry is temporary and the list can only shrink. The test fails
// when an entry goes stale in either direction, so a merged definition or a
// removed emit forces the entry out instead of leaving a silent exception
// behind.
var pendingDefinitions = map[string]string{
	metrics.RealtimeNumRequestsRunning: "#3499",
}

// TestEmittedMetricNamesHaveDefinitions is the CI guard for #3506: a metric name
// without a definition entry in metrics.Metrics is dropped by
// EmitMetricToPrometheus with no series, which is how #3498 and #3501 lost
// their gauges. Every EmitMetricToPrometheus call site in this package and in
// the gateway plugin whose metric name is statically resolvable must therefore
// resolve to a defined metric, or to a pendingDefinitions entry whose
// definition is landing in the pull request named there.
//
// Call sites whose metric name is computed at runtime are outside this test's
// reach: the cache's variable-argument sites iterate names that come from the
// definitions-driven fetch and query lists, and any runtime name that still
// lacks a definition now increments aibrix_metrics_emit_dropped_total instead
// of vanishing, so it is visible on /metrics.
func TestEmittedMetricNamesHaveDefinitions(t *testing.T) {
	constValues := metricsPackageConstValues(t)

	var resolved, dynamic []emitSite
	// This package's own emit sites; the gateway plugin tree, all of it.
	for _, root := range []string{".", "../plugins/gateway"} {
		r, d := emitCallSites(t, root, constValues)
		resolved = append(resolved, r...)
		dynamic = append(dynamic, d...)
	}
	if len(resolved) == 0 {
		t.Fatal("no EmitMetricToPrometheus call sites found; the scan itself is broken, not the code")
	}

	emittedNames := make(map[string]bool)
	for _, site := range resolved {
		emittedNames[site.metricName] = true
		if _, exists := metrics.Metrics[site.metricName]; exists {
			continue
		}
		if pr, pending := pendingDefinitions[site.metricName]; pending {
			t.Logf("%s: %q has no definition yet; its definition is pending in %s (drop the pendingDefinitions entry once %s lands and this branch rebases onto it)",
				site.pos, site.metricName, pr, pr)
			continue
		}
		t.Errorf("%s: %q is emitted but has no definition entry in metrics.Metrics, so EmitMetricToPrometheus drops it (#3506)",
			site.pos, site.metricName)
	}

	// The pending list must not outlive reality: an entry whose definition
	// landed, or whose emit disappeared, has to be removed.
	for name := range pendingDefinitions {
		if _, exists := metrics.Metrics[name]; exists {
			t.Errorf("pendingDefinitions entry %q already has a definition; remove the entry", name)
		}
		if !emittedNames[name] {
			t.Errorf("pendingDefinitions entry %q is no longer emitted; remove the entry", name)
		}
	}

	for _, site := range dynamic {
		t.Logf("%s: metric name is computed at runtime, not statically checkable", site.pos)
	}
	t.Logf("asserted definitions for %d distinct emitted metric names; %d call sites use runtime-computed names",
		len(emittedNames), len(dynamic))
}

// metricsPackageConstValues returns the metrics package's exported string
// constants (identifier -> value), read from its source so a call site's
// metrics.X argument can be resolved to the metric name it emits.
func metricsPackageConstValues(t *testing.T) map[string]string {
	t.Helper()
	constValues := make(map[string]string)
	fset := token.NewFileSet()
	entries, err := os.ReadDir("../metrics")
	if err != nil {
		t.Fatalf("reading ../metrics: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join("..", "metrics", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing ../metrics/%s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok || len(valueSpec.Names) != 1 || len(valueSpec.Values) != 1 {
					continue
				}
				lit, ok := valueSpec.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if value, err := strconv.Unquote(lit.Value); err == nil {
					constValues[valueSpec.Names[0].Name] = value
				}
			}
		}
	}
	if len(constValues) == 0 {
		t.Fatal("no metric name constants found under ../metrics; the name resolution table is empty")
	}
	return constValues
}

// emitCallSites walks root (the top level only when root is "."), parses every
// non-test Go file, and returns the EmitMetricToPrometheus call sites whose
// metric name could be resolved to a constant or literal value, and those whose
// name is computed at runtime.
func emitCallSites(t *testing.T, root string, constValues map[string]string) (resolved, dynamic []emitSite) {
	t.Helper()
	fset := token.NewFileSet()

	var files []string
	if root == "." {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("reading %s: %v", root, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			files = append(files, name)
		}
	} else {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "EmitMetricToPrometheus" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "metrics" {
				return true
			}
			if len(call.Args) < 3 {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			switch arg := call.Args[2].(type) {
			case *ast.SelectorExpr:
				if pkg, ok := arg.X.(*ast.Ident); ok && pkg.Name == "metrics" {
					if value, ok := constValues[arg.Sel.Name]; ok {
						resolved = append(resolved, emitSite{metricName: value, pos: pos})
						return true
					}
				}
				dynamic = append(dynamic, emitSite{pos: pos})
			case *ast.BasicLit:
				if arg.Kind == token.STRING {
					if value, err := strconv.Unquote(arg.Value); err == nil {
						resolved = append(resolved, emitSite{metricName: value, pos: pos})
						return true
					}
				}
				dynamic = append(dynamic, emitSite{pos: pos})
			default:
				dynamic = append(dynamic, emitSite{pos: pos})
			}
			return true
		})
	}
	return resolved, dynamic
}
