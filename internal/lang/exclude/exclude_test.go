package exclude

import (
	"testing"

	"giraffe/internal/scanner"
)

// TestExtract_ClaimsADeniedExtension (moved from
// scanner_test.go's former TestList_DeniesBinaryExtensions_..., §24
// C-19, directive 017 §2): a binary/archive extension is claimed —
// handled=true, no entries — decided from the path alone.
func TestExtract_ClaimsADeniedExtension(t *testing.T) {
	f := scanner.WithContent("logo.png", []byte("\x89PNG"))
	entries, handled, err := Extract(f, "giraffe", nil)
	if !handled || entries != nil || err != nil {
		t.Errorf("Extract(logo.png) = %v, %v, %v, want nil, true, nil", entries, handled, err)
	}
}

// TestExtract_DeclinesAnUnknownExtension: anything not on the deny
// list, including a language giraffe has no extractor for, is left for
// a later module to answer.
func TestExtract_DeclinesAnUnknownExtension(t *testing.T) {
	f := scanner.WithContent("widget.cpp", []byte("int main() {}\n"))
	entries, handled, err := Extract(f, "giraffe", nil)
	if handled || entries != nil || err != nil {
		t.Errorf("Extract(widget.cpp) = %v, %v, %v, want nil, false, nil", entries, handled, err)
	}
}

// TestExtract_ClaimsAnUnknownExtensionlessFile: an extensionless file
// not on knownExtensionless has exactly the shape of a compiled
// binary, so it is claimed (excluded) by default.
func TestExtract_ClaimsAnUnknownExtensionlessFile(t *testing.T) {
	f := scanner.WithContent("configure", []byte("#!/bin/sh\n"))
	entries, handled, err := Extract(f, "giraffe", nil)
	if !handled || entries != nil || err != nil {
		t.Errorf("Extract(configure) = %v, %v, %v, want nil, true, nil", entries, handled, err)
	}
}

// TestExtract_DeclinesAKnownExtensionlessFile: a build/doc file whose
// name alone says what it is (Makefile, CMakeLists.txt despite its
// ".txt" extension) is left for a later module to produce a file entry
// for.
func TestExtract_DeclinesAKnownExtensionlessFile(t *testing.T) {
	for _, path := range []string{"Makefile", "CMakeLists.txt"} {
		f := scanner.WithContent(path, []byte("x"))
		entries, handled, err := Extract(f, "giraffe", nil)
		if handled || entries != nil || err != nil {
			t.Errorf("Extract(%s) = %v, %v, %v, want nil, false, nil", path, entries, handled, err)
		}
	}
}

// TestExtract_NeverReadsContent (directive 017 §1, §2): the exclusion
// decision is made from f.Path alone — scanner.Open's read is lazy, so
// a path that does not exist on disk still resolves without error for
// both a claimed and a declined extension.
func TestExtract_NeverReadsContent(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"missing.png", "missing.cpp"} {
		f := scanner.Open(dir, scanner.FileMeta{Path: path})
		if _, _, err := Extract(f, "giraffe", nil); err != nil {
			t.Errorf("Extract(%s) returned an error, meaning content was read: %v", path, err)
		}
	}
}
