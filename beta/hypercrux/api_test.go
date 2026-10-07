// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// leftOut are 0.x's exported names that the Beta leaves out, since they
// have no meaning without SQLite (BETA.md, "The Go package and the
// command"). Methods are written as Type.Method.
var leftOut = []string{"ApplicationID", "DriverName", "DB.Adopt", "Tx.Adopt"}

// added are the Beta's exported names that 0.x doesn't have. Anything else
// the Beta exports beyond 0.x's API fails TestTheAPIMatches0x, so a name
// joins the API by joining this list.
var added = []string{
	"DB.Compact",
	"Damage",
	"ErrClosed",
	"ErrDamaged",
	"ErrFormatVersion",
	"ErrInsideUpdate",
	"ErrLockTimeout",
	"ErrNotDatabase",
	"ErrStuck",
	"ErrZeroX",
}

// decl is one exported name as TestTheAPIMatches0x compares it.
type decl struct {
	kind  string // const, var, type, func or method
	shape string // what has to match: the types of a func or method, a type's definition, or a const's or var's declared type
	doc   bool   // whether it has a comment
}

// TestTheAPIMatches0x holds the Beta's exported API to 0.x's. Every
// exported name in 0.x's package, apart from those in leftOut, is in the
// Beta's, as the same kind of name, with the same types, and with a comment
// where 0.x has one. Types are compared by their exported parts. Beyond
// 0.x's names the Beta exports only those in added, and each of them has a
// comment. The test reads both packages' source, so it needs neither to
// work.
func TestTheAPIMatches0x(t *testing.T) {
	root := filepath.Join("..", "..")
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.HasPrefix(mod, []byte("module github.com/hypercrux/hypercrux\n")) {
		t.Fatalf("%s isn't the module's root, where 0.x's package is: %v", root, err)
	}
	zero := exported(t, root)
	beta := exported(t, ".")
	for _, name := range []string{"Open", "DB.Update", "Tx.Put", "Vector", "ErrInvalid", "Out"} {
		if _, ok := zero[name]; !ok {
			t.Fatalf("0.x's package, as read, has no %s", name)
		}
	}

	for _, name := range sortedKeys(zero) {
		z := zero[name]
		b, ok := beta[name]
		switch {
		case slices.Contains(leftOut, name):
			if ok {
				t.Errorf("the Beta exports %s, which BETA.md leaves out", name)
			}
		case !ok:
			t.Errorf("the Beta has no %s %s", z.kind, name)
		case b.kind != z.kind:
			t.Errorf("%s is a %s in the Beta and a %s in 0.x", name, b.kind, z.kind)
		case b.shape != z.shape:
			t.Errorf("%s %s differs:\nBeta %s\n0.x  %s", z.kind, name, b.shape, z.shape)
		case z.doc && !b.doc:
			t.Errorf("%s has a comment in 0.x and none in the Beta", name)
		}
	}
	for _, name := range sortedKeys(beta) {
		if _, ok := zero[name]; ok {
			continue
		}
		switch {
		case !slices.Contains(added, name):
			t.Errorf("the Beta exports %s, which 0.x doesn't, and it isn't in added", name)
		case !beta[name].doc:
			t.Errorf("the Beta's %s has no comment", name)
		}
	}
	for _, name := range leftOut {
		if _, ok := zero[name]; !ok {
			t.Errorf("leftOut names %s, which 0.x doesn't export", name)
		}
	}
	for _, name := range added {
		if _, ok := zero[name]; ok {
			t.Errorf("added names %s, which 0.x exports too", name)
		} else if _, ok := beta[name]; !ok {
			t.Errorf("added names %s, which the Beta doesn't export", name)
		}
	}
}

// TestEveryErrorIsExported checks that each error value and type in
// beta/internal/errs is in the API under its own name, as P3 settled, so an
// error added there can't stay out of reach of callers.
func TestEveryErrorIsExported(t *testing.T) {
	beta := exported(t, ".")
	names := errsNames(t)
	if !slices.Contains(names, "ErrNotFound") || !slices.Contains(names, "Damage") {
		t.Fatalf("errs, as read, gives %v", names)
	}
	for _, name := range names {
		if _, ok := beta[name]; !ok {
			t.Errorf("errs has %s, and the Beta's package doesn't export it", name)
		}
	}
}

// TestTheConstantsAre0xs checks the limits and the directions, which the
// Beta keeps from 0.x.
func TestTheConstantsAre0xs(t *testing.T) {
	for _, c := range []struct {
		name     string
		beta, zx int
	}{
		{"MaxKeyLen", hc.MaxKeyLen, zx.MaxKeyLen},
		{"MaxDims", hc.MaxDims, zx.MaxDims},
		{"MaxK", hc.MaxK, zx.MaxK},
		{"MaxDepth", hc.MaxDepth, zx.MaxDepth},
		{"Out", int(hc.Out), int(zx.Out)},
		{"In", int(hc.In), int(zx.In)},
		{"Both", int(hc.Both), int(zx.Both)},
	} {
		if c.beta != c.zx {
			t.Errorf("%s is %d, and %d in 0.x", c.name, c.beta, c.zx)
		}
	}
}

