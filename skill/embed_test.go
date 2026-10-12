package skill

import (
	"bytes"
	"os"
	"testing"
)

// The embedded text is the file in the repository, and it is a skill
// named giraffe (directive 087 §2).
func TestText_IsSKILLmd(t *testing.T) {
	disk, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk, Text) {
		t.Fatal("embedded text differs from SKILL.md")
	}
	if !bytes.HasPrefix(Text, []byte("---\nname: giraffe\n")) {
		t.Fatalf("SKILL.md does not open with the giraffe frontmatter: %q", Text[:40])
	}
}
