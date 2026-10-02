package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}

func TestWorktreesGetStableDistinctDatabaseNames(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	other := filepath.Join(base, "feature")
	if err := os.Mkdir(main, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, main, "init")
	runGit(t, main, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	m, err := Init(main, "postgres://user:secret@localhost:5432/source?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := os.Stat(statePath(m.CommonDir)); err != nil || mode.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v, %v", mode, err)
	}
	runGit(t, main, "worktree", "add", "-b", "feature", other)
	fromOther, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	if fromOther.CommonDir != m.CommonDir {
		t.Fatal("linked worktree did not find shared config")
	}
	mainDB, err := m.DatabaseName(main)
	if err != nil {
		t.Fatal(err)
	}
	featureDB, err := fromOther.DatabaseName(other)
	if err != nil {
		t.Fatal(err)
	}
	if mainDB == featureDB || !strings.HasPrefix(featureDB, "source_wt_") {
		t.Fatalf("unexpected database names: %s, %s", mainDB, featureDB)
	}
	if again, _ := m.DatabaseName(other); again != featureDB {
		t.Fatalf("database name changed between calls: %s, %s", featureDB, again)
	}
	url, err := fromOther.URL(other)
	if err != nil || !strings.Contains(url, "/"+featureDB+"?") {
		t.Fatalf("worktree URL: %s, %v", url, err)
	}
}

func TestEnvFileDoesNotOverwriteUserFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, EnvFile)
	if err := os.WriteFile(path, []byte("DATABASE_URL=user-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(root, "postgres://example/db"); err == nil {
		t.Fatal("expected existing user file to be protected")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "DATABASE_URL=user-value\n" {
		t.Fatalf("user file changed: %q, %v", data, err)
	}
}
