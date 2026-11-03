package humans_test

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

// crew-humans stands alone: it imports no other crew service, and the others
// reach it only over HTTP through humansclient (docs/crew-humans.md,
// Boundary).
func TestImportBoundary(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
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
	own := func(p string) bool {
		return p == "crew-services/internal/humans" || strings.HasPrefix(p, "crew-services/internal/humans/")
	}
	for _, dir := range []string{"internal/humans", "cmd/crew-humans"} {
		check(dir, func(p string) bool { return strings.HasPrefix(p, "crew-services/") && !own(p) })
	}
	check("internal/humansclient", func(p string) bool { return strings.HasPrefix(p, "crew-services/") })
	for _, dir := range []string{"internal/board", "cmd/crew-board", "internal/service", "internal/store", "internal/sqlite", "internal/httpapi", "internal/app", "cmd/crew-messaging", "internal/review", "cmd/crew-review"} {
		check(dir, own)
	}
	for _, dir := range []string{"internal/service", "internal/store", "internal/sqlite", "internal/httpapi", "internal/app", "cmd/crew-messaging"} {
		check(dir, func(p string) bool { return p == "crew-services/internal/humansclient" })
	}
}
