package pluginmarket

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/marketprofile"
)

func tarFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if e := tw.WriteHeader(&tar.Header{Name: "package/" + name, Mode: 0644, Size: int64(len(data))}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write([]byte(data)); e != nil {
			t.Fatal(e)
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func integrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}
func packageFixture(t *testing.T, name, version string) []byte {
	return tarFixture(t, map[string]string{"package.json": `{"name":"` + name + `","version":"` + version + `","exports":{".":{"import":"./index.mjs"}},"scripts":{"postinstall":"exit 99"}}`, "index.mjs": "export const plugin = async () => ({auth: {provider: 'fixture'}});"})
}
func inspectFixture(_ context.Context, x Installed) ([]marketprofile.Provider, error) {
	if _, e := os.Stat(x.ModulePath); e != nil {
		return nil, e
	}
	return []marketprofile.Provider{{ID: "fixture", Name: "Fixture", Methods: []marketprofile.AuthMethod{{Type: "api", Label: "Fixture key"}}}}, nil
}

type registryFixture struct {
	mu         sync.RWMutex
	version    string
	archive    []byte
	corrupt    bool
	dependency []byte
	server     *httptest.Server
}

func newRegistryFixture(t *testing.T, name string) (*registryFixture, *Manager) {
	t.Helper()
	f := &registryFixture{version: "1.0.0", archive: packageFixture(t, name, "1.0.0")}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.RLock()
		defer f.mu.RUnlock()
		if r.URL.Path == "/fixture-runtime-dep" && f.dependency != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "fixture-runtime-dep", "dist-tags": map[string]string{"latest": "1.0.0"}, "versions": map[string]any{"1.0.0": map[string]any{"name": "fixture-runtime-dep", "version": "1.0.0", "dist": map[string]string{"tarball": f.server.URL + "/dependency.tgz", "integrity": integrity(f.dependency)}}}})
			return
		}
		if r.URL.Path == "/dependency.tgz" && f.dependency != nil {
			_, _ = w.Write(f.dependency)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/latest") || strings.HasSuffix(r.URL.Path, "/1.0.0") {
			sri := integrity(f.archive)
			if f.corrupt {
				sri = "sha512-broken"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "version": f.version, "dist": map[string]string{"tarball": f.server.URL + "/package.tgz", "integrity": sri}})
			return
		}
		if r.URL.Path == "/package.tgz" {
			_, _ = w.Write(f.archive)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(f.server.Close)
	m := New(t.TempDir())
	m.npmURL = f.server.URL
	m.prepareRuntime = func(context.Context) (string, error) { return "fixture-bun", nil }
	m.dependencies = func(context.Context, string, string) error { return nil }
	return f, m
}
func (f *registryFixture) update(t *testing.T, name, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
	f.archive = packageFixture(t, name, version)
}

