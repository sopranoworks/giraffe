package lang

import "testing"

func TestFor_KnownAndUnknownExtension(t *testing.T) {
	if _, ok := For(".go"); !ok {
		t.Error("expected .go to have a registered extractor")
	}
	if _, ok := For(".cpp"); ok {
		t.Error("expected .cpp to have no registered extractor in this version")
	}
}

// TestNameFor_AbsentWithoutRegistrationOrAssignment (§25 C-24): a file
// with no registered extractor and no operator assignment gets "" —
// absence, not a placeholder — so it is never special-cased downstream
// as a fake language.
func TestNameFor_AbsentWithoutRegistrationOrAssignment(t *testing.T) {
	if got := NameFor(".go", nil); got != "go" {
		t.Errorf("NameFor(.go, nil) = %q, want %q", got, "go")
	}
	if got := NameFor(".cpp", nil); got != "" {
		t.Errorf("NameFor(.cpp, nil) = %q, want absent", got)
	}
}

// TestNameFor_OperatorAssignmentFillsTheGap (§25 C-26): an extension
// with no registered extractor still gets a lang value when the
// operator assigned one in .giraffe/exclude.
func TestNameFor_OperatorAssignmentFillsTheGap(t *testing.T) {
	assignments := map[string]string{".inc": "c"}
	if got := NameFor(".inc", assignments); got != "c" {
		t.Errorf("NameFor(.inc, assignments) = %q, want %q", got, "c")
	}
	if got := NameFor(".hpp", assignments); got != "" {
		t.Errorf("NameFor(.hpp, assignments) = %q, want absent (unassigned)", got)
	}
}

// TestNameFor_RegistrationWinsOverAssignment (§25 C-26): an assignment
// exists to label what giraffe cannot otherwise parse, not to override
// a real extractor's own name for its own extension.
func TestNameFor_RegistrationWinsOverAssignment(t *testing.T) {
	assignments := map[string]string{".go": "not-go"}
	if got := NameFor(".go", assignments); got != "go" {
		t.Errorf("NameFor(.go, assignments) = %q, want %q (registration wins)", got, "go")
	}
}
