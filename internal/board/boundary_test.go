package board_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// crew-board reaches the fabric only through fabricclient, and the hub never
// reaches crew-board (docs/crew-board.md, Code and process boundary).
func TestImportBoundary(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	forbiddenForBoard := []string{"crew-services/internal/service", "crew-services/internal/store", "crew-services/internal/sqlite", "crew-services/internal/httpapi", "crew-services/internal/app", "crew-services/internal/config"}
	hub := []string{"internal/service", "internal/store", "internal/sqlite", "internal/httpapi", "internal/app", "internal/config", "cmd/crew-messaging"}
	check := func(dir string, bad func(string) bool) {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range f.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				if bad(imported) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s imports %s", rel, imported)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"internal/board", "cmd/crew-board"} {
		check(dir, func(p string) bool {
			for _, f := range forbiddenForBoard {
				if p == f {
					return true
				}
			}
			return false
		})
	}
	for _, dir := range hub {
		check(dir, func(p string) bool { return strings.HasPrefix(p, "crew-services/internal/board") })
	}
}
