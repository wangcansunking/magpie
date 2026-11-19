package wan

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// Each runtime exit is checked at its real caller, including the two paths
// that never return to Main (macOS termination and Unix web reexec).
func TestProcessExitPathsWaitForWAN(t *testing.T) {
	for _, path := range []struct {
		file, function, before string
	}{
		{"../cli/main.go", "Main", "proc.EndProbes"},
		{"../cli/signals.go", "endProbesOnSignal", "proc.EndProbes"},
		{"../gui/app.go", "Run", "proc.EndProbes"},
		{"../gui/web.go", "Wait", "update.Executable"},
	} {
		t.Run(path.file, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), path.file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			var body *ast.BlockStmt
			for _, declaration := range file.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if ok && fn.Name.Name == path.function {
					body = fn.Body
				}
			}
			if body == nil {
				t.Fatalf("missing exit function %s", path.function)
			}
			if path.file == "../gui/app.go" {
				var shutdown *ast.BlockStmt
				ast.Inspect(body, func(node ast.Node) bool {
					kv, ok := node.(*ast.KeyValueExpr)
					if !ok {
						return true
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "OnShutdown" {
						return true
					}
					if fn, ok := kv.Value.(*ast.FuncLit); ok {
						shutdown = fn.Body
					}
					return false
				})
				if shutdown == nil {
					t.Fatal("missing Wails OnShutdown hook")
				}
				body = shutdown
			}
			var shutdown, before token.Pos
			ast.Inspect(body, func(node ast.Node) bool {
				// It must be waited, not put in another goroutine or deferred
				// past an os.Exit/syscall.Exec that cannot run that defer.
				statement, ok := node.(*ast.ExprStmt)
				if !ok {
					return true
				}
				call, ok := statement.X.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callName(call) == "wan.Shutdown" {
					shutdown = call.Pos()
				}
				return true
			})
			ast.Inspect(body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok && callName(call) == path.before {
					before = call.Pos()
				}
				return true
			})
			if shutdown == token.NoPos || before == token.NoPos || shutdown >= before {
				t.Fatalf("%s must synchronously stop WAN before %s", path.function, path.before)
			}
		})
	}
	// Wails/TUI reclaim INT/TERM, but the CLI must retain HUP cleanup.
	file, err := parser.ParseFile(token.NewFileSet(), "../cli/signals.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var haveHangup bool
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "SIGHUP" {
			haveHangup = true
		}
		return true
	})
	if !haveHangup {
		t.Fatal("process exit cleanup lost SIGHUP")
	}
}

func callName(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	name, ok := selector.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return name.Name + "." + selector.Sel.Name
}

// Exercise the exported process-wide gate in its own process, so tests of
// later owners cannot accidentally reset a gate production never resets.
func TestProcessWANShutdown(t *testing.T) {
	if slices.Contains(os.Args, "--wan-shutdown-process") {
		m, _ := fakeManager(t, "ok", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		m.runtime = &processTunnels
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		Shutdown()
		if m.Status().State != "stopped" {
			t.Fatal(m.Status())
		}
		if err := New(m.handler).Start(context.Background()); err == nil {
			t.Fatal("exported shutdown did not block a new manager")
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := proc.CommandContext(ctx, exe, "-test.run=^TestProcessWANShutdown$", "--", "--wan-shutdown-process")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process shutdown: %v\n%s", err, output)
	}
}
