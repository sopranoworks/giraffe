// Package skill carries the giraffe skill's text. SKILL.md here is the
// one source: the binary embeds it for `giraffe install` and `giraffe
// update`, and the measurement harness copies the same file into its
// plugin directory (harness/sync-skill.sh), so the skill that is
// measured and the skill that ships cannot drift apart.
package skill

import _ "embed"

// Text is SKILL.md exactly as it is in the repository.
//
//go:embed SKILL.md
var Text []byte
