package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"torouter/internal/buildinfo"
)

type processIdentity struct {
	PID     int    `json:"pid"`
	Created uint64 `json:"created"`
}

type lockRecord struct {
	Token    string          `json:"token"`
	PlanPath string          `json:"plan_path"`
	Owner    processIdentity `json:"owner"`
}

// The journal is durable before each destructive operation. An unregistered
// child cannot pass CheckLaunch, closing the cmd.Start/PID-record crash window.
type journal struct {
	Phase              string          `json:"phase"`
	Snapshot           map[string]bool `json:"snapshot,omitempty"`
	Child              processIdentity `json:"child"`
	Outcome            string          `json:"outcome,omitempty"`
	ExecutableRestored bool            `json:"executable_restored,omitempty"`
	DataRestored       bool            `json:"data_restored,omitempty"`
}

func readJSONFile(path string, out any, limit int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("update transaction metadata too large")
	}
	return decodeStrict(b, out)
}
func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	f, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return atomicReplace(temporary, path)
}
func journalPath(p Plan) string            { return filepath.Join(p.Prepared.WorkDir, "journal.json") }
func writeJournal(p Plan, j journal) error { return atomicJSON(journalPath(p), j) }
func readJournal(p Plan) (journal, error) {
	var j journal
	err := readJSONFile(journalPath(p), &j, 16<<10)
	return j, err
}
func readLock(path string) (lockRecord, error) {
	var record lockRecord
	err := readJSONFile(path, &record, 4096)
	return record, err
}

func publishLock(path string, record lockRecord) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary := path + "." + record.Token + ".tmp"
	if err = writeExclusive(temporary, b, 0600); err != nil {
		return err
	}
	defer os.Remove(temporary)
	return atomicPublish(temporary, path)
}

func loadRecoveryPlan(record lockRecord, dataDir string) (Plan, error) {
	var p Plan
	if !filepath.IsAbs(record.PlanPath) {
		return p, errors.New("invalid update recovery plan path")
	}
	if err := readJSONFile(record.PlanPath, &p, MaxManifestSize*2); err != nil {
		return p, err
	}
	if err := validatePlanPaths(p); err != nil {
		return p, err
	}
	actualDir, dirErr := os.Stat(dataDir)
	plannedDir, plannedErr := os.Stat(p.Options.DataDir)
	if record.PlanPath != filepath.Join(p.Prepared.WorkDir, "plan.json") || record.Token != p.Token || dirErr != nil || plannedErr != nil || !os.SameFile(actualDir, plannedDir) {
		return p, errors.New("update recovery plan does not own this data directory")
	}
	key, err := ParsePublicKey(buildinfo.UpdatePublicKey)
	if err != nil {
		return p, err
	}
	release, err := VerifyManifest(p.Prepared.Manifest, key)
	if err != nil {
		return p, err
	}
	if release != p.Prepared.Release {
		return p, errors.New("recovery release differs from signed manifest")
	}
	return p, nil
}