func TestInstallActivationRollbackRestartAndRemoval(t *testing.T) {
	ctx := context.Background()
	name := "opencode-fixture-provider"
	f, m := newRegistryFixture(t, name)
	first, e := m.Install(ctx, name, inspectFixture)
	if e != nil {
		t.Fatal(e)
	}
	if first.ID != marketprofile.PackageID(name) || first.Community || first.Version != "1.0.0" {
		t.Fatalf("unexpected installed: %#v", first)
	}
	f.update(t, name, "2.0.0")
	var failedPath string
	_, e = m.Install(ctx, name, func(_ context.Context, x Installed) ([]marketprofile.Provider, error) {
		failedPath = x.ModulePath
		return nil, errors.New("fixture probe failed")
	})
	if e == nil {
		t.Fatal("probe failure activated")
	}
	if _, e = os.Stat(failedPath); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("failed candidate not removed")
	}
	active, ok := m.Lookup(first.ID)
	if !ok || active.ModulePath != first.ModulePath || active.Version != "1.0.0" {
		t.Fatal("probe failure replaced old version")
	}
	m.dependencies = func(context.Context, string, string) error { return errors.New("fixture dependency failure") }
	if _, e = m.Install(ctx, name, inspectFixture); e == nil {
		t.Fatal("dependency failure activated")
	}
	m.dependencies = func(context.Context, string, string) error { return nil }
	f.mu.Lock()
	f.corrupt = true
	f.mu.Unlock()
	if _, e = m.Install(ctx, name, inspectFixture); e == nil {
		t.Fatal("bad integrity installed")
	}
	f.mu.Lock()
	f.corrupt = false
	f.mu.Unlock()
	second, e := m.Install(ctx, name, inspectFixture)
	if e != nil {
		t.Fatal(e)
	}
	if second.Version != "2.0.0" || second.ModulePath == first.ModulePath {
		t.Fatal("update did not create immutable version")
	}
	if _, e = os.Stat(first.ModulePath); e != nil {
		t.Fatal("old stream code removed", e)
	}
	third, e := m.Install(ctx, name, inspectFixture)
	if e != nil {
		t.Fatal("same-version reinstall", e)
	}
	if third.ModulePath == second.ModulePath {
		t.Fatal("reinstall mutated old version")
	}
	reopened := New(filepath.Dir(filepath.Dir(m.root)))
	loaded, ok := reopened.Lookup(first.ID)
	if !ok || loaded.ModulePath != third.ModulePath || loaded.RuntimePath != "fixture-bun" {
		t.Fatal("restart lost activation/runtime paths")
	}
	loaded.Providers[0].ID = "mutated"
	again, _ := reopened.Lookup(first.ID)
	if again.Providers[0].ID != "fixture" {
		t.Fatal("caller mutated manager state")
	}
	credentials := filepath.Join(filepath.Dir(filepath.Dir(m.root)), "synthetic-credentials.json")
	if e = os.WriteFile(credentials, []byte("fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = reopened.Remove(first.ID); e != nil {
		t.Fatal(e)
	}
	if _, ok = reopened.Lookup(first.ID); ok {
		t.Fatal("still registered")
	}
	if _, e = os.Stat(first.ModulePath); e != nil {
		t.Fatal("remove deleted old version")
	}
	if b, e := os.ReadFile(credentials); e != nil || string(b) != "fixture" {
		t.Fatal("remove changed unrelated credentials")
	}
	if len(New(filepath.Dir(filepath.Dir(m.root))).Installed()) != 0 {
		t.Fatal("remove did not persist")
	}
}

