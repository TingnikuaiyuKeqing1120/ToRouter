package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"torouter/internal/buildinfo"
)

const HelperArgument = "--internal-update"
const updateTokenEnv = "TO_ROUTER_UPDATE_TOKEN"

type HelperOptions struct {
	DataDir   string
	Port      int
	ParentPID int
	WaitPIDs  []int
	StartArgs []string
	OpenMode  string
	OpenPet   bool
}

type Plan struct {
	Prepared Prepared      `json:"prepared"`
	Options  HelperOptions `json:"options"`
	Token    string        `json:"token"`
}

type Result struct {
	Version            string `json:"version"`
	State              string `json:"state"`
	Error              string `json:"error,omitempty"`
	ExecutableRestored bool   `json:"executable_restored"`
	DataRestored       bool   `json:"data_restored"`
	ViewError          string `json:"view_error,omitempty"`
}

// CheckLaunch must run before opening the Store. Only the backend started by
// the helper may open the data directory while replacement is in progress.
func CheckLaunch(dataDir string) error {
	return checkLaunch(dataDir)
}

func targetLock(p Plan) string { return p.Prepared.TargetPath + ".update-lock" }
func dataLock(p Plan) string   { return filepath.Join(p.Options.DataDir, ".update-lock") }

// LaunchHelper runs a temporary copy of this executable. A successful return
// means the helper started; the caller must then drain, close Store, and exit.
func LaunchHelper(prepared *Prepared, options HelperOptions) (int, error) {
	if prepared == nil {
		return 0, errors.New("missing prepared update")
	}
	if options.DataDir == "" {
		return 0, errors.New("missing application data directory")
	}
	var pathErr error
	options.DataDir, pathErr = filepath.Abs(options.DataDir)
	if pathErr != nil {
		return 0, errors.New("could not resolve application data directory")
	}
	if !filepath.IsAbs(options.DataDir) || options.Port < 1 || options.Port > 65535 || options.ParentPID <= 0 {
		return 0, errors.New("invalid helper options")
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	p := Plan{*clonePrepared(prepared), options, hex.EncodeToString(nonce)}
	if err := validatePlanPaths(p); err != nil {
		return 0, err
	}
	current, err := HashFile(p.Prepared.TargetPath)
	if err != nil {
		return 0, err
	}
	if current != p.Prepared.CurrentSHA256 {
		return 0, errors.New("current executable changed after download")
	}
	key, err := ParsePublicKey(buildinfo.UpdatePublicKey)
	if err != nil {
		return 0, err
	}
	release, err := VerifyManifest(p.Prepared.Manifest, key)
	if err != nil {
		return 0, err
	}
	if release != p.Prepared.Release || p.Prepared.CurrentVersion != buildinfo.Version {
		return 0, errors.New("prepared update does not match this build and signed release")
	}
	if err = verifyArtifact(p.Prepared.StagedPath, release); err != nil {
		return 0, err
	}
	guard, err := acquireGuard(options.DataDir)
	if err != nil {
		return 0, errors.New("another update transaction is active")
	}
	defer guard.Close()
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		return 0, err
	}
	planPath := filepath.Join(p.Prepared.WorkDir, "plan.json")
	record := lockRecord{Token: p.Token, PlanPath: planPath, Owner: owner}
	helper := filepath.Join(p.Prepared.WorkDir, "helper.exe")
	cleanup := func() { releaseLocks(p); _ = os.Remove(helper); _ = os.Remove(planPath) }
	if err := copyFile(p.Prepared.TargetPath, helper, 0700); err != nil {
		cleanup()
		return 0, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		cleanup()
		return 0, err
	}
	if err = writeExclusive(planPath, b, 0600); err != nil {
		cleanup()
		return 0, err
	}
	if err = writeJournal(p, journal{Phase: "prepared"}); err != nil {
		cleanup()
		return 0, err
	}
	// Recovery materials are complete before the first visible gate. Publish
	// the data gate first so a crash can never strand a target-only gate.
	for _, path := range []string{dataLock(p), targetLock(p)} {
		if err = publishLock(path, record); err != nil {
			cleanup()
			return 0, fmt.Errorf("acquire update lock: %w", err)
		}
	}
	cmd := exec.Command(helper, HelperArgument, planPath)
	detach(cmd)
	if err = cmd.Start(); err != nil {
		cleanup()
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func writeExclusive(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return closeErr
}
func copyFile(source, target string, mode os.FileMode) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, f)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(target)
		return err
	}
	return closeErr
}

