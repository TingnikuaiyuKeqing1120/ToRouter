package updater

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torouter/internal/buildinfo"
)

func trustedRecoveryPlan(t *testing.T) Plan {
	t.Helper()
	p := testPlan(t)
	pub, key := keyPair(t)
	p.Prepared.Manifest = signed(t, p.Prepared.Release, key)
	old := buildinfo.UpdatePublicKey
	buildinfo.UpdatePublicKey = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { buildinfo.UpdatePublicKey = old })
	if err := atomicJSON(filepath.Join(p.Prepared.WorkDir, "plan.json"), p); err != nil {
		t.Fatal(err)
	}
	setLockOwner(t, p, processIdentity{PID: 2147483647, Created: 1})
	return p
}

func setLockOwner(t *testing.T, p Plan, owner processIdentity) {
	t.Helper()
	r := lockRecord{Token: p.Token, PlanPath: filepath.Join(p.Prepared.WorkDir, "plan.json"), Owner: owner}
	for _, path := range []string{targetLock(p), dataLock(p)} {
		if err := atomicJSON(path, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStaleBeforeReplacementAutomaticallyUnblocks(t *testing.T) {
	p := trustedRecoveryPlan(t)
	if err := CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{targetLock(p), dataLock(p)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("stale gate was not removed")
		}
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.CurrentSHA256 {
		t.Fatal("pre-replacement recovery touched old executable")
	}
}

func TestRecoveryNeverClearsLiveOwnerOrGuard(t *testing.T) {
	p := trustedRecoveryPlan(t)
	self, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	setLockOwner(t, p, self)
	if err = CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("cleared live launch owner")
	}
	setLockOwner(t, p, processIdentity{2147483647, 1})
	guard, err := acquireGuard(p.Options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("cleared OS-held live transaction guard")
	}
	guard.Close()
	if err = CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal("dead owner did not recover after guard release", err)
	}
}

func TestReusedPIDCannotImpersonateUpdateOwner(t *testing.T) {
	p := trustedRecoveryPlan(t)
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	identity.Created++
	setLockOwner(t, p, identity)
	if err = CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal("PID reuse was treated as the original owner", err)
	}
}

func TestUnregisteredChildCannotOpenStore(t *testing.T) {
	p := trustedRecoveryPlan(t)
	t.Setenv(updateTokenEnv, p.Token)
	start := time.Now()
	if err := CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("unrecorded child passed launch gate")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child registration wait was unbounded")
	}
	if _, err := os.Stat(dataLock(p)); err != nil {
		t.Fatal("child deleted gate")
	}
}

func TestRecoveryRefusesBadSignedPlan(t *testing.T) {
	p := trustedRecoveryPlan(t)
	p.Prepared.Manifest = []byte(`{"payload":{},"signature":"invalid"}`)
	if err := atomicJSON(filepath.Join(p.Prepared.WorkDir, "plan.json"), p); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("cleared lock from invalid signed recovery plan")
	}
	if _, err := os.Stat(dataLock(p)); err != nil {
		t.Fatal("invalid signed plan removed gate")
	}
}

