package pluginmarket

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"torouter/internal/marketprofile"
)

var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var distFile = regexp.MustCompile(`^tgz/[A-Za-z0-9._-]+\.tgz$`)
var commitID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

type distVersion struct {
	File      string          `json:"file"`
	Integrity string          `json:"integrity"`
	Manifest  json.RawMessage `json:"manifest"`
}

func (m *Manager) distManifest(ctx context.Context, name, version string) (manifest, error) {
	var p manifest
	if !strings.HasPrefix(name, "@magpie-community/") {
		return p, errors.New("不属于社区分发范围")
	}
	b, e := m.get(ctx, m.distURL+"/index.json", 32<<20)
	if e != nil {
		return p, e
	}
	var index struct {
		Packages map[string]struct {
			Latest   string                 `json:"latest"`
			Versions map[string]distVersion `json:"versions"`
		} `json:"packages"`
	}
	if e = json.Unmarshal(b, &index); e != nil {
		return p, e
	}
	pkg, ok := index.Packages[name]
	if !ok {
		return p, errors.New("社区分发没有该包")
	}
	if version == "latest" {
		version = pkg.Latest
	}
	v, ok := pkg.Versions[version]
	if !ok || !distFile.MatchString(v.File) {
		return p, errors.New("社区分发没有所需版本")
	}
	if e = json.Unmarshal(v.Manifest, &p); e != nil {
		return p, e
	}
	if p.Name != name || p.Version != version {
		return p, errors.New("社区包信息与索引不一致")
	}
	p.Dist.Tarball = m.distURL + "/" + v.File
	p.Dist.Integrity = v.Integrity
	return p, nil
}

func verifyIntegrity(b []byte, sri string) error {
	sum := sha512.Sum512(b)
	for _, token := range strings.Fields(sri) {
		if !strings.HasPrefix(token, "sha512-") {
			continue
		}
		expected, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "sha512-"))
		if e == nil && len(expected) == len(sum) {
			var difference byte
			for i := range sum {
				difference |= sum[i] ^ expected[i]
			}
			if difference == 0 {
				return nil
			}
		}
	}
	return errors.New("下载的供应商包未通过 SHA-512 完整性校验")
}
func (m *Manager) npmArchive(ctx context.Context, p manifest, limit int64) ([]byte, error) {
	if !strings.HasPrefix(p.Dist.Tarball, "https://") && !strings.HasPrefix(p.Dist.Tarball, "http://") {
		return nil, errors.New("供应商包下载地址无效")
	}
	b, e := m.get(ctx, p.Dist.Tarball, limit)
	if e != nil {
		return nil, e
	}
	if e = verifyIntegrity(b, p.Dist.Integrity); e != nil {
		return nil, e
	}
	return b, nil
}
func (m *Manager) npmSource(ctx context.Context, p manifest) (map[string][]byte, error) {
	b, e := m.npmArchive(ctx, p, 16<<20)
	if e != nil {
		return nil, e
	}
	return archiveFiles(b, 32<<20)
}

func splitSpec(spec string) (name, version string, err error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "github:") {
		repo := strings.TrimPrefix(spec, "github:")
		if !gitName.MatchString(repo) {
			return "", "", errors.New("GitHub包名无效")
		}
		return "github:" + repo, "latest", nil
	}
	name, version = spec, "latest"
	if at := strings.LastIndex(spec, "@"); at > 0 {
		name, version = spec[:at], spec[at+1:]
	}
	if !npmName.MatchString(name) || (version != "latest" && !exactVersion.MatchString(version)) {
		return "", "", errors.New("供应商包名或版本无效")
	}
	return name, version, nil
}

