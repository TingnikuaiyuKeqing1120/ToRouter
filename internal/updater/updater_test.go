package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func keyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}
func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func signed(t *testing.T, r Release, key ed25519.PrivateKey) []byte {
	t.Helper()
	b, err := SignManifest(r, key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func fixtureRelease() Release {
	return Release{"1.2.4", runtime.GOOS, runtime.GOARCH, "https://updates.example.invalid/app.exe", digest([]byte("new binary")), 10, "Signed release notes"}
}

func TestManifestVerification(t *testing.T) {
	pub, priv := keyPair(t)
	r := fixtureRelease()
	valid := signed(t, r, priv)
	got, err := VerifyManifest(valid, pub)
	if err != nil || got != r {
		t.Fatalf("valid signed metadata: %#v %v", got, err)
	}
	other, _ := keyPair(t)
	for name, input := range map[string][]byte{"malformed": []byte(`{"payload":`), "oversize": bytes.Repeat([]byte("x"), MaxManifestSize+1), "tampered": bytes.Replace(valid, []byte("Signed release notes"), []byte("Forged release notes"), 1), "trailing": append(append([]byte(nil), valid...), []byte(` {}`)...)} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyManifest(input, pub); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	if _, err = VerifyManifest(valid, other); err == nil {
		t.Fatal("accepted untrusted key")
	}
	for _, version := range []string{"latest", "1.2", "1.02.3", "1.2.3-beta", "-1.2.3"} {
		bad := r
		bad.Version = version
		if _, err := VerifyManifest(signed(t, bad, priv), pub); err == nil {
			t.Fatalf("accepted version %q", version)
		}
	}
	for _, size := range []int64{0, MaxArtifactSize + 1} {
		bad := r
		bad.Size = size
		if _, err := VerifyManifest(signed(t, bad, priv), pub); err == nil {
			t.Fatal("accepted invalid size")
		}
	}
}

func TestVersionOrdering(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{{"1.2.4", "1.2.3", true}, {"v2.0.0", "1.99.99", true}, {"1.2.3", "1.2.3", false}, {"1.2.2", "1.2.3", false}, {"1.2.99", "1.3.0", false}} {
		got, err := newer(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Fatalf("%s > %s = %v %v", tt.a, tt.b, got, err)
		}
	}
}

func TestURLPolicy(t *testing.T) {
	for _, tt := range []struct {
		url         string
		allow, want bool
	}{{"https://updates.example.invalid/app", false, true}, {"http://127.0.0.1:1234/app", false, false}, {"http://127.0.0.1:1234/app", true, true}, {"http://[::1]:1234/app", true, true}, {"http://localhost/app", true, false}, {"http://updates.example.invalid/app", true, false}, {"https://user:password@updates.example.invalid/app", false, false}, {"file:///tmp/app", true, false}} {
		if got := validateURL(tt.url, tt.allow) == nil; got != tt.want {
			t.Errorf("URL %s allowed=%v", tt.url, got)
		}
	}
}

type feedFixture struct {
	manager       *Manager
	release       Release
	key           ed25519.PrivateKey
	manifest      []byte
	artifact      []byte
	checks        atomic.Int32
	downloads     atomic.Int32
	delay         time.Duration
	artifactDelay time.Duration
	status        int
}

func newFeed(t *testing.T) *feedFixture {
	t.Helper()
	pub, key := keyPair(t)
	f := &feedFixture{key: key, artifact: []byte("synthetic new executable"), status: 200}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			f.checks.Add(1)
			if f.delay > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(f.delay):
				}
			}
			w.WriteHeader(f.status)
			w.Write(f.manifest)
		} else {
			f.downloads.Add(1)
			if f.artifactDelay > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(f.artifactDelay):
				}
			}
			w.Write(f.artifact)
		}
	}))
	t.Cleanup(server.Close)
	f.release = Release{"1.2.4", runtime.GOOS, runtime.GOARCH, server.URL + "/app.exe", digest(f.artifact), int64(len(f.artifact)), "Signed fixture notes"}
	f.manifest = signed(t, f.release, key)
	target := filepath.Join(t.TempDir(), "ToRouter.exe")
	if err := os.WriteFile(target, []byte("synthetic old executable"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Version: "1.2.3", FeedURL: server.URL + "/manifest", PublicKey: base64.StdEncoding.EncodeToString(pub), TargetPath: target, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	f.manager = m
	return f
}

func TestDisabledDeveloperBuild(t *testing.T) {
	m, err := New(Config{Version: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Check(context.Background())
	if err != nil || s.Enabled || s.State != "disabled" || s.CurrentVersion != "dev" {
		t.Fatalf("%#v %v", s, err)
	}
	if _, err = m.Prepare(context.Background()); err == nil {
		t.Fatal("developer build prepared update")
	}
}

func TestCheckValidationAndNoNewer(t *testing.T) {
	for _, kind := range []string{"valid", "same", "older", "platform", "signature", "oversize", "http-error"} {
		t.Run(kind, func(t *testing.T) {
			f := newFeed(t)
			r := f.release
			switch kind {
			case "same":
				r.Version = "1.2.3"
			case "older":
				r.Version = "1.2.2"
			case "platform":
				r.OS = "other-os"
			case "signature":
				f.manifest = bytes.Replace(f.manifest, []byte("fixture notes"), []byte("forgery notes"), 1)
			case "oversize":
				f.manifest = bytes.Repeat([]byte("x"), MaxManifestSize+1)
			case "http-error":
				f.status = 503
			}
			if kind == "same" || kind == "older" || kind == "platform" {
				f.manifest = signed(t, r, f.key)
			}
			s, err := f.manager.Check(context.Background())
			bad := kind == "platform" || kind == "signature" || kind == "oversize" || kind == "http-error"
			if bad {
				if err == nil || s.Available != nil || s.State != "error" {
					t.Fatalf("invalid feed: %#v %v", s, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (s.Available != nil) != (kind == "valid") {
				t.Fatalf("availability: %#v", s)
			}
			if s.Available != nil && s.Available.Notes != r.Notes {
				t.Fatal("signed notes lost")
			}
		})
	}
}

func TestConcurrentChecksAndPrepare(t *testing.T) {
	f := newFeed(t)
	f.delay = 150 * time.Millisecond
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, err := f.manager.Check(context.Background()); errs <- err }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.checks.Load(); got != 1 {
		t.Fatalf("concurrent checks fetched %d times", got)
	}
	start = make(chan struct{})
	prepared := make(chan *Prepared, 16)
	errs = make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, err := f.manager.PrepareVersion(context.Background(), "1.2.4")
			prepared <- p
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(prepared)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var path string
	for p := range prepared {
		if path != "" && p.StagedPath != path {
			t.Fatal("concurrent preparation created competing artifacts")
		}
		path = p.StagedPath
	}
	if f.downloads.Load() != 1 {
		t.Fatalf("download count %d", f.downloads.Load())
	}
	if f.manager.Status().State != "ready" {
		t.Fatal("not ready")
	}
}

func TestDownloadTamperAndCancellation(t *testing.T) {
	t.Run("tamper", func(t *testing.T) {
		f := newFeed(t)
		f.artifact = []byte("tampered executable")
		if _, err := f.manager.Prepare(context.Background()); err == nil {
			t.Fatal("accepted tampered artifact")
		}
		entries, _ := filepath.Glob(filepath.Join(filepath.Dir(f.manager.config.TargetPath), ".torouter-update-*"))
		if len(entries) != 0 {
			t.Fatal("failed download left staging")
		}
		old, _ := os.ReadFile(f.manager.config.TargetPath)
		if string(old) != "synthetic old executable" {
			t.Fatal("download failure touched old executable")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := newFeed(t)
		f.delay = time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if _, err := f.manager.Check(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected cancellation: %v", err)
		}
		if f.manager.Status().Available != nil {
			t.Fatal("canceled check advertised update")
		}
	})
	t.Run("confirmed version", func(t *testing.T) {
		f := newFeed(t)
		if _, err := f.manager.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.release.Version = "1.2.5"
		f.manifest = signed(t, f.release, f.key)
		if _, err := f.manager.PrepareVersion(context.Background(), "1.2.4"); err == nil {
			t.Fatal("installed unconfirmed version")
		}
		if f.downloads.Load() != 0 {
			t.Fatal("downloaded unconfirmed version")
		}
	})
	t.Run("download cancellation", func(t *testing.T) {
		f := newFeed(t)
		f.artifactDelay = time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		if _, err := f.manager.Prepare(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("download cancellation: %v", err)
		}
		entries, _ := filepath.Glob(filepath.Join(filepath.Dir(f.manager.config.TargetPath), ".torouter-update-*"))
		if len(entries) != 0 {
			t.Fatal("canceled download left staging")
		}
	})
	t.Run("same version different artifact", func(t *testing.T) {
		f := newFeed(t)
		oldHash := f.release.SHA256
		if _, err := f.manager.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.artifact = []byte("different signed executable under same version")
		f.release.SHA256 = digest(f.artifact)
		f.release.Size = int64(len(f.artifact))
		f.manifest = signed(t, f.release, f.key)
		if _, err := f.manager.PrepareRelease(context.Background(), f.release.Version, oldHash); err == nil {
			t.Fatal("substituted a different signed artifact with the confirmed version")
		}
		if f.downloads.Load() != 0 {
			t.Fatal("downloaded unconfirmed content")
		}
	})
}

func TestViewsRestoreOnlyAfterGateRelease(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		p := testPlan(t)
		p.Options.OpenMode = "desktop"
		p.Options.OpenPet = true
		starts, views := 0, 0
		h := helperHooks{wait: func(context.Context, []int) error { return nil }, start: func(Plan) (*exec.Cmd, error) { starts++; return &exec.Cmd{}, nil }, ready: func(context.Context, *exec.Cmd, Plan) error {
			if rollback && starts == 1 {
				return errors.New("fixture start failure")
			}
			return nil
		}, stop: func(*exec.Cmd) error { return nil }, view: func(plan Plan) error {
			views++
			if plan.Options.OpenMode != "desktop" || !plan.Options.OpenPet {
				t.Fatal("view intent lost")
			}
			if err := CheckLaunch(plan.Options.DataDir); err != nil {
				t.Fatal("view opened while gate held")
			}
			return errors.New("fixture view unavailable")
		}}
		err := applyPlan(context.Background(), p, h)
		if (err != nil) != rollback {
			t.Fatalf("view failure changed backend update result: %v", err)
		}
		result, _ := ReadResult(p.Prepared.TargetPath)
		if views != 1 || result.ViewError == "" {
			t.Fatalf("view error not recorded: %#v views=%d", result, views)
		}
	}
}

func testPlan(t *testing.T) Plan {
	t.Helper()
	f := newFeed(t)
	p, err := f.manager.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	for name, value := range map[string]string{"router.db": "old database bytes", "master.key": "synthetic old key", "router.db-wal": "old wal"} {
		if err = os.WriteFile(filepath.Join(data, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan := Plan{*p, HelperOptions{DataDir: data, Port: 12345, ParentPID: 11111}, strings.Repeat("ab", 24)}
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(plan.Prepared.WorkDir, "plan.json")
	if err = atomicJSON(planPath, plan); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(lockRecord{Token: plan.Token, PlanPath: planPath, Owner: owner})
	for _, path := range []string{targetLock(plan), dataLock(plan)} {
		if err = writeExclusive(path, record, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = writeJournal(plan, journal{Phase: "prepared"}); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestReplacementAndDataRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			p := testPlan(t)
			var starts, stops int
			h := helperHooks{wait: func(context.Context, []int) error { return nil }, start: func(plan Plan) (*exec.Cmd, error) {
				starts++
				if starts == 1 {
					b, _ := os.ReadFile(plan.Prepared.TargetPath)
					if digest(b) != p.Prepared.Release.SHA256 {
						t.Fatal("new executable not installed before start")
					}
					if fail {
						os.WriteFile(filepath.Join(p.Options.DataDir, "router.db"), []byte("new migrated database"), 0600)
						os.WriteFile(filepath.Join(p.Options.DataDir, "master.key"), []byte("changed synthetic key"), 0600)
						os.WriteFile(filepath.Join(p.Options.DataDir, "router.db-shm"), []byte("transient"), 0600)
					}
				} else {
					if stops != 1 {
						t.Fatal("rollback started before failed child stopped")
					}
					if plan.Prepared.Release.Version != "1.2.3" {
						t.Fatal("rollback health did not require old version")
					}
				}
				return &exec.Cmd{}, nil
			}, ready: func(context.Context, *exec.Cmd, Plan) error {
				journal, err := readJournal(p)
				if err != nil {
					t.Fatal(err)
				}
				if starts == 1 && journal.Phase == "completed" {
					t.Fatal("marked update completed before readiness")
				}
				if fail && starts == 1 {
					return errors.New("fixture startup failed")
				}
				return nil
			}, stop: func(*exec.Cmd) error { stops++; return nil }}
			err := applyPlan(context.Background(), p, h)
			if (err != nil) != fail {
				t.Fatalf("replacement result %v", err)
			}
			result, e := ReadResult(p.Prepared.TargetPath)
			if e != nil {
				t.Fatal(e)
			}
			for _, path := range []string{targetLock(p), dataLock(p)} {
				if _, e = os.Stat(path); !os.IsNotExist(e) {
					t.Fatal("update lock leaked")
				}
			}
			if fail {
				hash, _ := HashFile(p.Prepared.TargetPath)
				if hash != p.Prepared.CurrentSHA256 {
					t.Fatal("old executable not restored")
				}
				for name, want := range map[string]string{"router.db": "old database bytes", "master.key": "synthetic old key", "router.db-wal": "old wal"} {
					b, _ := os.ReadFile(filepath.Join(p.Options.DataDir, name))
					if string(b) != want {
						t.Fatalf("%s not restored", name)
					}
				}
				if _, err = os.Stat(filepath.Join(p.Options.DataDir, "router.db-shm")); !os.IsNotExist(err) {
					t.Fatal("new shm survived rollback")
				}
				if result.State != "rolled_back" || !result.DataRestored || !result.ExecutableRestored {
					t.Fatalf("%#v", result)
				}
			} else {
				if result.State != "updated" || starts != 1 {
					t.Fatalf("%#v starts=%d", result, starts)
				}
				if _, err = os.Stat(snapshotDir(p)); !os.IsNotExist(err) {
					t.Fatal("successful update retained key snapshot")
				}
				if _, err = os.Stat(filepath.Join(p.Prepared.WorkDir, "previous.exe")); err != nil {
					t.Fatal("no recovery executable")
				}
			}
		})
	}
}

func TestRefuseRollbackUntilFailedChildStopped(t *testing.T) {
	p := testPlan(t)
	h := helperHooks{wait: func(context.Context, []int) error { return nil }, start: func(Plan) (*exec.Cmd, error) { return &exec.Cmd{}, nil }, ready: func(context.Context, *exec.Cmd, Plan) error { return errors.New("failed startup") }, stop: func(*exec.Cmd) error { return errors.New("still running") }}
	if err := applyPlan(context.Background(), p, h); err == nil {
		t.Fatal("accepted unconfirmed stop")
	}
	hash, _ := HashFile(p.Prepared.TargetPath)
	if hash != p.Prepared.Release.SHA256 {
		t.Fatal("touched executable while child could still run")
	}
	result, _ := ReadResult(p.Prepared.TargetPath)
	if result.DataRestored || result.ExecutableRestored {
		t.Fatal("claimed rollback despite active child")
	}
}

func TestLaunchGateAndPathConfinement(t *testing.T) {
	p := testPlan(t)
	if err := CheckLaunch(p.Options.DataDir); err == nil {
		t.Fatal("unrelated backend bypassed lock")
	}
	t.Setenv(updateTokenEnv, p.Token)
	self, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = writeJournal(p, journal{Phase: "replaced", Child: self}); err != nil {
		t.Fatal(err)
	}
	if err := CheckLaunch(p.Options.DataDir); err != nil {
		t.Fatal(err)
	}
	p.Prepared.StagedPath = filepath.Join(t.TempDir(), "update.exe")
	if err := validatePlanPaths(p); err == nil {
		t.Fatal("accepted unconfined staged executable")
	}
}

func TestStatusCopiesCannotMutateCachedMetadata(t *testing.T) {
	f := newFeed(t)
	s, err := f.manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Available.Notes = "changed"
	if f.manager.Status().Available.Notes == "changed" {
		t.Fatal("status mutated cached signed notes")
	}
	encoded, _ := json.Marshal(f.manager.Status())
	if bytes.Contains(encoded, []byte(f.manager.config.TargetPath)) {
		t.Fatal("public status exposed installation path")
	}
}
