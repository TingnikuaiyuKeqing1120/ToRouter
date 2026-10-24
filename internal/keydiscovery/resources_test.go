package keydiscovery

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func resourceTestDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("synthetic directory could not be canonicalized")
	}
	return path
}

// Only links between two checked children of this test's own directory are
// created. Junctions need no ordinary symlink privilege on Windows.
func resourceTestDirectoryLink(t *testing.T, base, link, target string) {
	t.Helper()
	if !withinResourceRoot(base, link) || !withinResourceRoot(base, target) || filepath.Clean(base) == filepath.Clean(link) || filepath.Clean(base) == filepath.Clean(target) {
		t.Fatal("synthetic directory link escaped its fixture")
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("synthetic link path already exists")
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatal("synthetic link target unavailable")
	}
	if runtime.GOOS == "windows" {
		command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:TOROUTER_FIXTURE_LINK -Target $env:TOROUTER_FIXTURE_TARGET | Out-Null`)
		command.Env = append(os.Environ(), "TOROUTER_FIXTURE_LINK="+link, "TOROUTER_FIXTURE_TARGET="+target)
		if err := command.Run(); err != nil {
			t.Fatal("synthetic directory junction creation failed")
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal("synthetic directory symlink creation failed")
	}
	t.Cleanup(func() {
		if !withinResourceRoot(base, link) {
			t.Error("synthetic link cleanup escaped fixture")
			return
		}
		// Remove the link itself; never recursively remove its target.
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("synthetic directory link cleanup failed")
		}
	})
}

func TestResourcesCanonicalGrantRejectsDirectoryRedirection(t *testing.T) {
	for _, ancestor := range []bool{false, true} {
		t.Run(map[bool]string{false: "root junction", true: "ancestor junction"}[ancestor], func(t *testing.T) {
			base := resourceTestDirectory(t)
			link, target, backup := filepath.Join(base, "authorized"), filepath.Join(base, "unauthorized"), filepath.Join(base, "original")
			root, targetData := link, target
			if ancestor {
				root = filepath.Join(root, "nested")
				targetData = filepath.Join(targetData, "nested")
			}
			for _, path := range []string{root, targetData} {
				if !withinResourceRoot(base, path) {
					t.Fatal("synthetic directory escaped fixture")
				}
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal("synthetic directory creation failed")
				}
			}
			data := []byte("synthetic-same-content-distinct-scope")
			resourceWrite(t, root, "settings.json", data)
			resourceWrite(t, targetData, "settings.json", data)
			source := Source{Files: []string{"settings.json"}}
			before := resourceFixture(t, root, source, nil)
			if _, err := before.Read(context.Background(), "settings.json"); err != nil {
				t.Fatal(err)
			}
			expected := before.Fingerprints()
			if err := before.Close(); err != nil {
				t.Fatal(err)
			}
			if !withinResourceRoot(base, link) || !withinResourceRoot(base, backup) {
				t.Fatal("synthetic rename escaped fixture")
			}
			if err := os.Rename(link, backup); err != nil {
				t.Fatal("synthetic authorized directory rename failed")
			}
			resourceTestDirectoryLink(t, base, link, target)
			linked, err := os.Readlink(link)
			if err != nil || filepath.Clean(linked) != filepath.Clean(target) {
				t.Fatal("synthetic directory redirection was not actually created")
			}
			opened, err := newResources(root, source, nil)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, ErrConflict) {
				t.Error("canonical grant silently followed a different directory")
			}
			if err = verifyFiles(context.Background(), root, source, expected, nil); !errors.Is(err, ErrConflict) {
				t.Error("equal file contents hid a redirected authorization root")
			}
		})
	}
}

func TestResourcesRootIdentityRemainsBoundAfterOpen(t *testing.T) {
	for _, duringOpen := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached read", true: "before constructor returns"}[duringOpen], func(t *testing.T) {
			base := resourceTestDirectory(t)
			root, target, backup := filepath.Join(base, "authorized"), filepath.Join(base, "unauthorized"), filepath.Join(base, "original")
			for _, path := range []string{root, target} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal("synthetic directory creation failed")
				}
			}
			resourceWrite(t, root, "settings.json", []byte("synthetic-original-material"))
			resourceWrite(t, target, "settings.json", []byte("synthetic-other-material"))
			redirected := false
			switchRoot := func() {
				if !withinResourceRoot(base, root) || !withinResourceRoot(base, backup) {
					t.Fatal("synthetic rename escaped fixture")
				}
				if err := os.Rename(root, backup); err != nil {
					if runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(5))) {
						t.Log("Windows open Root handle prevented directory replacement")
						return
					}
					t.Fatal("synthetic authorized directory rename failed")
				}
				resourceTestDirectoryLink(t, base, root, target)
				redirected = true
			}
			var guard func() error
			if duringOpen {
				calls := 0
				guard = func() error {
					calls++
					if calls == 2 {
						switchRoot()
					}
					return nil
				}
			}
			r, err := newResources(root, Source{Files: []string{"settings.json"}}, guard)
			if duringOpen {
				if runtime.GOOS == "windows" && !redirected {
					if err != nil || r == nil {
						t.Fatal("OS-preserved canonical root failed construction")
					}
					defer r.Close()
					if material, readErr := r.Read(context.Background(), "settings.json"); readErr != nil || string(material) != "synthetic-original-material" {
						t.Fatal("OS-preserved root changed material")
					}
					return
				}
				if r != nil {
					r.Close()
				}
				if !errors.Is(err, ErrConflict) {
					t.Fatal("constructor returned after its root identity changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			if _, err = r.Read(context.Background(), "settings.json"); err != nil {
				t.Fatal(err)
			}
			switchRoot()
			if runtime.GOOS == "windows" && !redirected {
				if material, readErr := r.Read(context.Background(), "settings.json"); readErr != nil || string(material) != "synthetic-original-material" {
					t.Fatal("OS-preserved cached root changed material")
				}
				return
			}
			if material, err := r.Read(context.Background(), "settings.json"); !errors.Is(err, ErrConflict) || material != nil {
				t.Fatal("cached material bypassed changed root identity")
			}
		})
	}
}

func resourceFixture(t *testing.T, root string, source Source, guard func() error) *resources {
	t.Helper()
	r, err := newResources(root, source, guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}
func resourceWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func resourceDatabase(t *testing.T, root, name string, wal bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if wal {
		if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("CREATE TABLE fixture (value TEXT, optional TEXT); INSERT INTO fixture VALUES ('synthetic-committed-value', NULL)"); err != nil {
		t.Fatal(err)
	}
	return db
}
func resourceSourceHashes(t *testing.T, root, name string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(root, name+suffix))
		if errors.Is(err, os.ErrNotExist) {
			values[name+suffix] = "missing"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		values[name+suffix] = resourceFingerprint(data)
	}
	return values
}

func TestResourcesDeclaredBoundedCacheMissingAndFingerprints(t *testing.T) {
	root := resourceTestDirectory(t)
	source := Source{Files: []string{"settings.json", "missing.json", "large.json", "directory.json"}}
	original := []byte(`{"key":"synthetic-private-key"}`)
	resourceWrite(t, root, "settings.json", original)
	r := resourceFixture(t, root, source, nil)
	ctx := context.Background()
	for _, name := range []string{"../settings.json", `C:\synthetic\settings.json`, `\\server\settings.json`, "undeclared.json"} {
		if _, err := r.Read(ctx, name); !errors.Is(err, ErrInvalid) {
			t.Fatal("undeclared path accepted")
		}
	}
	first, err := r.Read(ctx, "settings.json")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'X'
	resourceWrite(t, root, "settings.json", []byte("changed source"))
	cached, err := r.Read(ctx, "settings.json")
	if err != nil || !bytes.Equal(cached, original) {
		t.Fatal("batch snapshot or return-copy isolation failed")
	}
	if _, err = r.Read(ctx, "missing.json"); !errors.Is(err, ErrMissing) {
		t.Fatal("missing file was not reported")
	}
	expected := r.Fingerprints()
	if expected["missing.json"] != "missing" {
		t.Fatal("missing fingerprint not retained")
	}
	expected["settings.json"] = "tampered return map"
	if r.Fingerprints()["settings.json"] != resourceFingerprint(original) {
		t.Fatal("fingerprints exposed mutable state")
	}
	if err = verifyFiles(ctx, root, source, r.Fingerprints(), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("changed file did not invalidate preview", err)
	}
	// An absent configuration becoming available is also a source change.
	fresh := resourceFixture(t, root, Source{Files: []string{"appeared.json"}}, nil)
	if _, err = fresh.Read(ctx, "appeared.json"); !errors.Is(err, ErrMissing) {
		t.Fatal(err)
	}
	resourceWrite(t, root, "appeared.json", []byte("new material"))
	if err = verifyFiles(ctx, root, Source{Files: []string{"appeared.json"}}, fresh.Fingerprints(), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("new file did not invalidate missing snapshot", err)
	}
	if err = os.Mkdir(filepath.Join(root, "directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Read(ctx, "directory.json"); err == nil {
		t.Fatal("nonregular file accepted")
	}
	f, err := os.Create(filepath.Join(root, "large.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(resourceJSONLimit + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err = r.Read(ctx, "large.json"); err == nil {
		t.Fatal("oversized JSON accepted")
	}
}

func TestResourcesGuardAndCancellationNeverReturnMaterial(t *testing.T) {
	root := resourceTestDirectory(t)
	resourceWrite(t, root, "settings.json", []byte("synthetic-private-material"))
	source := Source{Files: []string{"settings.json"}}
	denied := errors.New("synthetic consent revoked")
	revoked := false
	r := resourceFixture(t, root, source, func() error {
		if revoked {
			return denied
		}
		return nil
	})
	if _, err := r.Read(context.Background(), "settings.json"); err != nil {
		t.Fatal(err)
	}
	revoked = true
	if data, err := r.Read(context.Background(), "settings.json"); !errors.Is(err, denied) || data != nil {
		t.Fatal("cached material bypassed revocation")
	}
	if err := verifyFiles(context.Background(), root, source, r.Fingerprints(), func() error { return denied }); !errors.Is(err, denied) {
		t.Fatal("verification bypassed revocation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	active := false
	other := resourceFixture(t, root, source, func() error {
		if active {
			calls++
			if calls == 3 {
				cancel()
			}
		}
		return nil
	})
	active = true
	if data, err := other.Read(ctx, "settings.json"); !errors.Is(err, context.Canceled) || data != nil {
		t.Fatal("cancelled read returned material", err)
	}
}

func TestResourcesRootAndSymlinkEscapeAreRefused(t *testing.T) {
	source := Source{Files: []string{"settings.json"}}
	for _, root := range []string{"relative", `\\server\private`, "//server/private"} {
		if _, err := newResources(root, source, nil); err == nil {
			t.Fatal("unbounded root accepted")
		}
	}
	for _, name := range []string{"../secret", "sub/settings.json", `sub\settings.json`, "C:secret"} {
		if _, err := newResources(resourceTestDirectory(t), Source{Files: []string{name}}, nil); err == nil {
			t.Fatal("path declaration accepted")
		}
	}
	t.Run("symlink escape", func(t *testing.T) {
		root, outside := resourceTestDirectory(t), resourceTestDirectory(t)
		resourceWrite(t, outside, "private.json", []byte("synthetic-outside-material"))
		if err := os.Symlink(filepath.Join(outside, "private.json"), filepath.Join(root, "settings.json")); err != nil {
			t.Skip("symlink creation unavailable")
		}
		r := resourceFixture(t, root, source, nil)
		data, err := r.Read(context.Background(), "settings.json")
		if err == nil || data != nil || strings.Contains(err.Error(), outside) {
			t.Fatal("symlink escaped root or path leaked")
		}
	})
}

func TestResourcesWALSnapshotIncludesCommittedOnlyAndSourceBytesRemainUntouched(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", true)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM fixture").Scan(&count); err != nil {
		t.Fatal(err)
	}
	before := resourceSourceHashes(t, root, "fixture.db")
	if before["fixture.db-wal"] == "missing" || before["fixture.db-shm"] == "missing" {
		t.Fatal("fixture is not an active WAL database")
	}
	r := resourceFixture(t, root, Source{Files: []string{"fixture.db"}}, nil)
	rows, err := r.Query(context.Background(), "fixture.db", "SELECT value, optional FROM fixture")
	if err != nil || !reflect.DeepEqual(rows, [][]string{{"synthetic-committed-value", ""}}) {
		t.Fatalf("WAL committed/null snapshot failed: %v", err)
	}
	path := r.snapshotRoot
	if path == "" || !filepath.IsAbs(path) || withinResourceRoot(root, path) {
		t.Fatal("snapshot not created in an isolated absolute directory")
	}
	if _, err = r.Query(context.Background(), "fixture.db", "SELECT count(*) FROM fixture"); err != nil {
		t.Fatal(err)
	}
	if r.snapshotRoot != path || len(r.databases) != 1 {
		t.Fatal("queries did not reuse the same snapshot")
	}
	fingerprints := r.Fingerprints()
	if fingerprints["fixture.db"] != before["fixture.db"] || fingerprints["fixture.db-wal"] != before["fixture.db-wal"] {
		t.Fatal("fingerprints did not describe copied source")
	}
	if _, exists := fingerprints["fixture.db-shm"]; exists {
		t.Fatal("source shared memory was treated as committed data")
	}
	if err = verifyFiles(context.Background(), root, Source{Files: []string{"fixture.db"}}, fingerprints, nil); err != nil {
		t.Fatal("unchanged WAL verification failed", err)
	}
	if after := resourceSourceHashes(t, root, "fixture.db"); !reflect.DeepEqual(before, after) {
		t.Fatal("source DB/WAL/SHM bytes changed")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private snapshot directory was not removed")
	}
	if after := resourceSourceHashes(t, root, "fixture.db"); !reflect.DeepEqual(before, after) {
		t.Fatal("closing snapshot touched source DB/WAL/SHM")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatal("source directory gained snapshot artifacts")
	}
}

func TestResourcesWALUncommittedWriterIsNotPartOfSnapshot(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", true)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal("fixture transaction failed")
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO fixture VALUES ('synthetic-uncommitted-value',NULL)"); err != nil {
		t.Fatal("fixture write failed")
	}
	r := resourceFixture(t, root, Source{Files: []string{"fixture.db"}}, nil)
	rows, err := r.Query(context.Background(), "fixture.db", "SELECT value FROM fixture")
	if err != nil || !reflect.DeepEqual(rows, [][]string{{"synthetic-committed-value"}}) {
		t.Fatal("snapshot included uncommitted data or failed", err)
	}
}

func TestResourcesDatabaseRemainsSameBatchAndVerificationDetectsWALCommit(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", true)
	source := Source{Files: []string{"fixture.db"}}
	r := resourceFixture(t, root, source, nil)
	if _, err := r.Query(context.Background(), "fixture.db", "SELECT value FROM fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO fixture VALUES ('synthetic-later-value',NULL)"); err != nil {
		t.Fatal(err)
	}
	rows, err := r.Query(context.Background(), "fixture.db", "SELECT count(*) FROM fixture")
	if err != nil || !reflect.DeepEqual(rows, [][]string{{"1"}}) {
		t.Fatal("later query switched snapshots", err)
	}
	if err = verifyFiles(context.Background(), root, source, r.Fingerprints(), nil); !errors.Is(err, ErrConflict) {
		t.Fatal("later WAL commit did not invalidate preview", err)
	}
}

func TestResourcesDatabaseChangesDuringCopyAreConflict(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", false)
	db.Close()
	calls := 0
	active := false
	r := resourceFixture(t, root, Source{Files: []string{"fixture.db"}}, func() error {
		if active {
			calls++
			if calls == 6 {
				data, err := os.ReadFile(filepath.Join(root, "fixture.db"))
				if err != nil {
					return err
				}
				data[len(data)-1] ^= 1
				return os.WriteFile(filepath.Join(root, "fixture.db"), data, 0600)
			}
		}
		return nil
	})
	active = true
	if rows, err := r.Query(context.Background(), "fixture.db", "SELECT value FROM fixture"); !errors.Is(err, ErrConflict) || rows != nil {
		t.Fatal("unstable copy did not fail closed", err)
	}
	if r.snapshotRoot != "" {
		t.Fatal("unstable source was opened as a snapshot")
	}
}

func TestResourcesSQLiteBoundsSQLPolicyAndControlledErrors(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", false)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for range 1001 {
		if _, err = tx.Exec("INSERT INTO fixture VALUES ('synthetic-row',NULL)"); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r := resourceFixture(t, root, Source{Files: []string{"fixture.db", "oversized.db"}}, nil)
	ctx := context.Background()
	for _, query := range []string{"DELETE FROM fixture", "PRAGMA wal_checkpoint", "ATTACH DATABASE 'synthetic-private-path' AS outside", "SELECT value FROM fixture; DELETE FROM fixture", "SELECT 1 -- comment"} {
		if rows, err := r.Query(ctx, "fixture.db", query); !errors.Is(err, ErrInvalid) || rows != nil {
			t.Fatal("non-SELECT/multiple SQL accepted")
		}
	}
	for _, query := range []string{"SELECT * FROM synthetic_secret_table", "SELECT value FROM fixture", "SELECT " + strings.Repeat("1,", 20) + "1", "SELECT zeroblob(8388609)", "SELECT zeroblob(6000000) UNION ALL SELECT zeroblob(6000000) UNION ALL SELECT zeroblob(6000000)"} {
		if rows, err := r.Query(ctx, "fixture.db", query); err == nil || rows != nil || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "synthetic_secret_table") {
			t.Fatal("query bounds or error privacy failed")
		}
	}
	if rows, err := r.Query(ctx, "fixture.db", "SELECT count(*) FROM fixture"); err != nil || rows[0][0] != "1002" {
		t.Fatal("rejected queries mutated snapshot", err)
	}
	f, err := os.Create(filepath.Join(root, "oversized.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(resourceDatabaseLimit + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if rows, err := r.Query(ctx, "oversized.db", "SELECT 1"); err == nil || rows != nil {
		t.Fatal("oversized DB accepted")
	}
	resourceWrite(t, root, "oversized.db", []byte("synthetic-not-a-database"))
	f, err = os.Create(filepath.Join(root, "oversized.db-wal"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(resourceDatabaseLimit + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if rows, err := r.Query(ctx, "oversized.db", "SELECT 1"); err == nil || rows != nil {
		t.Fatal("oversized WAL accepted")
	}
}

func TestResourcesSQLiteMissingTracksDBAndWAL(t *testing.T) {
	root := resourceTestDirectory(t)
	source := Source{Files: []string{"missing.db"}}
	r := resourceFixture(t, root, source, nil)
	if rows, err := r.Query(context.Background(), "missing.db", "SELECT 1"); !errors.Is(err, ErrMissing) || rows != nil {
		t.Fatal("missing DB not reported")
	}
	fingerprints := r.Fingerprints()
	if fingerprints["missing.db"] != "missing" || fingerprints["missing.db-wal"] != "missing" {
		t.Fatal("missing database pair not fingerprinted")
	}
	if err := verifyFiles(context.Background(), root, source, fingerprints, nil); err != nil {
		t.Fatal(err)
	}
	fingerprints["../outside"] = "missing"
	if err := verifyFiles(context.Background(), root, source, fingerprints, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("verification accepted undeclared path", err)
	}
}

func TestResourcesQueryFinalGuardCancellationAndCloseClearOwnedBuffers(t *testing.T) {
	root := resourceTestDirectory(t)
	db := resourceDatabase(t, root, "fixture.db", false)
	db.Close()
	resourceWrite(t, root, "settings.json", []byte("synthetic-owned-secret"))
	r := resourceFixture(t, root, Source{Files: []string{"fixture.db", "settings.json"}}, nil)
	ctx := context.Background()
	if _, err := r.Query(ctx, "fixture.db", "SELECT value FROM fixture"); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("synthetic revoked after query")
	calls := 0
	r.guard = func() error {
		calls++
		if calls == 3 {
			return denied
		}
		return nil
	}
	if rows, err := r.Query(ctx, "fixture.db", "SELECT value FROM fixture"); !errors.Is(err, denied) || rows != nil {
		t.Fatal("final guard returned query material", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	r.guard = func() error { cancel(); return nil }
	if rows, err := r.Query(cancelled, "fixture.db", "SELECT value FROM fixture"); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatal("cancelled query returned material", err)
	}
	r.guard = nil
	if _, err := r.Read(ctx, "settings.json"); err != nil {
		t.Fatal(err)
	}
	owned := r.files["settings.json"].data
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, value := range owned {
		if value != 0 {
			t.Fatal("Close retained owned secret buffers")
		}
	}
	if len(r.Fingerprints()) != 0 {
		t.Fatal("Close retained fingerprint state")
	}
	if _, err := r.Read(ctx, "settings.json"); !errors.Is(err, ErrInvalid) {
		t.Fatal("closed resources reopened source")
	}
}

func TestResourcesManagerExplicitDatabaseFilename(t *testing.T) {
	root := resourceTestDirectory(t)
	path := filepath.Join(root, "synthetic-manager.sqlite")
	t.Setenv("CODEXMANAGER_DB_PATH", path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE fixture (value TEXT); INSERT INTO fixture VALUES ('synthetic-value')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	source, _ := Lookup("source-codex-manager")
	r := resourceFixture(t, root, source, nil)
	rows, err := r.Query(context.Background(), "codexmanager.db", "SELECT value FROM fixture")
	if err != nil || len(rows) != 1 || rows[0][0] != "synthetic-value" {
		t.Fatal("explicit manager filename was ignored")
	}
	if err = verifyFiles(context.Background(), root, source, r.Fingerprints(), nil); err != nil {
		t.Fatal("alias fingerprint could not be rechecked")
	}
}