// TestFormatVersionIsTheSpecs checks FormatVersion against beta/FORMAT.md,
// in its title and in the header's table, so the two move together.
func TestFormatVersionIsTheSpecs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "FORMAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	spec, v := string(b), strconv.Itoa(hc.FormatVersion)
	if title, _, _ := strings.Cut(spec, "\n"); title != "# The HyperCrux Beta file format, version "+v {
		t.Errorf("FORMAT.md's title is %q, and FormatVersion is %s", title, v)
	}
	if !strings.Contains(spec, "| 8 | 4 | Format version, u32: "+v+" |") {
		t.Errorf("FORMAT.md's table of the header doesn't give format version %s", v)
	}
}

// errsNames returns the names of the error values, the vars named Err...,
// and the exported types in beta/internal/errs.
func errsNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for name, d := range exported(t, filepath.Join("..", "internal", "errs")) {
		if d.kind == "type" || (d.kind == "var" && strings.HasPrefix(name, "Err")) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// exported reads the package in dir, from the files a build for this
// system would use, and returns its exported names: top-level names as
// they are, and methods as Type.Method.
func exported(t *testing.T, dir string) map[string]decl {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]decl{}
	add := func(name string, d decl) {
		if _, ok := out[name]; ok {
			t.Fatalf("%s: %s is declared twice", dir, name)
		}
		out[name] = d
	}
	for _, e := range entries {
		file := e.Name()
		if e.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		if match, err := build.Default.MatchFile(dir, file); err != nil {
			t.Fatal(err)
		} else if !match {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					add(d.Name.Name, decl{"func", funcShape(fset, d.Type), d.Doc != nil})
					continue
				}
				recv, base := receiver(fset, d.Recv)
				if ast.IsExported(base) {
					add(base+"."+d.Name.Name, decl{"method", recv + " " + funcShape(fset, d.Type), d.Doc != nil})
				}
			case *ast.GenDecl:
				// A constant without a type or values repeats the last one
				// before it that has values, type and all.
				var constType ast.Expr
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							add(s.Name.Name, decl{"type", typeShape(fset, s), d.Doc != nil || s.Doc != nil || s.Comment != nil})
						}
					case *ast.ValueSpec:
						kind, typ := "var", s.Type
						if d.Tok == token.CONST {
							if len(s.Values) > 0 {
								constType = s.Type
							}
							kind, typ = "const", constType
						}
						for _, n := range s.Names {
							if n.IsExported() {
								add(n.Name, decl{kind, text(fset, typ), d.Doc != nil || s.Doc != nil || s.Comment != nil})
							}
						}
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s exports nothing", dir)
	}
	return out
}

// funcShape is a function's parameter and result types, without their
// names, which aren't part of its type.
func funcShape(fset *token.FileSet, ft *ast.FuncType) string {
	s := "func(" + fieldTypes(fset, ft.Params) + ")"
	if ft.Results != nil && len(ft.Results.List) > 0 {
		s += " (" + fieldTypes(fset, ft.Results) + ")"
	}
	if ft.TypeParams != nil {
		s = "[" + fieldTypes(fset, ft.TypeParams) + "] " + s
	}
	return s
}

// fieldTypes lists the types in a list of parameters or results, once for
// each name.
func fieldTypes(fset *token.FileSet, list *ast.FieldList) string {
	var types []string
	for _, f := range list.List {
		for range max(1, len(f.Names)) {
			types = append(types, text(fset, f.Type))
		}
	}
	return strings.Join(types, ", ")
}

// receiver returns a method's receiver type as written, such as *DB, and the
// name of the type it's on.
func receiver(fset *token.FileSet, recv *ast.FieldList) (string, string) {
	typ := recv.List[0].Type
	written := text(fset, typ)
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return written, id.Name
	}
	return written, ""
}

// typeShape is what a type's declaration exports: whether it's an alias,
// and its definition, or for a struct its exported fields in order and
// every embedded field, since an embedded field's methods join the type's.
func typeShape(fset *token.FileSet, s *ast.TypeSpec) string {
	shape := ""
	if s.Assign.IsValid() {
		shape = "= "
	}
	if s.TypeParams != nil {
		shape += "[" + fieldTypes(fset, s.TypeParams) + "] "
	}
	st, ok := s.Type.(*ast.StructType)
	if !ok {
		return shape + text(fset, s.Type)
	}
	var fields []string
	for _, f := range st.Fields.List {
		typ := text(fset, f.Type)
		if len(f.Names) == 0 {
			fields = append(fields, "embedded "+typ)
		}
		for _, n := range f.Names {
			if n.IsExported() {
				fields = append(fields, n.Name+" "+typ)
			}
		}
	}
	return shape + "struct{" + strings.Join(fields, "; ") + "}"
}

// text prints an expression as Go source, or "" for none.
func text(fset *token.FileSet, e ast.Expr) string {
	if e == nil {
		return ""
	}
	var b bytes.Buffer
	if err := printer.Fprint(&b, fset, e); err != nil {
		panic(err)
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