func TestInstallRejectsMissingInvalidAndCanceledProbe(t *testing.T) {
	_, m := newRegistryFixture(t, "opencode-fixture-provider")
	ctx := context.Background()
	for _, probe := range []Inspector{nil, func(context.Context, Installed) ([]marketprofile.Provider, error) { return nil, nil }, func(context.Context, Installed) ([]marketprofile.Provider, error) {
		return []marketprofile.Provider{{ID: "../escape"}}, nil
	}, func(context.Context, Installed) ([]marketprofile.Provider, error) {
		return []marketprofile.Provider{{ID: "same"}, {ID: "same"}}, nil
	}} {
		if _, e := m.Install(ctx, "opencode-fixture-provider", probe); e == nil {
			t.Fatal("invalid probe accepted")
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	_, e := m.Install(canceled, "opencode-fixture-provider", func(context.Context, Installed) ([]marketprofile.Provider, error) {
		cancel()
		return []marketprofile.Provider{{ID: "fixture"}}, nil
	})
	if e == nil || len(m.Installed()) != 0 {
		t.Fatal("canceled install activated")
	}
}

func TestArchiveAndEntryProtection(t *testing.T) {
	for _, name := range []string{"../escape", "nested/../../escape", "/absolute", "C:/escape", "nested\\escape"} {
		t.Run(name, func(t *testing.T) {
			if _, e := archiveFiles(tarFixture(t, map[string]string{name: "bad"}), 1024); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "package/link", Typeflag: tar.TypeSymlink, Linkname: "../../escape"})
	_ = tw.Close()
	_ = gz.Close()
	if _, e := archiveFiles(b.Bytes(), 1024); e == nil {
		t.Fatal("symlink accepted")
	}
	if _, e := archiveFiles(tarFixture(t, map[string]string{"large": "12345"}), 4); e == nil {
		t.Fatal("expansion limit ignored")
	}
	for _, entry := range []string{"../escape", "/escape", "C:/escape", "dir\\escape"} {
		if _, e := packageEntry(manifest{Main: entry}); e == nil {
			t.Fatal("unsafe module entry", entry)
		}
	}
	dir := t.TempDir()
	if e := os.Mkdir(filepath.Join(dir, "dist"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "dist", "index.mjs"), []byte("fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, p := range []manifest{{Exports: json.RawMessage(`{".":{"import":"./dist/index.mjs","default":"bad.js"}}`)}, {Module: "dist/index.mjs"}, {Main: "dist"}} {
		got, e := resolveEntry(dir, p)
		if e != nil || got != filepath.Join(dir, "dist", "index.mjs") {
			t.Fatalf("entry resolution: %s %v", got, e)
		}
	}
}

func TestRegistryFiltersAndCacheReturnsImmediately(t *testing.T) {
	registry := `{"plugins":[{"package":"@magpie-community/fixture","name":"Fixture","community":true,"providers":["fixture"]},{"package":"@magpie-community/tools","name":"Tools","community":true,"kind":"tools","providers":["pretend"]},{"package":"opencode-third","name":"Third","providers":["third"]}]}`
	list, e := parseRegistry([]byte(registry))
	if e != nil || len(list) != 1 {
		t.Fatalf("registry filtering: %#v %v", list, e)
	}
	m := New(t.TempDir())
	m.cache.Sources = map[string][]Item{"community": list, "npm": {{Package: "@magpie-community/unlisted", Name: "Unlisted", Community: true, Providers: []string{"unlisted"}}}}
	m.trusted = map[string]Item{list[0].Package: list[0]}
	start := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(start) })
		<-release
		http.Error(w, "fixture offline", 503)
	}))
	defer srv.Close()
	m.registryURL = srv.URL
	m.npmURL = srv.URL
	m.githubURL = srv.URL
	before := time.Now()
	out, e := m.Catalogue(context.Background(), "")
	if e != nil || time.Since(before) > 100*time.Millisecond || !out.Refreshing || len(out.Items) != 2 {
		t.Fatalf("catalogue blocked or lost cache: %#v %v", out, e)
	}
	if !out.Items[0].Community || out.Items[1].Community {
		t.Fatal("unlisted namespace received community badge")
	}
	<-start
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.RLock()
		done := !m.refreshing
		m.mu.RUnlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	out, e = m.Catalogue(context.Background(), "")
	if e != nil || len(out.Items) != 2 || out.Warning == "" {
		t.Fatalf("failed refresh destroyed cache: %#v %v", out, e)
	}
	reopened := New(filepath.Dir(filepath.Dir(m.root)))
	reopened.mu.Lock()
	cached := reopened.itemsLocked("")
	reopened.mu.Unlock()
	if len(cached) != 2 || !cached[0].Community || cached[1].Community {
		t.Fatal("disk fallback lost provenance")
	}
}

func TestCommunityDistFallbackAndNamespaceSpoof(t *testing.T) {
	name := "@magpie-community/fixture"
	archive := packageFixture(t, name, "1.0.0")
	var distRequests atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			distRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"packages": map[string]any{name: map[string]any{"latest": "1.0.0", "versions": map[string]any{"1.0.0": map[string]any{"file": "tgz/fixture.tgz", "integrity": integrity(archive), "manifest": map[string]any{"name": name, "version": "1.0.0"}}}}}})
		case "/tgz/fixture.tgz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := New(t.TempDir())
	m.npmURL = srv.URL
	m.distURL = srv.URL
	m.prepareRuntime = func(context.Context) (string, error) { return "fixture-bun", nil }
	m.dependencies = func(context.Context, string, string) error { return nil }
	if _, e := m.Install(context.Background(), name, inspectFixture); e == nil || distRequests.Load() != 0 {
		t.Fatal("namespace alone allowed community fallback")
	}
	m.trusted[name] = Item{Package: name, Name: "Fixture", Community: true, Providers: []string{"fixture"}}
	x, e := m.Install(context.Background(), name, inspectFixture)
	if e != nil || !x.Community || distRequests.Load() != 1 {
		t.Fatalf("dist fallback: %#v %v", x, e)
	}
}

