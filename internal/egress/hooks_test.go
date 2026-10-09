package egress

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// setDialRewrite is the only code that assigns Manager.dialRewrite. It
// lives in a _test.go file, so it is not compiled into any non-test build.
// The deny set itself is never altered: resolver results and IP literals
// are validated as in production, and only the address finally dialled is
// swapped for a loopback test server.
func setDialRewrite(m *Manager, fn func(netip.AddrPort) (netip.AddrPort, bool)) {
	m.dialRewrite = fn
}

// newManagerWithIfaces builds a Manager with a fake host interface list.
func newManagerWithIfaces(cfg Config, fn func() ([]net.Addr, error)) (*Manager, error) {
	return newManager(cfg, fn)
}

// TestDialRewriteIsTestOnly fails if any non-test source file assigns or
// otherwise sets dialRewrite, or if Config grows a field that could carry
// such a hook.
func TestDialRewriteIsTestOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "dialRewrite" {
						t.Errorf("%s: dialRewrite is assigned in production code", fset.Position(v.Pos()))
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := v.Key.(*ast.Ident); ok && id.Name == "dialRewrite" {
					t.Errorf("%s: dialRewrite is set in a composite literal in production code", fset.Position(v.Pos()))
				}
			case *ast.TypeSpec:
				st, ok := v.Type.(*ast.StructType)
				if !ok || v.Name.Name != "Config" {
					return true
				}
				for _, f := range st.Fields.List {
					for _, id := range f.Names {
						l := strings.ToLower(id.Name)
						if strings.Contains(l, "rewrite") || strings.Contains(l, "insecure") || strings.Contains(l, "loopback") || strings.Contains(l, "test") {
							t.Errorf("Config.%s looks like a deny-set bypass", id.Name)
						}
					}
				}
			}
			return true
		})
	}
}
