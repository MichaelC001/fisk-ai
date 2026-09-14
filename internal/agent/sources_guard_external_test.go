//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

// Every tool source has one caller, Assemble. This walks the repository's non-test Go
// files and fails on a site that calls a source by hand, so the next surface cannot
// list, count or claim its tools differently from the others.
package agent_test

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// guardedSource is an exported function of a source package that only Assemble calls.
// pkg is the import path with the module path stripped.
type guardedSource struct {
	pkg  string
	name string
}

var guardedSources = []guardedSource{
	{"internal/toolkit/builtin", "HITLTools"},
	{"internal/toolkit/builtin", "MemoryTools"},
	{"internal/toolkit/builtin", "RAGTools"},
	{"internal/toolkit/fisktool", "LoadTools"},
	{"internal/toolkit/fisktool", "ServedTools"},
	{"internal/remotetools", "ImportForRun"},
	{"internal/remotetools", "ImportHosts"},
	{"internal/mcpclient", "Import"},
}

// assemblerFile is the one file outside the source packages that may call a source.
const assemblerFile = "internal/agent/assemble.go"

// walkGoFiles skips these directories, testdata, and any whose name starts with a
// dot.
var skippedDirs = map[string]bool{"dist": true, "docs": true, "poc": true, "testdata": true}

// sourceUse is a selector or a dot import in one file that resolves to a guarded
// source.
type sourceUse struct {
	source guardedSource
	pos    string
}

var _ = Describe("Assemble is the only caller of a tool source", func() {
	It("finds each source declared in its package and called from assemble.go alone", func() {
		root := repositoryRoot()
		module := modulePath(root)

		declared := map[guardedSource]bool{}
		var handWritten []string
		walkGoFiles(root, func(rel string, f *ast.File, fset *token.FileSet) {
			for _, g := range guardedSources {
				if path.Dir(rel) == g.pkg && declaresFunc(f, g.name) {
					declared[g] = true
				}
			}

			for _, use := range sourceUses(module, rel, f, fset) {
				if rel == assemblerFile || path.Dir(rel) == use.source.pkg {
					continue
				}
				handWritten = append(handWritten, fmt.Sprintf("%s calls %s.%s", use.pos, path.Base(use.source.pkg), use.source.name))
			}
		})

		var missing []string
		for _, g := range guardedSources {
			if !declared[g] {
				missing = append(missing, g.pkg+"."+g.name)
			}
		}

		Expect(missing).To(BeEmpty(), "a guarded source is not declared where this test looks for it; update guardedSources")
		Expect(handWritten).To(BeEmpty(), "a tool source is called outside Assemble; add the source to the assembler and read the assembly instead")
	})

	// The table feeds a site written three ways through the resolver the walk uses, so
	// the guard is shown to catch one.
	DescribeTable("finds a hand-written site", func(src string, want int) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "site.go", src, parser.SkipObjectResolution)
		Expect(err).NotTo(HaveOccurred())

		Expect(sourceUses("example.com/mod", "cmd/site.go", f, fset)).To(HaveLen(want))
	},
		Entry("a plain call", `package cmd
import "example.com/mod/internal/toolkit/builtin"
var _ = builtin.HITLTools(nil)`, 1),
		Entry("an aliased import", `package cmd
import bi "example.com/mod/internal/toolkit/builtin"
var _ = bi.MemoryTools(nil, nil)`, 1),
		Entry("a dot import of a guarded package", `package cmd
import . "example.com/mod/internal/toolkit/builtin"
var _ = HITLTools(nil)`, 1),
		Entry("a same-named function from another package", `package cmd
import "example.com/mod/other"
var _ = other.HITLTools(nil)`, 0),
	)
})

// repositoryRoot walks up from this file to the directory holding go.mod.
func repositoryRoot() string {
	GinkgoHelper()

	_, file, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue())

	dir := filepath.Dir(file)
	for {
		_, err := os.Stat(filepath.Join(dir, "go.mod"))
		if err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		Expect(parent).NotTo(Equal(dir), "no go.mod above %s", file)
		dir = parent
	}
}

// modulePath reads the module line of the go.mod at root.
func modulePath(root string) string {
	GinkgoHelper()

	f, err := os.Open(filepath.Join(root, "go.mod"))
	Expect(err).NotTo(HaveOccurred())
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}

	Fail("go.mod has no module line")

	return ""
}

// walkGoFiles parses every non-test Go file under root, skipping the directories in
// skippedDirs and any whose name starts with a dot, and calls visit with each file's
// repository-relative path.
func walkGoFiles(root string, visit func(rel string, f *ast.File, fset *token.FileSet)) {
	GinkgoHelper()

	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := d.Name()
		if d.IsDir() {
			if p != root && (skippedDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(rel), f, fset)

		return nil
	})
	Expect(err).NotTo(HaveOccurred())
}

// declaresFunc reports whether f declares a top-level function of that name.
func declaresFunc(f *ast.File, name string) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return true
		}
	}

	return false
}

// sourceUses resolves each selector in f against its imports and returns those that
// resolve to a guarded source. An import with no alias is addressed by the last
// element of its path, which is the package name of every source package. A dot
// import of a source package is returned as a use on its own, since every guarded
// name in that package is then callable bare.
func sourceUses(module string, rel string, f *ast.File, fset *token.FileSet) []sourceUse {
	var uses []sourceUse

	imports := map[string]string{}
	for _, imp := range f.Imports {
		p := strings.TrimPrefix(strings.Trim(imp.Path.Value, `"`), module+"/")
		local := path.Base(p)
		if imp.Name != nil {
			local = imp.Name.Name
		}

		switch local {
		case "_":
		case ".":
			for _, g := range guardedSources {
				if g.pkg == p {
					uses = append(uses, sourceUse{source: g, pos: fmt.Sprintf("%s:%d", rel, fset.Position(imp.Pos()).Line)})
					break
				}
			}
		default:
			imports[local] = p
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}

		pkg, imported := imports[ident.Name]
		if !imported {
			return true
		}

		for _, g := range guardedSources {
			if g.pkg == pkg && g.name == sel.Sel.Name {
				uses = append(uses, sourceUse{source: g, pos: fmt.Sprintf("%s:%d", rel, fset.Position(sel.Pos()).Line)})
			}
		}

		return true
	})

	return uses
}