// checkLaunch serializes recovery with a live helper using an OS-held guard.
// The persistent guard filename is harmless: process death releases its handle.
func checkLaunch(dataDir string) error {
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return errors.New("could not resolve application data directory")
	}
	record, err := readLock(filepath.Join(dataDir, ".update-lock"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("invalid interrupted update record; recovery required")
	}
	if record.Token != "" && record.Token == os.Getenv(updateTokenEnv) {
		self, err := identifyProcess(os.Getpid())
		if err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			var p Plan
			if err = readJSONFile(record.PlanPath, &p, MaxManifestSize*2); err != nil {
				return errors.New("update child has no valid transaction plan")
			}
			j, e := readJournal(p)
			if e == nil && j.Child == self {
				return nil
			}
			if time.Now().After(deadline) {
				return errors.New("update child was not registered by its helper")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	guard, err := acquireGuard(dataDir)
	if err != nil {
		return errors.New("application update is in progress")
	}
	defer guard.Close()
	// Re-read after acquiring the guard: a completing helper may have removed it.
	record, err = readLock(filepath.Join(dataDir, ".update-lock"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("invalid interrupted update record; recovery required")
	}
	alive, err := identityAlive(record.Owner)
	if err != nil {
		return errors.New("cannot establish update owner identity")
	}
	if alive {
		return errors.New("application update is in progress")
	}
	p, err := loadRecoveryPlan(record, dataDir)
	if err != nil {
		return errors.New("interrupted update plan could not be verified; recovery artifacts retained")
	}
	j, err := readJournal(p)
	if err != nil {
		return errors.New("interrupted update journal could not be verified; recovery artifacts retained")
	}
	if j.Phase == "prepared" || j.Phase == "restored" || j.Phase == "completed" {
		if j.Phase == "completed" {
			if err = finishCompleted(p, j); err != nil {
				return err
			}
		} else {
			hash, e := HashFile(p.Prepared.TargetPath)
			if e != nil || hash != p.Prepared.CurrentSHA256 {
				return errors.New("interrupted update executable needs recovery")
			}
		}
		if j.Phase == "prepared" {
			removeSnapshot(p)
		}
		releaseLocks(p)
		return nil
	}
	if j.Child.PID != 0 {
		alive, e := identityAlive(j.Child)
		if e == nil && alive && j.Phase == "replaced" {
			process, findErr := os.FindProcess(j.Child.PID)
			if findErr == nil {
				probeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				probeErr := waitReady(probeCtx, &exec.Cmd{Process: process}, p)
				cancel()
				_ = process.Release()
				if probeErr == nil {
					j.Phase = "completed"
					j.Outcome = "updated"
					if err = writeJournal(p, j); err != nil {
						return err
					}
					if err = finishCompleted(p, j); err != nil {
						return err
					}
					releaseLocks(p)
					return nil
				}
			}
		}
		if e != nil || alive {
			return errors.New("interrupted update backend is still active; stop it before recovery")
		}
	}
	helper := filepath.Join(p.Prepared.WorkDir, "helper.exe")
	hash, err := HashFile(helper)
	if err != nil || hash != p.Prepared.CurrentSHA256 {
		return errors.New("interrupted update helper failed integrity validation; recovery artifacts retained")
	}
	// The original helper is a separate executable, so Windows can replace a
	// target currently displaying this startup error after that process exits.
	cmd := exec.Command(helper, HelperArgument, record.PlanPath)
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return errors.New("could not start interrupted update recovery")
	}
	_ = cmd.Process.Release()
	return errors.New("recovering interrupted update; the previous version will restart")
}

func trackedStart(p Plan, h helperHooks, j *journal) (*exec.Cmd, error) {
	j.Child = processIdentity{}
	if err := writeJournal(p, *j); err != nil {
		return nil, err
	}
	cmd, err := h.start(p)
	if err != nil {
		return cmd, err
	}
	if cmd != nil && cmd.Process != nil {
		identity, e := identifyProcess(cmd.Process.Pid)
		if e != nil {
			return cmd, e
		}
		j.Child = identity
		if e = writeJournal(p, *j); e != nil {
			return cmd, e
		}
	}
	return cmd, nil
}

// recoverInterrupted runs under the same guard as normal replacement. It never
// kills a process on the basis of a persisted PID, even if the PID was reused.
func recoverInterrupted(ctx context.Context, p Plan, j journal, h helperHooks) error {
	if j.Phase == "completed" {
		if err := finishCompleted(p, j); err != nil {
			return err
		}
		releaseLocks(p)
		return nil
	}
	if j.Child.PID != 0 {
		alive, err := identityAlive(j.Child)
		if err != nil || alive {
			return errors.New("interrupted update backend is still active; no data recovery attempted")
		}
	}
	files, err := lockDataFiles(p.Options.DataDir)
	if err != nil {
		return errors.New("interrupted update data remains in use; no recovery attempted")
	}
	closeFiles(files)
	backup := filepath.Join(p.Prepared.WorkDir, "previous.exe")
	current, _ := HashFile(p.Prepared.TargetPath)
	if current != p.Prepared.CurrentSHA256 {
		hash, e := HashFile(backup)
		if e != nil || hash != p.Prepared.CurrentSHA256 {
			return errors.New("interrupted update backup failed integrity validation")
		}
		if err = replaceWithRetry(ctx, backup, p.Prepared.TargetPath); err != nil {
			return err
		}
	}
	if j.Phase != "prepared" && j.Phase != "restored" {
		if j.Snapshot == nil {
			return errors.New("interrupted update has no complete data snapshot")
		}
		if err = validateSnapshot(p, j.Snapshot); err != nil {
			return err
		}
		j.Phase = "restoring"
		if err = writeJournal(p, j); err != nil {
			return err
		}
		if err = restoreData(p, j.Snapshot); err != nil {
			return err
		}
	}
	j.Phase = "restored"
	if err = writeJournal(p, j); err != nil {
		return err
	}
	old := p
	old.Prepared.Release.Version = p.Prepared.CurrentVersion
	cmd, startErr := trackedStart(old, h, &j)
	if startErr == nil {
		readyCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		startErr = h.ready(readyCtx, cmd, old)
		cancel()
	}
	if startErr != nil {
		if cmd != nil {
			startErr = errors.Join(startErr, h.stop(cmd))
		}
		return startErr
	}
	j.Phase = "completed"
	j.Outcome = "rolled_back"
	j.ExecutableRestored = true
	j.DataRestored = true
	if err = writeJournal(p, j); err != nil {
		return err
	}
	result := Result{Version: p.Prepared.Release.Version, State: "rolled_back", ExecutableRestored: true, DataRestored: true, Error: "interrupted update recovered to previous version"}
	releaseLocks(p)
	if h.view != nil {
		result.ViewError = ErrorMessage(h.view(p))
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return atomicJSON(p.Prepared.TargetPath+".update-result.json", result)
}

func finishCompleted(p Plan, j journal) error {
	result := Result{Version: p.Prepared.Release.Version, State: j.Outcome}
	switch j.Outcome {
	case "updated":
		if err := verifyArtifact(p.Prepared.TargetPath, p.Prepared.Release); err != nil {
			return errors.New("completed update executable failed validation")
		}
		removeSnapshot(p)
	case "rolled_back":
		hash, err := HashFile(p.Prepared.TargetPath)
		if err != nil || hash != p.Prepared.CurrentSHA256 {
			return errors.New("completed rollback executable failed validation")
		}
		result.ExecutableRestored = j.ExecutableRestored
		result.DataRestored = j.DataRestored
	default:
		return errors.New("invalid completed update outcome")
	}
	return atomicJSON(p.Prepared.TargetPath+".update-result.json", result)
}

func restartUntouchedOriginal(ctx context.Context, p Plan, j journal, h helperHooks, reason error) (Result, bool, error) {
	result := Result{Version: p.Prepared.Release.Version, State: "failed", Error: ErrorMessage(reason)}
	if hash, err := HashFile(p.Prepared.TargetPath); err != nil || hash != p.Prepared.CurrentSHA256 {
		return result, false, nil
	}
	files, err := lockDataFiles(p.Options.DataDir)
	if err != nil {
		return result, false, nil
	}
	closeFiles(files)
	j.Phase = "restored"
	j.Child = processIdentity{}
	// No new code has run and the original bytes are still installed. Dropping
	// gates is safe here and lets the old backend start even if a full disk
	// prevents another small journal write.
	releaseLocks(p)
	_ = writeJournal(p, j)
	old := p
	old.Prepared.Release.Version = p.Prepared.CurrentVersion
	cmd, err := h.start(old)
	if err == nil {
		readyCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		err = h.ready(readyCtx, cmd, old)
		cancel()
	}
	if err != nil {
		if cmd != nil {
			err = errors.Join(err, h.stop(cmd))
		}
		return result, false, err
	}
	j.Phase = "completed"
	j.Outcome = "rolled_back"
	_ = writeJournal(p, j)
	result.State = "rolled_back" // Neither EXE nor data required restoration.
	removeSnapshot(p)
	releaseLocks(p)
	if h.view != nil {
		result.ViewError = ErrorMessage(h.view(p))
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return result, true, nil
}

func validateSnapshot(p Plan, present map[string]bool) error {
	for _, name := range dataNames {
		if !present[name] {
			continue
		}
		if name == "router.db-shm" {
			return errors.New("invalid persistent data snapshot")
		}
		info, err := os.Lstat(filepath.Join(snapshotDir(p), name))
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("data snapshot is missing or invalid; recovery artifacts retained")
		}
	}
	return nil
}