func TestBunPlatformAndVersion(t *testing.T) {
	for _, tt := range []struct {
		os, arch string
		musl     bool
		want     string
	}{{"windows", "amd64", false, "bun-windows-x64-baseline"}, {"windows", "arm64", false, "bun-windows-aarch64"}, {"linux", "amd64", true, "bun-linux-x64-musl-baseline"}, {"darwin", "amd64", false, "bun-darwin-x64"}} {
		got, e := bunTarget(tt.os, tt.arch, tt.musl)
		if e != nil || got != tt.want || bunSums[got] == "" {
			t.Fatalf("platform: %s %v", got, e)
		}
	}
	for _, v := range []string{"1.4.2", "1.5.0", "2.0.0"} {
		if !compatibleBun(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"1.4.1", "0.9.0", "garbage"} {
		if compatibleBun(v) {
			t.Fatal(v)
		}
	}
}

func TestThirdPartyDiscoveryAndFixedGitHubInstall(t *testing.T) {
	ctx := context.Background()
	packages := map[string][]byte{}
	for _, name := range []string{"opencode-fixture-provider", "opencode-fixture-tools"} {
		source := `export const plugin=async()=>({auth:{provider:"fixture"},provider:{id:"fixture",models(){return {fixture:{id:"fixture"}}}}})`
		if strings.Contains(name, "tools") {
			source = `export const plugin=async()=>({tool:{calculator:{}}})`
		}
		packages[name] = tarFixture(t, map[string]string{"package.json": `{"name":"` + name + `","version":"1.0.0","main":"index.mjs"}`, "index.mjs": source})
	}
	gitPackage := tarFixture(t, map[string]string{"package.json": `{"name":"magpie-fixture-git","version":"1.0.0","main":"index.mjs"}`, "index.mjs": `export default async()=>({auth:{provider:"fixture"}})`})
	sha := strings.Repeat("a", 40)
	var archiveRequested atomic.Bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/-/v1/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"objects": []any{map[string]any{"package": map[string]any{"name": "opencode-fixture-provider", "keywords": []string{"opencode", "provider"}}}, map[string]any{"package": map[string]any{"name": "opencode-fixture-tools", "keywords": []string{"opencode", "provider"}}}}})
		case strings.HasSuffix(r.URL.Path, "/latest"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/latest")
			if a, ok := packages[name]; ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "version": "1.0.0", "main": "index.mjs", "dist": map[string]any{"tarball": srv.URL + "/tgz/" + name, "integrity": integrity(a)}})
				return
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/tgz/"):
			_, _ = w.Write(packages[strings.TrimPrefix(r.URL.Path, "/tgz/")])
		case r.URL.Path == "/search/repositories":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "items": []any{map[string]any{"full_name": "fixture-owner/supplier", "default_branch": "main", "owner": map[string]string{"login": "fixture-owner"}}, map[string]any{"full_name": "magpie-community/ignored", "owner": map[string]string{"login": "magpie-community"}}}})
		case r.URL.Path == "/fixture-owner/supplier/main/package.json":
			_, _ = w.Write([]byte(`{"name":"magpie-fixture-git","version":"1.0.0","main":"index.mjs"}`))
		case r.URL.Path == "/fixture-owner/supplier/main/index.mjs":
			_, _ = w.Write([]byte(`export default async()=>({auth:{provider:"fixture"},provider:{id:"fixture",models(){return {fixture:{id:"fixture"}}}}})`))
		case r.URL.Path == "/repos/fixture-owner/supplier/commits/HEAD":
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
		case r.URL.Path == "/repos/fixture-owner/supplier/tarball/"+sha:
			archiveRequested.Store(true)
			_, _ = w.Write(gitPackage)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := New(t.TempDir())
	m.npmURL = srv.URL
	m.githubURL = srv.URL
	m.rawURL = srv.URL
	npm, e := m.searchNPM(ctx, "")
	if e != nil || len(npm) != 1 || npm[0].Package != "opencode-fixture-provider" || npm[0].Community {
		t.Fatalf("npm supplier filtering: %#v %v", npm, e)
	}
	github, e := m.searchGitHub(ctx)
	if e != nil || len(github) != 1 || github[0].Package != "github:fixture-owner/supplier" || github[0].Community {
		t.Fatalf("github supplier filtering: %#v %v", github, e)
	}
	m.prepareRuntime = func(context.Context) (string, error) { return "fixture-bun", nil }
	m.dependencies = func(context.Context, string, string) error { return nil }
	x, e := m.Install(ctx, github[0].Package, inspectFixture)
	if e != nil || x.Community || !archiveRequested.Load() || x.Package != github[0].Package {
		t.Fatalf("fixed github install: %#v %v", x, e)
	}
}

