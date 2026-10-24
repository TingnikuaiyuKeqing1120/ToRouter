package pluginhost

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"torouter/internal/compatprofile"
)

// Only the fixed reviewed code is distributed; no registry/latest, npm scripts,
// arbitrary module path or shell command is accepted by the installation API.
//
//go:embed packages/qoder/index.mjs packages/qoder/contract.mjs packages/qoder/LICENSE packages/qoder/LICENSE-CLIProxyAPI
//go:embed packages/workbuddy/index.mjs packages/workbuddy/contract.mjs packages/workbuddy/LICENSE
//go:embed packages/minimax/index.mjs packages/minimax/contract.mjs packages/minimax/LICENSE
var packageFiles embed.FS

var ErrNotInstalled = errors.New("兼容层未安装或文件已变化，请在本机插件库重新安装")

type Installation struct {
	mu   sync.Mutex
	root string
}

type RuntimeStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type runtimeConfig struct {
	Path, Version, Hash string
}

func NewInstallation(dataDir string) *Installation {
	return &Installation{root: filepath.Join(dataDir, "plugins", "compatible")}
}

func shippedFiles() map[string][]byte {
	files := map[string][]byte{"host.mjs": Bridge}
	for _, pkg := range []string{"qoder", "workbuddy", "minimax"} {
		names := []string{"index.mjs", "contract.mjs", "LICENSE"}
		if pkg == "qoder" {
			names = append(names, "LICENSE-CLIProxyAPI")
		}
		for _, name := range names {
			data, err := packageFiles.ReadFile("packages/" + pkg + "/" + name)
			if err != nil {
				panic("missing bundled compatible file")
			}
			files[filepath.Join(pkg, name)] = data
		}
	}
	return files
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (i *Installation) read() (runtimeConfig, error) {
	if i == nil || i.root == "" {
		return runtimeConfig{}, ErrNotInstalled
	}
	raw, err := os.ReadFile(filepath.Join(i.root, "runtime.json"))
	var cfg runtimeConfig
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &cfg) != nil || !filepath.IsAbs(cfg.Path) || cfg.Hash == "" {
		return cfg, ErrNotInstalled
	}
	return cfg, nil
}

// Status never launches a program or exposes a local path in a public listing.
func (i *Installation) Status() RuntimeStatus {
	if i == nil {
		return RuntimeStatus{Reason: "此进程未配置兼容层"}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	cfg, err := i.read()
	if err != nil {
		return RuntimeStatus{Reason: "需要安装兼容层（Node.js 24）"}
	}
	if hash, err := fileDigest(cfg.Path); err != nil || hash != cfg.Hash {
		return RuntimeStatus{Reason: "Node.js 已变化或不可用，请重新检测安装兼容层"}
	}
	for path, expected := range shippedFiles() {
		got, err := os.ReadFile(filepath.Join(i.root, path))
		if err != nil || digest(got) != digest(expected) {
			return RuntimeStatus{Reason: "兼容层文件需要更新或修复"}
		}
	}
	return RuntimeStatus{Installed: true, Version: cfg.Version}
}

// Install is local-console-only: selecting a trusted executable is code
// execution authority, not ordinary remote/MCP configuration authority.
// Empty path detects Node on PATH; it never downloads an unverified runtime.
func (i *Installation) Install(ctx context.Context, runtimePath string) error {
	if i == nil {
		return ErrNotInstalled
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if runtimePath == "" {
		var err error
		runtimePath, err = exec.LookPath("node")
		if err != nil {
			return errors.New("未找到 Node.js 24，请先安装，或指定已安装的 node 可执行文件")
		}
	}
	path, err := filepath.Abs(runtimePath)
	if err != nil {
		return errors.New("运行时路径无效")
	}
	if base := strings.ToLower(filepath.Base(path)); base != "node" && base != "node.exe" {
		return errors.New("请选择受信任的 Node.js node 可执行文件")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	hideWindow(cmd)
	cmd.Env = cleanEnvironment()
	var output strings.Builder
	cmd.Stdout = &limitedWriter{writer: &output, remaining: 256}
	if err = cmd.Run(); err != nil {
		return errors.New("Node.js 检查失败，请确认可执行文件可用")
	}
	version := strings.TrimSpace(output.String())
	if !regexp.MustCompile(`^v24\.\d+\.\d+$`).MatchString(version) {
		return errors.New("此兼容层目前支持 Node.js 24；其他运行时尚未验证")
	}
	hash, err := fileDigest(path)
	if err != nil {
		return errors.New("无法核验 Node.js 文件")
	}
	if err = os.MkdirAll(i.root, 0700); err != nil {
		return err
	}
	// Each module is written completely before the activation record. Existing
	// workers already loaded their code; new launches take this same lock.
	for name, data := range shippedFiles() {
		if err = os.MkdirAll(filepath.Dir(filepath.Join(i.root, name)), 0700); err != nil {
			return err
		}
		if err = atomicWrite(filepath.Join(i.root, name), data); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(runtimeConfig{Path: path, Hash: hash, Version: version})
	return atomicWrite(filepath.Join(i.root, "runtime.json"), raw)
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errors.New("runtime output limit")
	}
	w.remaining -= len(b)
	return w.writer.Write(b)
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".install-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
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
	return os.Rename(f.Name(), path)
}

func (i *Installation) Launch(ctx context.Context, p compatprofile.Profile, state string) (*Worker, error) {
	if i == nil {
		return nil, ErrNotInstalled
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	cfg, err := i.read()
	if err != nil {
		return nil, err
	}
	if hash, err := fileDigest(cfg.Path); err != nil || hash != cfg.Hash {
		return nil, ErrNotInstalled
	}
	for path, expected := range shippedFiles() {
		got, err := os.ReadFile(filepath.Join(i.root, path))
		if err != nil || digest(got) != digest(expected) {
			return nil, ErrNotInstalled
		}
	}
	w, err := Start(cfg.Path, filepath.Join(i.root, "host.mjs"))
	if err != nil {
		return nil, ErrNotInstalled
	}
	input := map[string]any{"protocol": 1, "module_path": filepath.Join(i.root, p.Package, "index.mjs"), "provider_id": p.ID, "export_name": p.Export, "methods": []int{0}}
	if state != "" {
		input["auth"] = json.RawMessage(state)
	}
	raw, err := w.Invoke(ctx, "init", input, nil)
	var descriptor struct{ ID, API, Npm string }
	if err != nil || json.Unmarshal(raw, &descriptor) != nil || descriptor.ID != p.ID || descriptor.API != p.API || descriptor.Npm != p.NPM() {
		w.Close()
		return nil, errors.New("固定插件加载验证失败，请修复兼容层")
	}
	return w, nil
}
