package nudgeparity

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestComparatorProductionFilesImportOnlyStandardLibrary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate comparator package")
	}
	dir := filepath.Dir(currentFile)
	packages, err := parser.ParseDir(token.NewFileSet(), dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse comparator package: %v", err)
	}
	production := packages["nudgeparity"]
	if production == nil {
		t.Fatal("parsed comparator package is missing")
	}
	for filename, file := range production.Files {
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, filename, err)
			}
			if strings.Contains(path, ".") {
				t.Errorf("effect-free comparator production file %s imports non-standard package %q", filepath.Base(filename), path)
			}
		}
	}
}