func validatePlanPaths(p Plan) error {
	target := p.Prepared.TargetPath
	work := p.Prepared.WorkDir
	for _, path := range []string{target, work, p.Prepared.StagedPath, p.Options.DataDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("update paths must be clean and absolute")
		}
	}
	if filepath.Dir(work) != filepath.Dir(target) || !strings.HasPrefix(filepath.Base(work), ".torouter-update-") || p.Prepared.StagedPath != filepath.Join(work, "update.exe") {
		return errors.New("update staging paths are not confined to this installation")
	}
	token, tokenErr := hex.DecodeString(p.Token)
	if tokenErr != nil || len(token) != 24 || p.Options.ParentPID <= 0 || p.Options.Port < 1 || p.Options.Port > 65535 {
		return errors.New("invalid update plan")
	}
	if p.Options.OpenMode != "" && p.Options.OpenMode != "web" && p.Options.OpenMode != "desktop" && p.Options.OpenMode != "cli" {
		return errors.New("invalid update view mode")
	}
	for _, path := range []string{target, work, p.Options.DataDir} {
		info, err := os.Lstat(path)
		if path == target && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("update paths cannot be symlinks")
		}
	}
	return nil
}

func releaseLocks(p Plan) {
	for _, path := range []string{targetLock(p), dataLock(p)} {
		record, err := readLock(path)
		if err == nil && record.Token == p.Token {
			_ = os.Remove(path)
		}
	}
}

// RunHelper is dispatched before any ordinary application initialization.
func RunHelper(ctx context.Context, planPath, publicKey string) error {
	if !filepath.IsAbs(planPath) {
		return errors.New("helper plan must be absolute")
	}
	f, err := os.Open(planPath)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxManifestSize*2+1))
	f.Close()
	if err != nil {
		return err
	}
	if len(b) > MaxManifestSize*2 {
		return errors.New("helper plan too large")
	}
	var plan Plan
	if err = decodeStrict(b, &plan); err != nil {
		return err
	}
	if err = validatePlanPaths(plan); err != nil {
		return err
	}
	if planPath != filepath.Join(plan.Prepared.WorkDir, "plan.json") {
		return errors.New("unexpected helper plan location")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.Abs(self)
	if err != nil {
		return err
	}
	if self != filepath.Join(plan.Prepared.WorkDir, "helper.exe") {
		return errors.New("helper must run from its temporary executable")
	}
	key, err := ParsePublicKey(publicKey)
	if err != nil {
		return err
	}
	release, err := VerifyManifest(plan.Prepared.Manifest, key)
	if err != nil {
		return err
	}
	if release != plan.Prepared.Release {
		return errors.New("helper release differs from signed manifest")
	}
	if plan.Prepared.CurrentVersion != buildinfo.Version {
		return errors.New("helper version differs from current build")
	}
	if isNewer, err := newer(release.Version, buildinfo.Version); err != nil || !isNewer {
		return errors.New("helper release is not newer than current build")
	}
	selfHash, err := HashFile(self)
	if err != nil {
		return err
	}
	if selfHash != plan.Prepared.CurrentSHA256 {
		return errors.New("helper differs from original executable")
	}
	return applyPlan(ctx, plan, defaultHooks())
}

// helperHooks isolates process operations while tests exercise real file
// replacement and exact-file snapshot/restore in temporary installations.
type helperHooks struct {
	wait  func(context.Context, []int) error
	start func(Plan) (*exec.Cmd, error)
	ready func(context.Context, *exec.Cmd, Plan) error
	stop  func(*exec.Cmd) error
	view  func(Plan) error
}

func defaultHooks() helperHooks {
	return helperHooks{wait: waitProcesses, start: startBackend, ready: waitReady, stop: stopBackend, view: openRequestedViews}
}

