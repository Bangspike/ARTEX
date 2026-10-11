package reanalysis

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotIdentityAndBoundary(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(filepath.Join(root, "app"), 0700)
	_ = os.WriteFile(filepath.Join(root, "app", "main.js"), []byte(`export const route="/api/login";`), 0600)
	path, sha, err := Snapshot(root, "app", t.TempDir())
	if err != nil || len(sha) != 64 {
		t.Fatalf("snapshot: %s %v", sha, err)
	}
	_, sha2, err := Snapshot(root, "app", t.TempDir())
	if err != nil || sha != sha2 {
		t.Fatalf("unstable manifest: %v", err)
	}
	_ = os.WriteFile(filepath.Join(root, "app", "main.js"), []byte("changed"), 0600)
	original, _ := os.ReadFile(filepath.Join(path, "main.js"))
	if string(original) == "changed" {
		t.Fatal("snapshot changed with original")
	}
	_, sha3, err := Snapshot(root, "app", t.TempDir())
	if err != nil || sha == sha3 {
		t.Fatal("content change did not change identity")
	}
	for _, p := range []string{"../escape", "/etc/passwd", "."} {
		if _, _, err = Snapshot(root, p, t.TempDir()); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	if _, _, err = Snapshot(root, "app", filepath.Join(root, "app", "nested", "output")); err == nil {
		t.Fatal("accepted a recursively included output directory")
	}
	_ = os.Symlink("main.js", filepath.Join(root, "app", "alias.js"))
	if _, _, err = Snapshot(root, "app", t.TempDir()); err == nil {
		t.Fatal("accepted symlink")
	}
}
