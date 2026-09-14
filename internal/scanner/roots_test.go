package scanner

import (
	"strings"
	"testing"
)

func TestMarkerDirs_FindsMarkerAtRootAndNested(t *testing.T) {
	dirs := MarkerDirs([]string{"go.mod", "services/api/go.mod"})
	want := map[string]bool{".": true, "services/api": true}
	if len(dirs) != len(want) {
		t.Fatalf("MarkerDirs = %v, want keys of %v", dirs, want)
	}
	for _, d := range dirs {
		if !want[d] {
			t.Errorf("unexpected marker dir %q", d)
		}
	}
}

func TestMarkerDirs_NoMarkerAnywhere(t *testing.T) {
	dirs := MarkerDirs(nil)
	if len(dirs) != 0 {
		t.Fatalf("MarkerDirs = %v, want none", dirs)
	}
}

func TestIsMarker_GlobMarker(t *testing.T) {
	if !isMarker("App.csproj", markerNames) {
		t.Error("App.csproj should match the *.csproj marker")
	}
	if isMarker("notes.txt", markerNames) {
		t.Error("notes.txt should not be a marker")
	}
}

// TestIsMarker_ExtraMarkerFromAScope (directive 017 §3): a registered
// language module's own marker (go.mod today) is not in markerNames —
// it reaches isMarker only via the extra list a caller (mergedMarkers,
// MarkerFiles) merges in, the same way Scope.Markers does.
func TestIsMarker_ExtraMarkerFromAScope(t *testing.T) {
	if isMarker("go.mod", markerNames) {
		t.Error("go.mod should not be in scanner's own markerNames; it belongs to the golang module now")
	}
	if !isMarker("go.mod", append(append([]string(nil), markerNames...), "go.mod")) {
		t.Error("go.mod should be recognised once merged in as an extra marker")
	}
}

func TestOwningDir_NestedFileOwnedByNearestMarker(t *testing.T) {
	dirs := MarkerDirs([]string{"go.mod", "services/api/go.mod"})
	got := OwningDir("services/api/handler.go", dirs)
	if got != "services/api" {
		t.Errorf("OwningDir = %q, want %q (nearest, not the outer root)", got, "services/api")
	}
	got = OwningDir("main.go", dirs)
	if got != "." {
		t.Errorf("OwningDir = %q, want %q", got, ".")
	}
}

func TestOwningDir_FileUnderNoRoot(t *testing.T) {
	dirs := MarkerDirs([]string{"services/api/go.mod"})
	got := OwningDir("docs/readme.go", dirs)
	if got != "" {
		t.Errorf("OwningDir = %q, want \"\" (docs/ is outside every marker dir)", got)
	}
}

func TestProjectFor_UsesFallbackWhenNoMarkerOwnsTheFile(t *testing.T) {
	dirs := MarkerDirs([]string{"services/api/go.mod"})
	names := ProjectNames(dirs, "myrepo")
	got := ProjectFor("docs/readme.go", dirs, names, "myrepo")
	if got != "myrepo" {
		t.Errorf("ProjectFor = %q, want fallback %q", got, "myrepo")
	}
	got = ProjectFor("services/api/handler.go", dirs, names, "myrepo")
	if got != "api" {
		t.Errorf("ProjectFor = %q, want %q (nested marker dir's own basename)", got, "api")
	}
}

func TestCheckNameCollisions_NoCollision(t *testing.T) {
	names := map[string]string{"services/a/util": "a-util", "services/b/util": "b-util"}
	if err := CheckNameCollisions(names); err != nil {
		t.Fatalf("unexpected error for distinct names: %v", err)
	}
}

func TestCheckNameCollisions_TwoDirsSharingAName(t *testing.T) {
	names := map[string]string{"services/a/util": "util", "services/b/util": "util"}
	err := CheckNameCollisions(names)
	if err == nil {
		t.Fatal("expected an error for two dirs sharing the name \"util\"")
	}
	if !strings.Contains(err.Error(), "services/a/util") || !strings.Contains(err.Error(), "services/b/util") {
		t.Errorf("error = %v, want it to name both paths", err)
	}
	if !strings.Contains(err.Error(), `"util"`) {
		t.Errorf("error = %v, want it to name the colliding name", err)
	}
}

func TestCheckNameCollisions_Empty(t *testing.T) {
	if err := CheckNameCollisions(nil); err != nil {
		t.Fatalf("unexpected error for an empty map: %v", err)
	}
}

func TestProjectFor_RootMarkerUsesRepoBasename(t *testing.T) {
	dirs := MarkerDirs([]string{"go.mod"})
	names := ProjectNames(dirs, "myrepo")
	got := ProjectFor("main.go", dirs, names, "myrepo")
	if got != "myrepo" {
		t.Errorf("ProjectFor = %q, want %q (marker at root uses the scan root's own basename)", got, "myrepo")
	}
}