func applyPlan(ctx context.Context, p Plan, h helperHooks) (err error) {
	if err = validatePlanPaths(p); err != nil {
		return err
	}
	guardCtx, guardCancel := context.WithTimeout(ctx, 45*time.Second)
	defer guardCancel()
	var guard *os.File
	for {
		guard, err = acquireGuard(p.Options.DataDir)
		if err == nil {
			break
		}
		select {
		case <-guardCtx.Done():
			return guardCtx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer guard.Close()
	for _, path := range []string{targetLock(p), dataLock(p)} {
		record, e := readLock(path)
		if e != nil || record.Token != p.Token {
			return errors.New("update lock ownership lost")
		}
	}
	j, journalErr := readJournal(p)
	if journalErr != nil {
		return journalErr
	}
	if j.Phase != "prepared" {
		return recoverInterrupted(ctx, p, j, h)
	}
	safeToUnlock := true
	defer func() {
		if safeToUnlock {
			releaseLocks(p)
		}
	}()
	result := Result{Version: p.Prepared.Release.Version, State: "failed"}
	defer func() {
		if err != nil {
			result.Error = ErrorMessage(err)
		}
		b, _ := json.Marshal(result)
		resultPath := p.Prepared.TargetPath + ".update-result.json"
		temporary := filepath.Join(p.Prepared.WorkDir, "result.json")
		if writeErr := os.WriteFile(temporary, b, 0600); writeErr == nil {
			_ = os.Rename(temporary, resultPath)
		}
	}()
	parentStopped, newStartAttempted := false, false
	defer func() {
		if err != nil && parentStopped && !newStartAttempted {
			fallback, restarted, fallbackErr := restartUntouchedOriginal(ctx, p, j, h, err)
			if restarted {
				result = fallback
				safeToUnlock = true
			}
			if fallbackErr != nil {
				err = errors.Join(err, fallbackErr)
			}
		}
	}()
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	pids := append([]int{p.Options.ParentPID}, p.Options.WaitPIDs...)
	if err = h.wait(waitCtx, pids); err != nil {
		return err
	}
	parentStopped = true
	if err = verifyArtifact(p.Prepared.StagedPath, p.Prepared.Release); err != nil {
		return err
	}
	if hash, e := HashFile(p.Prepared.TargetPath); e != nil || hash != p.Prepared.CurrentSHA256 {
		return errors.New("installed executable changed before replacement")
	}
	snapshot, err := snapshotData(p)
	if err != nil {
		return fmt.Errorf("data snapshot: %w", err)
	}
	j.Phase = "replacing"
	j.Snapshot = snapshot
	if err = writeJournal(p, j); err != nil {
		return err
	}
	safeToUnlock = false
	backup := filepath.Join(p.Prepared.WorkDir, "previous.exe")
	if err = copyFile(p.Prepared.TargetPath, backup, 0700); err != nil {
		return err
	}
	if err = replaceWithRetry(ctx, p.Prepared.StagedPath, p.Prepared.TargetPath); err != nil {
		return err
	}
	j.Phase = "replaced"
	if err = writeJournal(p, j); err != nil {
		return err
	}
	newStartAttempted = true
	child, startErr := trackedStart(p, h, &j)
	if startErr == nil {
		readyCtx, readyCancel := context.WithTimeout(ctx, 25*time.Second)
		startErr = h.ready(readyCtx, child, p)
		readyCancel()
	}
	if startErr == nil {
		j.Phase = "completed"
		j.Outcome = "updated"
		if err = writeJournal(p, j); err != nil {
			return err
		}
		safeToUnlock = true
		result.State = "updated"
		if child != nil && child.Process != nil {
			_ = child.Process.Release()
		}
		removeSnapshot(p)
		releaseLocks(p)
		if p.Options.OpenMode != "" || p.Options.OpenPet {
			if h.view == nil {
				h.view = openRequestedViews
			}
			result.ViewError = ErrorMessage(h.view(p))
		}
		return nil
	}
	if child != nil {
		if stopErr := h.stop(child); stopErr != nil {
			return errors.Join(startErr, fmt.Errorf("cannot confirm failed child stopped; no rollback attempted: %w", stopErr))
		}
	}
	// Acquire all data handles before touching either executable or data. If an
	// unrelated process opened this data directory, leave recovery artifacts.
	files, lockErr := lockDataFiles(p.Options.DataDir)
	if lockErr != nil {
		return errors.Join(startErr, fmt.Errorf("rollback data in use: %w", lockErr))
	}
	closeFiles(files)
	if hash, hashErr := HashFile(backup); hashErr != nil || hash != p.Prepared.CurrentSHA256 {
		return errors.Join(startErr, errors.New("saved executable failed integrity check; rollback stopped"))
	}
	if err = replaceWithRetry(ctx, backup, p.Prepared.TargetPath); err != nil {
		return errors.Join(startErr, err)
	}
	result.ExecutableRestored = true
	j.ExecutableRestored = true
	j.Phase = "restoring"
	if err = writeJournal(p, j); err != nil {
		return err
	}
	if err = restoreData(p, snapshot); err != nil {
		return errors.Join(startErr, fmt.Errorf("restore data: %w", err))
	}
	result.DataRestored = true
	j.DataRestored = true
	j.Phase = "restored"
	if err = writeJournal(p, j); err != nil {
		return err
	}
	safeToUnlock = true
	oldPlan := p
	oldPlan.Prepared.Release.Version = p.Prepared.CurrentVersion
	old, oldErr := trackedStart(oldPlan, h, &j)
	if oldErr == nil {
		readyCtx, readyCancel := context.WithTimeout(ctx, 25*time.Second)
		oldErr = h.ready(readyCtx, old, oldPlan)
		readyCancel()
	}
	if oldErr != nil {
		if old != nil {
			oldErr = errors.Join(oldErr, h.stop(old))
		}
		return errors.Join(startErr, fmt.Errorf("old executable restart: %w", oldErr))
	}
	result.State = "rolled_back"
	j.Phase = "completed"
	j.Outcome = "rolled_back"
	if err = writeJournal(p, j); err != nil {
		return err
	}
	if old != nil && old.Process != nil {
		_ = old.Process.Release()
	}
	releaseLocks(p)
	if p.Options.OpenMode != "" || p.Options.OpenPet {
		if h.view == nil {
			h.view = openRequestedViews
		}
		result.ViewError = ErrorMessage(h.view(p))
	}
	return fmt.Errorf("new version failed; previous executable and data restored: %w", startErr)
}

func openRequestedViews(p Plan) error {
	modes := []string{}
	if p.Options.OpenMode != "" {
		modes = append(modes, p.Options.OpenMode)
	}
	if p.Options.OpenPet {
		modes = append(modes, "pet")
	}
	var result error
	for _, mode := range modes {
		cmd := exec.Command(p.Prepared.TargetPath, "-mode", mode, "-data", p.Options.DataDir, "-port", strconv.Itoa(p.Options.Port))
		for _, item := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(item), updateTokenEnv+"=") {
				cmd.Env = append(cmd.Env, item)
			}
		}
		configureView(cmd, mode)
		if err := cmd.Start(); err != nil {
			result = errors.Join(result, err)
		} else {
			_ = cmd.Process.Release()
		}
	}
	return result
}

