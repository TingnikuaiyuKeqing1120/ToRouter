package pluginmarket

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Checksums pinned in Magpie b437e4adb4dba82446e0d625b040a8e2d95004b4,
// internal/plugin/bun.go, from Bun's official SHASUMS256.txt.
const bunVersion = "1.4.2"

var bunSums = map[string]string{
	"bun-darwin-aarch64":          "90987a3a16d7db556d886ac3d551e7b6d3edf0a1cf43acaed622e8676be1d12f",
	"bun-darwin-x64":              "80520d7e17526308c9185d261679ac6d27798d3803a0e9f7ff9121ab8affb012",
	"bun-linux-aarch64":           "54328bbc2d9c8e0c9f892c544d66c57a83b84139e34909e5ee81758f1ac8fda7",
	"bun-linux-x64-baseline":      "c678040f14fe0440eb839d37cbd0ce4c051a32da72806ac97de6a6aab6bf728f",
	"bun-linux-aarch64-musl":      "71760b6c8ea30623b81a4907cb815d48e2ea266f2e73e751534a44a0607950df",
	"bun-linux-x64-musl-baseline": "76e1db84e98f22f78de0a87e309bfbbf297732847f9720db36750646c85c8c18",
	"bun-windows-x64-baseline":    "78c221c2376f79731ccf4e4af0b3bb46d81fefa3296c5abee09ad8a1b21e68c6",
	"bun-windows-aarch64":         "a7a16b876a305fd1029c66dbd27007b4f6112ae896532f675878731a21e50cfd",
}
var runtimeMu sync.Mutex

func bunTarget(goos, arch string, musl bool) (string, error) {
	a := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	if a == "" {
		return "", errors.New("此架构没有可用的Bun运行时")
	}
	if goos != "windows" && goos != "linux" && goos != "darwin" {
		return "", errors.New("此系统没有可用的Bun运行时")
	}
	target := "bun-" + goos + "-" + a
	if goos == "linux" && musl {
		target += "-musl"
	}
	if a == "x64" && goos != "darwin" {
		target += "-baseline"
	}
	return target, nil
}
func compatibleBun(version string) bool {
	v := strings.Split(strings.TrimSpace(version), ".")
	if len(v) < 3 {
		return false
	}
	wanted := []int{1, 4, 2}
	for i, w := range wanted {
		part := strings.SplitN(v[i], "-", 2)[0]
		got, e := strconv.Atoi(part)
		if e != nil {
			return false
		}
		if got > w {
			return true
		}
		if got < w {
			return false
		}
	}
	return true
}
func command(ctx context.Context, program string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program, args...)
	hideWindow(cmd)
	return cmd
}
func usableBun(ctx context.Context, file string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out limitedOutput
	cmd := command(ctx, file, "--version")
	cmd.Stdout = &out
	cmd.Stderr = &out
	return cmd.Run() == nil && compatibleBun(string(out.b))
}
func (m *Manager) ensureBun(ctx context.Context) (string, error) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if available, e := exec.LookPath("bun"); e == nil && usableBun(ctx, available) {
		absolute, e := filepath.Abs(available)
		return absolute, e
	}
	exe := "bun"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	dest := filepath.Join(m.root, "runtime", bunVersion, exe)
	if usableBun(ctx, dest) {
		return dest, nil
	}
	musl := false
	if runtime.GOOS == "linux" {
		paths, _ := filepath.Glob("/lib/ld-musl-*.so.1")
		musl = len(paths) > 0
	}
	target, e := bunTarget(runtime.GOOS, runtime.GOARCH, musl)
	if e != nil {
		return "", e
	}
	want := bunSums[target]
	if want == "" {
		return "", errors.New("Bun运行时缺少固定校验值")
	}
	b, e := m.get(ctx, m.bunRelease+"/bun-v"+bunVersion+"/"+target+".zip", 200<<20)
	if e != nil {
		return "", fmt.Errorf("准备Bun运行时失败: %w", e)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != want {
		return "", errors.New("Bun运行时完整性校验失败")
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		return "", e
	}
	expected := target + "/" + exe
	for _, file := range z.File {
		if file.Name != expected || !file.Mode().IsRegular() {
			continue
		}
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return "", e
		}
		r, e := file.Open()
		if e != nil {
			return "", e
		}
		data, e := readLimited(r, 200<<20)
		_ = r.Close()
		if e != nil {
			return "", e
		}
		f, e := os.CreateTemp(filepath.Dir(dest), ".bun-*"+filepath.Ext(exe))
		if e != nil {
			return "", e
		}
		temp := f.Name()
		defer os.Remove(temp)
		_, e = f.Write(data)
		if e == nil {
			e = f.Chmod(0700)
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return "", e
		}
		if !usableBun(ctx, temp) {
			return "", errors.New("下载的Bun运行时无法启动")
		}
		if e = os.Rename(temp, dest); e != nil {
			return "", e
		}
		return dest, nil
	}
	return "", errors.New("Bun压缩包没有对应程序")
}

type limitedOutput struct{ b []byte }

func (w *limitedOutput) Write(b []byte) (int, error) {
	n := len(b)
	if room := 8192 - len(w.b); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		w.b = append(w.b, b...)
	}
	return n, nil
}
func (m *Manager) installDependencies(ctx context.Context, bun, dir string) error {
	cmd := command(ctx, bun, "install", "--ignore-scripts", "--no-progress", "--no-audit")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BUN_INSTALL_CACHE_DIR="+filepath.Join(m.root, "dependency-cache"), "BUN_CONFIG_REGISTRY="+m.npmURL)
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Run(); e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("供应商依赖安装失败（已禁用安装脚本）")
	}
	return nil
}

var _ io.Writer = (*limitedOutput)(nil)
