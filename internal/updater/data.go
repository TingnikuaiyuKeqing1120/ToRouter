package updater

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var dataNames = []string{"router.db", "master.key", "router.db-wal", "router.db-shm"}

func snapshotDir(p Plan) string { return filepath.Join(p.Options.DataDir, ".update-snapshot-"+p.Token) }

func removeSnapshot(p Plan) {
	for _, name := range dataNames {
		if name != "router.db-shm" {
			_ = os.Remove(filepath.Join(snapshotDir(p), name))
		}
	}
	_ = os.Remove(snapshotDir(p))
}

func closeFiles(files map[string]*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func lockDataFiles(dir string) (map[string]*os.File, error) {
	files := make(map[string]*os.File)
	for _, name := range dataNames {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			closeFiles(files)
			return nil, err
		}
		if !info.Mode().IsRegular() {
			closeFiles(files)
			return nil, fmt.Errorf("data file %s is not a regular file", name)
		}
		f, err := openExclusive(path)
		if err != nil {
			closeFiles(files)
			return nil, fmt.Errorf("data file %s is in use or inaccessible", name)
		}
		files[name] = f
	}
	return files, nil
}

// snapshotData runs only after the old Store and all its writers are closed.
// It snapshots exact SQLite/key files rather than treating application-level
// configuration exports as a migration rollback.
func snapshotData(p Plan) (map[string]bool, error) {
	files, err := lockDataFiles(p.Options.DataDir)
	if err != nil {
		return nil, err
	}
	defer closeFiles(files)
	dir := snapshotDir(p)
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	present := make(map[string]bool)
	for _, name := range dataNames {
		if name == "router.db-shm" {
			continue
		}
		f := files[name]
		if f == nil {
			continue
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(out, f)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		present[name] = true
	}
	return present, nil
}

func restoreData(p Plan, present map[string]bool) error {
	files, err := lockDataFiles(p.Options.DataDir)
	if err != nil {
		return err
	}
	defer closeFiles(files)
	// Keep exclusive handles until all original bytes have been restored. The
	// launch gate prevents new opens while the handles for deleted files close.
	for _, name := range dataNames {
		f := files[name]
		if !present[name] {
			if f != nil {
				f.Close()
				delete(files, name)
				if err = os.Remove(filepath.Join(p.Options.DataDir, name)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			continue
		}
		if f == nil {
			f, err = os.OpenFile(filepath.Join(p.Options.DataDir, name), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			files[name] = f
		}
		in, err := os.Open(filepath.Join(snapshotDir(p), name))
		if err != nil {
			return err
		}
		if err = f.Truncate(0); err == nil {
			_, err = f.Seek(0, io.SeekStart)
		}
		if err == nil {
			_, err = io.Copy(f, in)
		}
		in.Close()
		if err == nil {
			err = f.Sync()
		}
		if err != nil {
			return errors.New("could not restore exact data snapshot")
		}
	}
	return nil
}