// Opt-in integration uses a real locally available Bun, a local package and no
// account. It proves lifecycle scripts are skipped and original exports load.
func TestRealBunInstallWithoutLifecycleScripts(t *testing.T) {
	bun := os.Getenv("TOROUTER_TEST_BUN")
	if bun == "" {
		t.Skip("set TOROUTER_TEST_BUN to verify a real Bun runtime")
	}
	f, m := newRegistryFixture(t, "opencode-fixture-provider")
	m.prepareRuntime = func(context.Context) (string, error) { return bun, nil }
	m.dependencies = m.installDependencies
	f.mu.Lock()
	f.archive = tarFixture(t, map[string]string{"package.json": `{"name":"opencode-fixture-provider","version":"1.0.0","exports":{".":{"import":"./index.mjs"}},"dependencies":{"fixture-runtime-dep":"1.0.0"},"trustedDependencies":["fixture-runtime-dep"],"scripts":{"preinstall":"exit 89","install":"exit 90","postinstall":"exit 91"}}`, "index.mjs": `import value from "fixture-runtime-dep"; export default async()=>({auth:{provider:value}})`})
	f.dependency = tarFixture(t, map[string]string{"package.json": `{"name":"fixture-runtime-dep","version":"1.0.0","main":"index.mjs","scripts":{"postinstall":"exit 87"}}`, "index.mjs": `export default "fixture"`})
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	x, e := m.Install(ctx, "opencode-fixture-provider", func(ctx context.Context, x Installed) ([]marketprofile.Provider, error) {
		cmd := command(ctx, x.RuntimePath, "-e", `const p=await import(process.argv[1]); const hooks=await p.default(); if(hooks.auth.provider!=="fixture")process.exit(3);`, x.ModulePath)
		if output, e := cmd.CombinedOutput(); e != nil {
			return nil, errors.New(string(output))
		}
		return inspectFixture(ctx, x)
	})
	if e != nil {
		t.Fatal(e)
	}
	if x.Version != "1.0.0" {
		t.Fatal("real Bun install did not activate")
	}
	original, e := archiveFiles(f.archive, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	installedManifest, e := os.ReadFile(filepath.Join(filepath.Dir(x.ModulePath), "package.json"))
	if e != nil || !bytes.Equal(installedManifest, original["package.json"]) {
		t.Fatal("dependency install modified the original package manifest")
	}
}

func TestRealBunPreparationChecksArchiveAndReusesRuntime(t *testing.T) {
	archivePath := os.Getenv("TOROUTER_TEST_BUN_ARCHIVE")
	if archivePath == "" {
		t.Skip("set TOROUTER_TEST_BUN_ARCHIVE to verify automatic runtime preparation")
	}
	archive, e := os.ReadFile(archivePath)
	if e != nil {
		t.Fatal(e)
	}
	var requests atomic.Int32
	var corrupt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if corrupt.Load() {
			_, _ = w.Write([]byte("fixture corrupt runtime"))
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	m := New(t.TempDir())
	m.bunRelease = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	file, e := m.ensureBun(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if requests.Load() == 0 {
		t.Skip("an existing compatible Bun was selected from PATH")
	}
	if !within(m.root, file) || !usableBun(ctx, file) {
		t.Fatal("runtime was not prepared locally")
	}
	corrupt.Store(true)
	again, e := m.ensureBun(ctx)
	if e != nil || again != file || requests.Load() != 1 {
		t.Fatal("prepared runtime downloaded again", e)
	}
	if e = os.WriteFile(file, []byte("fixture broken exe"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.ensureBun(ctx); e == nil || requests.Load() != 2 {
		t.Fatal("bad runtime archive accepted")
	}
}

func TestActivationWriteFailureAndDamagedStateDoNotReplaceOld(t *testing.T) {
	_, m := newRegistryFixture(t, "opencode-fixture-provider")
	first, e := m.Install(context.Background(), "opencode-fixture-provider", inspectFixture)
	if e != nil {
		t.Fatal(e)
	}
	active := filepath.Join(m.root, "active.json")
	saved, e := os.ReadFile(active)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(active); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(active, 0700); e != nil {
		t.Fatal(e)
	}
	var candidate string
	_, e = m.Install(context.Background(), "opencode-fixture-provider", func(ctx context.Context, x Installed) ([]marketprofile.Provider, error) {
		candidate = x.ModulePath
		return inspectFixture(ctx, x)
	})
	if e == nil {
		t.Fatal("activation write failure ignored")
	}
	old, _ := m.Lookup(first.ID)
	if old.ModulePath != first.ModulePath {
		t.Fatal("failed persistence changed memory activation")
	}
	if _, e = os.Stat(candidate); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("failed activation candidate retained")
	}
	if e = os.Remove(active); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(active, []byte("fixture invalid JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	reopened := New(filepath.Dir(filepath.Dir(m.root)))
	if reopened.stateErr == nil {
		t.Fatal("damaged registration silently accepted")
	}
	if e = reopened.Remove(first.ID); e == nil {
		t.Fatal("damaged registration overwritten")
	}
	if e = os.WriteFile(active, saved, 0600); e != nil {
		t.Fatal(e)
	}
}

func TestSupplierDiscoveryRequiresModelsOrTransportBeyondAuth(t *testing.T) {
	for name, source := range map[string]string{
		"wakatime auth-only":                  `export default async()=>({auth:{provider:"wakatime",methods:[{type:"api"}],loader:async()=>({})}})`,
		"search tools with empty models":      `export default async()=>({config:async(config)=>{config.provider??={};config.provider.search??={name:"Search",models:{}}},auth:{provider:"search"},tool:{search:tool({})}})`,
		"pi tool with provider parameter":     `export default function(pi){pi.registerTool({execute(provider:string){return provider}})}`,
		"commented models":                    `export default async()=>({auth:{provider:"fixture"}}); /* provider:{id:"fake",models(){}} */`,
		"provider shell with plugin API":      `export default async()=>({config:async(config)=>{config.provider.shell={name:"Sign-in manager",api:"plugin://shell"}},auth:{provider:"shell"}})`,
		"search service model facade":         `export default async()=>({config:async(config)=>{config.provider.search={npm:"@ai-sdk/anthropic",api:BASE,searches:true,models:{search:{id:"search"}}}},auth:{provider:"search"}})`,
		"later tool transport not a provider": `export default async()=>({config:async(config)=>{config.provider.shell={name:"Sign-in manager",api:"plugin://shell"}},auth:{provider:"shell"},tool:{search:{npm:"some-tool"}}})`,
	} {
		t.Run(name, func(t *testing.T) {
			if supplierSource([]byte(source)) {
				t.Fatal("non-supplier accepted")
			}
		})
	}
	for name, source := range map[string]string{
		"actual provider hook":              `export default async()=>({auth:{provider:ID},provider:{id:ID,async models(provider,ctx){return models}}})`,
		"OpenCode auth and provider config": `export default async()=>({async config(cfg){cfg.provider??={};cfg.provider[ID]={name:"Fixture",npm:"@ai-sdk/openai",api:"https://example.invalid",models:{}}},auth:{provider:ID}})`,
		"OpenCode static models":            `export default async()=>({config:async(c)=>{c.provider.fixture={models:{fixture:{id:"fixture"}}}},auth:{provider:ID}})`,
	} {
		t.Run(name, func(t *testing.T) {
			if !supplierSource([]byte(source)) {
				t.Fatal("supplier excluded")
			}
		})
	}
	for _, kind := range []string{"tools", "middleware", "agent", "page"} {
		var p manifest
		if e := json.Unmarshal([]byte(`{"name":"fixture","main":"index.mjs","exports":"./index.mjs","magpie":{"`+kind+`":"./index.mjs"}}`), &p); e != nil {
			t.Fatal(e)
		}
		if !p.onlyOtherKind() {
			t.Fatal("executable non-supplier accepted", kind)
		}
		p.Magpie.Providers = []string{"fixture"}
		if p.onlyOtherKind() {
			t.Fatal("explicit mixed supplier excluded", kind)
		}
	}
}

func TestRefreshQueryBypassesTTLAndRecordsQuery(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		switch {
		case r.URL.Path == "/registry":
			_, _ = w.Write([]byte(`{"plugins":[{"package":"@magpie-community/fixture","name":"Fixture","community":true,"providers":["fixture"]}]}`))
		case r.URL.Path == "/-/v1/search":
			if !strings.Contains(r.URL.Query().Get("text"), "fixture") {
				t.Error("query was not forwarded")
			}
			_, _ = w.Write([]byte(`{"objects":[]}`))
		case r.URL.Path == "/search/repositories":
			_, _ = w.Write([]byte(`{"items":[],"total_count":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := New(t.TempDir())
	m.registryURL = srv.URL + "/registry"
	m.npmURL = srv.URL
	m.githubURL = srv.URL
	m.distURL = srv.URL
	if e := m.RefreshQuery(context.Background(), " fixture "); e != nil {
		t.Fatal(e)
	}
	first := count.Load()
	if first == 0 || m.queries["fixture"].IsZero() {
		t.Fatal("refresh/query not recorded")
	}
	if _, ok := m.cache.Sources["query:fixture"]; !ok {
		t.Fatal("query result cache missing")
	}
	if e := m.RefreshQuery(context.Background(), "fixture"); e != nil {
		t.Fatal(e)
	}
	if count.Load() <= first {
		t.Fatal("explicit refresh honored listing TTL")
	}
	before := count.Load()
	out, e := m.Catalogue(context.Background(), "fixture")
	if e != nil || out.Refreshing || count.Load() != before {
		t.Fatal("ordinary listing reissued fresh query")
	}
}

func TestPublishedSupplierSourceFiltering(t *testing.T) {
	root := os.Getenv("TOROUTER_TEST_PUBLIC_SOURCE_ROOT")
	if root == "" {
		t.Skip("set TOROUTER_TEST_PUBLIC_SOURCE_ROOT for captured public package sources")
	}
	for _, tt := range []struct {
		file string
		want bool
	}{{"magpie-search1api/src/index.ts", false}, {"magpie-wakatime/index.mjs", false}, {"pi-web-search/src/index.ts", false}, {"opencode-cloudflare-ai-gateway-auth/index.mjs", true}, {"magpie-api-auto-checkin/index.mjs", false}, {"magpie-commandcode-search/index.mjs", false}, {"magpie-workbuddy-search/index.mjs", false}} {
		t.Run(tt.file, func(t *testing.T) {
			raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(tt.file)))
			if e != nil {
				t.Fatal(e)
			}
			if got := supplierSource(raw); got != tt.want {
				t.Fatalf("supplier source: got %t want %t", got, tt.want)
			}
		})
	}
}

func TestBriefDescriptionStripsReadmeBadgesAndKeepsSourceMetadata(t *testing.T) {
	badge := `[![npm version](https://img.shields.io/npm/v/fixture.svg)](https://www.npmjs.com/package/fixture)`
	raw := badge + "\n\n# Fixture\n\nA [supplier](https://example.invalid/docs) with **model** access.\n\n" + strings.Repeat("README details ", 100)
	want := "A supplier with model access."
	if got := briefDescription(raw, "Fixture"); got != want {
		t.Fatalf("description: %q", got)
	}
	if got := briefDescription(badge+"\n![logo](https://example.invalid/logo.png)", "Fixture Supplier"); got != "Fixture Supplier" {
		t.Fatalf("badge fallback: %q", got)
	}
	if got := briefDescription(strings.Repeat("模型", 200), "Fixture"); len([]rune(got)) != 180 || !strings.HasSuffix(got, "…") {
		t.Fatal("description is not rune-bounded")
	}
	p := manifest{Name: "fixture", Description: raw, Version: "1.0.0", Repository: json.RawMessage(`"https://github.com/fixture-owner/supplier"`)}
	item := p.item("fixture")
	if item.Description != want || item.Repository != "https://github.com/fixture-owner/supplier" || item.Version != "1.0.0" {
		t.Fatal("normalization changed source metadata")
	}
}

func TestOldCandidateCacheCannotBypassCurrentSupplierFilter(t *testing.T) {
	root := t.TempDir()
	marketRoot := filepath.Join(root, "plugins", "market")
	if e := writeJSON(filepath.Join(marketRoot, "catalogue.json"), map[string]any{"sources": map[string][]Item{"npm": {{Package: "opencode-fixture-tool", Name: "Fixture tool"}}}, "updated_at": time.Now()}); e != nil {
		t.Fatal(e)
	}
	m := New(root)
	m.mu.Lock()
	items := m.itemsLocked("")
	m.mu.Unlock()
	for _, item := range items {
		if item.Package == "opencode-fixture-tool" {
			t.Fatal("old unclassified cache bypassed current filter")
		}
	}
	if len(items) == 0 {
		t.Fatal("old cache discarded cold-start suppliers")
	}
}