func verifyArtifact(path string, r Release) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != r.Size {
		return errors.New("staged artifact size/type changed")
	}
	hash, err := HashFile(path)
	if err != nil {
		return err
	}
	if hash != r.SHA256 {
		return errors.New("staged artifact SHA256 changed")
	}
	return nil
}

func replaceWithRetry(ctx context.Context, from, to string) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		err := atomicReplace(from, to)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("executable is still in use: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func startBackend(p Plan) (*exec.Cmd, error) {
	args := append([]string(nil), p.Options.StartArgs...)
	args = append(args, "-data", p.Options.DataDir, "-port", strconv.Itoa(p.Options.Port), "-autostart", "-restart-grace", "15", "-restart-ready", p.Token)
	cmd := exec.Command(p.Prepared.TargetPath, args...)
	cmd.Env = append(os.Environ(), updateTokenEnv+"="+p.Token)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func stopBackend(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return nil
	}
	return errors.Join(killErr, waitErr)
}

func waitReady(ctx context.Context, cmd *exec.Cmd, p Plan) error {
	client := &http.Client{Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("updated backend did not become ready: %w", ctx.Err())
		case <-ticker.C:
			if !processRunning(cmd.Process.Pid) {
				return errors.New("updated backend exited before readiness")
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/_internal/restart-ready", p.Options.Port), nil)
			req.Header.Set("X-To-Router-Restart", p.Token)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			var ready struct {
				PID     int    `json:"pid"`
				Port    int    `json:"port"`
				Version string `json:"version"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ready)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && ready.PID == cmd.Process.Pid && ready.Port == p.Options.Port && (p.Prepared.Release.Version == "" || ready.Version == p.Prepared.Release.Version) && processRunning(cmd.Process.Pid) {
				return nil
			}
		}
	}
}

func ReadResult(target string) (Result, error) {
	var result Result
	f, err := os.Open(target + ".update-result.json")
	if err != nil {
		return result, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		return result, err
	}
	err = decodeStrict(b, &result)
	return result, err
}
