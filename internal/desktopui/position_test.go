package desktopui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPetPositionCacheAndInstallationIsolation(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if _, ok := readPetPosition(dir); ok {
		t.Fatal("missing cache was treated as position")
	}
	want := petPosition{-880, -320}
	if err := writePetPosition(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, ok := readPetPosition(dir); !ok || got != want {
		t.Fatalf("cache lost physical position: %+v %v", got, ok)
	}
	if _, ok := readPetPosition(other); ok {
		t.Fatal("different installation shared position")
	}
	want = petPosition{920, 800}
	if err := writePetPosition(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, ok := readPetPosition(dir); !ok || got != want {
		t.Fatal("atomic replacement did not retain last position")
	}
	for _, bad := range []string{`{}`, `{"left":0}`, `{"left":null,"bottom":0}`, `{"left":1.5,"bottom":5}`, `{"left":1,"bottom":2} {}`, `{"left":2147483648,"bottom":5}`, `broken`} {
		if err := os.WriteFile(filepath.Join(dir, petPositionFile), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readPetPosition(dir); ok {
			t.Fatalf("invalid cache accepted: %s", bad)
		}
	}
}

func TestPositionAnchorIgnoresExpansionClampAndIncludesDragging(t *testing.T) {
	initial := windowRect{-1000, -1050, -740, -850}
	work := windowRect{-1920, -1080, 0, 0}
	var anchor petAnchor
	expanded := anchor.resize(initial, work, 260, 350)
	if got := anchor.position(expanded); got != (petPosition{-1000, -850}) {
		t.Fatalf("expanded dock changed remembered point: %+v", got)
	}
	expanded.left += 60
	expanded.right += 60
	expanded.top += 80
	expanded.bottom += 80
	if got := anchor.position(expanded); got != (petPosition{-940, -770}) {
		t.Fatalf("drag was not incorporated: %+v", got)
	}
	shrunk := anchor.resize(expanded, work, 260, 200)
	if got := anchor.position(shrunk); got != (petPosition{-940, -770}) {
		t.Fatalf("repeated resize drifted: %+v", got)
	}
}
