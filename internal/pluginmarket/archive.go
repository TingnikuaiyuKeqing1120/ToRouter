package pluginmarket

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > limit {
		return nil, errors.New("市场文件超过大小限制")
	}
	return b, nil
}
func visitArchive(b []byte, limit int64, visit func(string, []byte, os.FileMode) error) error {
	gz, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return errors.New("供应商包不是有效tgz")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var root string
	seen := map[string]bool{}
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("供应商压缩包损坏")
		}
		count++
		if count > 30000 {
			return errors.New("供应商包文件过多")
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
			return errors.New("供应商包包含不安全路径")
		}
		clean := path.Clean(name)
		if clean != name || clean == ".." || strings.HasPrefix(clean, "../") {
			return errors.New("供应商包包含越界路径")
		}
		parts := strings.SplitN(clean, "/", 2)
		for _, part := range strings.Split(clean, "/") {
			if strings.TrimRight(part, " .") != part {
				return errors.New("供应商包包含不安全路径")
			}
		}
		if root == "" {
			root = parts[0]
		}
		if parts[0] != root {
			return errors.New("供应商包必须只有一个根目录")
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return errors.New("供应商包不能包含链接或特殊文件")
		}
		if len(parts) == 1 {
			if h.Typeflag != tar.TypeDir {
				return errors.New("供应商包缺少根目录")
			}
			continue
		}
		name = parts[1]
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if seen[name] {
			return errors.New("供应商包包含重复文件")
		}
		seen[name] = true
		if h.Size < 0 || h.Size > limit-total {
			return errors.New("供应商包解压后过大")
		}
		total += h.Size
		data, e := readLimited(tr, h.Size)
		if e != nil {
			return e
		}
		if int64(len(data)) != h.Size {
			return errors.New("供应商包文件不完整")
		}
		if e = visit(name, data, os.FileMode(h.Mode)&0777); e != nil {
			return e
		}
	}
	// Consume the gzip trailer too; tar EOF alone does not verify its CRC.
	if trailing, err := io.Copy(io.Discard, io.LimitReader(gz, (1<<20)+1)); err != nil || trailing > 1<<20 {
		return errors.New("供应商压缩包校验失败")
	}
	return nil
}
func archiveFiles(b []byte, limit int64) (map[string][]byte, error) {
	out := map[string][]byte{}
	e := visitArchive(b, limit, func(name string, b []byte, _ os.FileMode) error { out[name] = b; return nil })
	return out, e
}
func extractArchive(b []byte, dir string, limit int64) error {
	return visitArchive(b, limit, func(name string, b []byte, mode os.FileMode) error {
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if !within(dir, dest) {
			return errors.New("供应商包包含越界路径")
		}
		if e := os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		return os.WriteFile(dest, b, mode|0600)
	})
}

func packageEntry(p manifest) (string, error) {
	var pick func(any) string
	pick = func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case []any:
			for _, v := range x {
				if s := pick(v); s != "" {
					return s
				}
			}
		case map[string]any:
			if v, ok := x["."]; ok {
				return pick(v)
			}
			for _, key := range []string{"bun", "import", "default", "node", "require"} {
				if s := pick(x[key]); s != "" {
					return s
				}
			}
		}
		return ""
	}
	entry := ""
	var exports any
	if len(p.Exports) > 0 && json.Unmarshal(p.Exports, &exports) == nil {
		entry = pick(exports)
	}
	if entry == "" {
		entry = p.Module
	}
	if entry == "" {
		entry = p.Main
	}
	if entry == "" {
		entry = "index.js"
	}
	entry = strings.TrimPrefix(entry, "./")
	if entry == "" || strings.ContainsAny(entry, "\\:\x00") || strings.HasPrefix(entry, "/") || entry != path.Clean(entry) || entry == ".." || strings.HasPrefix(entry, "../") {
		return "", errors.New("供应商模块入口越界或无效")
	}
	return entry, nil
}
func entryCandidates(entry string) []string {
	return []string{entry, entry + ".js", entry + ".mjs", entry + ".cjs", entry + ".ts", entry + ".mts", entry + "/index.js", entry + "/index.mjs", entry + "/index.ts"}
}
func entryInFiles(p manifest, files map[string][]byte) (string, error) {
	entry, e := packageEntry(p)
	if e != nil {
		return "", e
	}
	for _, name := range entryCandidates(entry) {
		if _, ok := files[name]; ok {
			return name, nil
		}
	}
	return "", errors.New("供应商包没有可加载入口")
}
func resolveEntry(dir string, p manifest) (string, error) {
	entry, e := packageEntry(p)
	if e != nil {
		return "", e
	}
	for _, name := range entryCandidates(entry) {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if !within(dir, file) {
			continue
		}
		info, e := os.Stat(file)
		if e != nil || !info.Mode().IsRegular() {
			continue
		}
		resolved, e := filepath.EvalSymlinks(file)
		if e != nil || !within(dir, resolved) {
			return "", errors.New("供应商模块入口越界")
		}
		return resolved, nil
	}
	return "", errors.New("供应商包没有可加载入口")
}
