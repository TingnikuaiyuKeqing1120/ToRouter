package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompanionUsesActualLocalOrigin(t *testing.T) {
	for _, origin := range []string{"http://127.0.0.1:19191", "http://[::1]:19191", "http://localhost:19191/"} {
		target, err := companionURL(origin)
		if err != nil {
			t.Fatal(err)
		}
		if target[len(target)-6:] != "/#/pet" {
			t.Fatal(target)
		}
	}
	for _, origin := range []string{"https://example.com", "http://127.0.0.1:17878/?key=secret", "http://user:pass@127.0.0.1:17878", "file:///C:/secret", "http://127.0.0.1:17878/path"} {
		if _, err := companionURL(origin); err == nil {
			t.Errorf("accepted unsafe origin %q", origin)
		}
	}
}

func TestPetIdentitySeparatesDirectoriesAndNormalizesAliases(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "child", "..")
	if err := os.Mkdir(filepath.Join(dir, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if PetInstanceID(dir) != PetInstanceID(alias) {
		t.Fatal("clean path alias split pet ownership")
	}
	if runtime.GOOS == "windows" && PetInstanceID(dir) != PetInstanceID(strings.ToUpper(dir)) {
		t.Fatal("case alias split pet ownership")
	}
	other := t.TempDir()
	if PetWindowTitle("http://127.0.0.1:19191", dir) == PetWindowTitle("http://127.0.0.1:19191", other) {
		t.Fatal("reused port shares window ownership across data directories")
	}
	t.Run("symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(dir, link); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		if PetInstanceID(dir) != PetInstanceID(link) {
			t.Fatal("symlink alias split pet ownership")
		}
	})
}
