package main

import (
	"strings"
	"testing"

	"giraffe/internal/index"
)

// Directive 077: two configured roots with the same project name no
// longer stop the build; the later one is named by its parent
// directories, and ## warnings says so.
func TestRootLabels_SameNameRootsAreQualified(t *testing.T) {
	results := []rootResult{
		{root: "/src/a/util", stats: index.Stats{RootProject: "util"}},
		{root: "/src/b/util", stats: index.Stats{RootProject: "util"}},
		{root: "/src/other", stats: index.Stats{RootProject: "other"}},
	}
	labels, notices := rootLabels(results)
	if strings.Join(labels, ",") != "util,b/util,other" {
		t.Errorf("labels = %v, want util, b/util, other", labels)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "/src/a/util") || !strings.Contains(notices[0], "project:b/util") {
		t.Errorf("notices = %v, want one naming both roots and project:b/util", notices)
	}
}
