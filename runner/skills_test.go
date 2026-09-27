// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocalSkillsSkipWhatIsNotASkill lists a personal skills folder that
// holds a loose file, a folder without SKILL.md, and a skill whose
// frontmatter has a line that is not a key: only the skill is indexed.
func TestLocalSkillsSkipWhatIsNotASkill(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"notes.txt":           "not a skill",
		"empty/README.md":     "no SKILL.md here",
		"release/SKILL.md":    "---\nname: release\n# a comment line\ndescription: Writes the release notes.\n---\nBody.\n",
		"release/template.md": "unrelated",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	skills, err := localSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "release" || skills[0].Description != "Writes the release notes." || skills[0].Path != filepath.Join(dir, "release", "SKILL.md") {
		t.Fatalf("skills %+v", skills)
	}
}
