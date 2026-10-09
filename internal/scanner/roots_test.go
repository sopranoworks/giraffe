package scanner

import "testing"

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
	if got != "services/api" {
		t.Errorf("ProjectFor = %q, want %q (nested marker dir's own path)", got, "services/api")
	}
}

// Directive 077: two nested directories with the same basename are two
// projects, each named by its path — what 003 made an error.
func TestProjectNames_SameBasenameTwoProjects(t *testing.T) {
	dirs := MarkerDirs([]string{"gcp/observability/go.mod", "interop/observability/go.mod", "client/pkg/go.mod", "pkg/go.mod", "go.mod"})
	names := ProjectNames(dirs, "grpc-go")
	want := map[string]string{"gcp/observability": "gcp/observability", "interop/observability": "interop/observability", "client/pkg": "client/pkg", "pkg": "pkg", ".": "grpc-go"}
	for d, w := range want {
		if names[d] != w {
			t.Errorf("ProjectNames[%q] = %q, want %q", d, names[d], w)
		}
	}
	if got := ProjectFor("gcp/observability/x.go", dirs, names, "grpc-go"); got != "gcp/observability" {
		t.Errorf("ProjectFor = %q, want gcp/observability", got)
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