func (m *Manager) Install(ctx context.Context, packageName string, inspect Inspector) (Installed, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if inspect == nil {
		return Installed{}, errors.New("缺少供应商接口探测")
	}
	name, version, e := splitSpec(packageName)
	if e != nil {
		return Installed{}, e
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()
	m.mu.RLock()
	stateErr := m.stateErr
	listing, community := m.trusted[name]
	m.mu.RUnlock()
	if stateErr != nil {
		return Installed{}, stateErr
	}
	var p manifest
	var archive []byte
	if strings.HasPrefix(name, "github:") {
		p, archive, e = m.githubArchive(ctx, strings.TrimPrefix(name, "github:"))
	} else {
		p, e = m.npmManifest(ctx, name, version)
		if e == nil {
			archive, e = m.npmArchive(ctx, p, 64<<20)
		}
		// Fallback is restricted by registry membership, never just the scope.
		if e != nil && community {
			p, e = m.distManifest(ctx, name, version)
			if e == nil {
				archive, e = m.npmArchive(ctx, p, 64<<20)
			}
		}
	}
	if e != nil {
		return Installed{}, fmt.Errorf("获取供应商包失败: %w", e)
	}
	if p.onlyOtherKind() {
		return Installed{}, errors.New("该包不提供供应商入口")
	}
	if e = ctx.Err(); e != nil {
		return Installed{}, e
	}
	runtimePath, e := m.prepareRuntime(ctx)
	if e != nil {
		return Installed{}, e
	}
	id := marketprofile.PackageID(name)
	versions := filepath.Join(m.root, "packages", id, "versions")
	if e = os.MkdirAll(versions, 0700); e != nil {
		return Installed{}, e
	}
	// A unique final path is used even for a reinstall of the same version. The
	// inspector/worker can retain this path; an update never modifies old code.
	dir, e := os.MkdirTemp(versions, "candidate-")
	if e != nil {
		return Installed{}, e
	}
	if !within(versions, dir) {
		return Installed{}, errors.New("供应商安装目录无效")
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(dir)
		}
	}()
	if e = extractArchive(archive, dir, 256<<20); e != nil {
		return Installed{}, e
	}
	b, e := os.ReadFile(filepath.Join(dir, "package.json"))
	if e != nil {
		return Installed{}, errors.New("供应商包缺少package.json")
	}
	var actual manifest
	if e = json.Unmarshal(b, &actual); e != nil {
		return Installed{}, errors.New("供应商包package.json无效")
	}
	if actual.Name != p.Name || actual.Version != p.Version {
		return Installed{}, errors.New("下载内容的包名或版本不一致")
	}
	if actual.onlyOtherKind() {
		return Installed{}, errors.New("该包不提供供应商入口")
	}
	if e = m.dependencies(ctx, runtimePath, dir); e != nil {
		return Installed{}, e
	}
	module, e := resolveEntry(dir, actual)
	if e != nil {
		return Installed{}, e
	}
	x := Installed{ID: id, Package: name, Name: actual.Name, Version: actual.Version, Description: actual.Description, Icon: icon(actual.Magpie.Icon, false), Community: community, ModulePath: module, RuntimePath: runtimePath}
	x.Description = briefDescription(x.Description, x.Name)
	if community {
		x.Name = listing.Name
		if listing.Icon != "" {
			x.Icon = listing.Icon
		}
		if listing.Description != "" {
			x.Description = listing.Description
		}
	}
	providers, e := inspect(ctx, x)
	if e != nil {
		return Installed{}, fmt.Errorf("供应商接口探测失败: %w", e)
	}
	if len(providers) == 0 {
		return Installed{}, errors.New("该包没有可用供应商接口")
	}
	seen := map[string]bool{}
	for _, p := range providers {
		if !marketprofile.ValidProviderID(p.ID) || seen[p.ID] {
			return Installed{}, errors.New("供应商接口返回无效或重复标识")
		}
		seen[p.ID] = true
	}
	x.Providers = providers
	if e = ctx.Err(); e != nil {
		return Installed{}, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]Installed, len(m.installed)+1)
	for k, v := range m.installed {
		next[k] = v
	}
	next[x.ID] = cloneInstalled(x)
	if e = m.saveInstalled(next); e != nil {
		return Installed{}, e
	}
	m.installed = next
	activated = true
	return cloneInstalled(x), nil
}

func (m *Manager) githubArchive(ctx context.Context, repo string) (manifest, []byte, error) {
	var p manifest
	b, e := m.get(ctx, m.githubURL+"/repos/"+repo+"/commits/HEAD", 2<<20)
	if e != nil {
		return p, nil, e
	}
	var c struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(b, &c) != nil || !commitID.MatchString(c.SHA) {
		return p, nil, errors.New("GitHub未返回有效固定版本")
	}
	// The immutable commit is fetched from GitHub over TLS, rather than a
	// caller-supplied archive URL or a moving branch after metadata inspection.
	b, e = m.get(ctx, m.githubURL+"/repos/"+repo+"/tarball/"+url.PathEscape(c.SHA), 64<<20)
	if e != nil {
		return p, nil, e
	}
	files, e := archiveFiles(b, 256<<20)
	if e != nil {
		return p, nil, e
	}
	if e = json.Unmarshal(files["package.json"], &p); e != nil {
		return p, nil, errors.New("GitHub仓库没有有效package.json")
	}
	if !npmName.MatchString(p.Name) || p.Version == "" {
		return p, nil, errors.New("GitHub包信息无效")
	}
	return p, b, nil
}