func interruptedPlan(t *testing.T) Plan {
	t.Helper()
	p := trustedRecoveryPlan(t)
	snapshot, err := snapshotData(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(p.Prepared.TargetPath, filepath.Join(p.Prepared.WorkDir, "previous.exe")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(p.Prepared.StagedPath, p.Prepared.TargetPath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.Options.DataDir, "router.db"), []byte("interrupted migration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = writeJournal(p, journal{Phase: "replaced", Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInterruptedReplacementRestoresExactFiles(t *testing.T) {
	p := interruptedPlan(t)
	j, err := readJournal(p)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	h := helperHooks{start: func(plan Plan) (*exec.Cmd, error) {
		starts++
		hash, _ := HashFile(plan.Prepared.TargetPath)
		if hash != p.Prepared.CurrentSHA256 {
			t.Fatal("did not restore old executable")
		}
		b, _ := os.ReadFile(filepath.Join(plan.Options.DataDir, "router.db"))
		if string(b) != "old database bytes" {
			t.Fatal("did not restore pre-update database")
		}
		return &exec.Cmd{}, nil
	}, ready: func(context.Context, *exec.Cmd, Plan) error { return nil }, stop: func(*exec.Cmd) error { return nil }}
	guard, err := acquireGuard(p.Options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	err = recoverInterrupted(context.Background(), p, j, h)
	guard.Close()
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatal("previous backend not restarted")
	}
	result, err := ReadResult(p.Prepared.TargetPath)
	if err != nil || result.State != "rolled_back" || !result.DataRestored {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestInterruptedRecoveryNeverTouchesLiveRegisteredChild(t *testing.T) {
	p := interruptedPlan(t)
	j, _ := readJournal(p)
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	j.Child = identity
	if err = writeJournal(p, j); err != nil {
		t.Fatal(err)
	}
	if err = CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("allowed recovery while registered backend remains alive")
	}
	if err = recoverInterrupted(context.Background(), p, j, helperHooks{}); err == nil {
		t.Fatal("restored data under a live registered process")
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.Release.SHA256 {
		t.Fatal("changed executable while registered child is alive")
	}
	b, _ := os.ReadFile(filepath.Join(p.Options.DataDir, "router.db"))
	if string(b) != "interrupted migration" {
		t.Fatal("restored data while registered child is alive")
	}
}

func TestCompletedInterruptedCleanupPreservesHealthyData(t *testing.T) {
	p := interruptedPlan(t)
	j, _ := readJournal(p)
	j.Phase = "completed"
	j.Outcome = "updated"
	if err := writeJournal(p, j); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal(err)
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.Release.SHA256 {
		t.Fatal("rolled back a completed transaction")
	}
	b, _ := os.ReadFile(filepath.Join(p.Options.DataDir, "router.db"))
	if string(b) != "interrupted migration" {
		t.Fatal("rolled back healthy migrated data")
	}
}

func TestPublicResultErrorsHideLocalPaths(t *testing.T) {
	err := &os.PathError{Op: "start", Path: filepath.Join(t.TempDir(), "program.exe"), Err: errors.New("fixture error")}
	message := ErrorMessage(err)
	if strings.Contains(message, err.Path) {
		t.Fatal("public update error exposed path")
	}
}

func TestRecoveryAfterOriginalRenameBeforeNewPlacement(t *testing.T) {
	p := trustedRecoveryPlan(t)
	snapshot, err := snapshotData(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(p.Prepared.TargetPath, filepath.Join(p.Prepared.WorkDir, "previous.exe")); err != nil {
		t.Fatal(err)
	}
	j := journal{Phase: "replacing", Snapshot: snapshot}
	if err = writeJournal(p, j); err != nil {
		t.Fatal(err)
	}
	if _, err = loadRecoveryPlan(lockRecord{Token: p.Token, PlanPath: filepath.Join(p.Prepared.WorkDir, "plan.json")}, p.Options.DataDir); err != nil {
		t.Fatal("missing target prevented signed recovery plan validation", err)
	}
	h := helperHooks{start: func(Plan) (*exec.Cmd, error) { return &exec.Cmd{}, nil }, ready: func(context.Context, *exec.Cmd, Plan) error { return nil }, stop: func(*exec.Cmd) error { return nil }}
	if err = recoverInterrupted(context.Background(), p, j, h); err != nil {
		t.Fatal(err)
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.CurrentSHA256 {
		t.Fatal("did not restore missing original target")
	}
}

func TestGatePublicationNeverOverwritesExistingOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".update-lock")
	first := lockRecord{Token: strings.Repeat("ab", 24), PlanPath: filepath.Join(dir, "one.json"), Owner: processIdentity{1, 1}}
	second := first
	second.Token = strings.Repeat("cd", 24)
	if err := publishLock(path, first); err != nil {
		t.Fatal(err)
	}
	if err := publishLock(path, second); err == nil {
		t.Fatal("replaced an existing owner's gate")
	}
	got, err := readLock(path)
	if err != nil || got.Token != first.Token {
		t.Fatal("existing owner record changed", err)
	}
}

func TestRelativeDataPathRecoversPreparedStaleGate(t *testing.T) {
	p := trustedRecoveryPlan(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, p.Options.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckLaunch(relative); err != nil {
		t.Fatal("same relative data directory failed recovery", err)
	}
}

func TestPreReplacementFailureRestartsVerifiedOldBackend(t *testing.T) {
	p := testPlan(t)
	if err := os.WriteFile(p.Prepared.StagedPath, []byte("tampered after download"), 0700); err != nil {
		t.Fatal(err)
	}
	started := 0
	h := helperHooks{wait: func(context.Context, []int) error { return nil }, start: func(plan Plan) (*exec.Cmd, error) {
		started++
		hash, _ := HashFile(plan.Prepared.TargetPath)
		if hash != p.Prepared.CurrentSHA256 || plan.Prepared.Release.Version != p.Prepared.CurrentVersion {
			t.Fatal("fallback did not start original build")
		}
		if err := CheckLaunch(plan.Options.DataDir); err != nil {
			t.Fatal("old startup blocked after safe pre-replacement failure", err)
		}
		return &exec.Cmd{}, nil
	}, ready: func(context.Context, *exec.Cmd, Plan) error { return nil }, stop: func(*exec.Cmd) error { return nil }}
	if err := applyPlan(context.Background(), p, h); err == nil {
		t.Fatal("lost original preparation failure")
	}
	if started != 1 {
		t.Fatal("original backend was not restarted")
	}
	result, err := ReadResult(p.Prepared.TargetPath)
	if err != nil || result.State != "rolled_back" || result.ExecutableRestored || result.DataRestored {
		t.Fatalf("fallback misreported restore operations: %#v %v", result, err)
	}
}
