package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPaseoSkillMountIsReadOnly(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not available")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	private := filepath.Join(root, "private-home")
	project := filepath.Join(root, "project")
	skill := filepath.Join(root, "installed-skill")
	for _, p := range []string{home, private, project, skill} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("skill-content"), 0600)
	os.WriteFile(filepath.Join(home, "secret"), []byte("hidden"), 0600)
	target := filepath.Join(home, ".claude", "skills", "paseo")
	cmd, err := (Bwrap{}).Command(Spec{Home: home, SandboxHome: private, Project: project, Exe: "/bin/true", Env: []string{"HOME=" + home}, ReadOnlyMounts: []ReadOnlyMount{{Source: skill, Target: target}}})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args[:len(cmd.Args)-2], "/bin/bash", "-c", `test "$(cat "$HOME/.claude/skills/paseo/SKILL.md")" = skill-content && test ! -e "$HOME/secret" && ! printf bad > "$HOME/.claude/skills/paseo/SKILL.md"`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("skill mount: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(skill, "SKILL.md"))
	if string(data) != "skill-content" {
		t.Fatal("host skill modified")
	}
}
