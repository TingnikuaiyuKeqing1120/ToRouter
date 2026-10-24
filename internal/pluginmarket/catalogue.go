package pluginmarket

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The pinned community registry is a cold-start fallback, not a claim that a
// supplier has passed this application's runtime or real-account acceptance.
// Source: magpie-community/plugins a1689b1b6feb37f9de4c213fee276bc9be4676cb.
//
//go:embed registry.json
var builtinRegistry []byte

var npmName = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)
var gitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var providerBlock = regexp.MustCompile(`\bprovider\s*:\s*(?:\[\s*)?\{`)
var authBlock = regexp.MustCompile(`\bauth\s*:\s*\{`)
var providerIDField = regexp.MustCompile(`\bid\s*:`)
var modelHook = regexp.MustCompile(`\bmodels\s*(?:\(|:)`)
var configHook = regexp.MustCompile(`\bconfig\s*(?:\(|:)`)
var providerMember = regexp.MustCompile(`\b[A-Za-z_$][A-Za-z0-9_$]*\s*(?:\.\s*provider|\[\s*["']provider["']\s*\])`)
var transportField = regexp.MustCompile(`\bnpm\s*:`)
var nonemptyModels = regexp.MustCompile(`\bmodels\s*:\s*(?:\{\s*[^}\s]|[A-Za-z_$])`)
var searchOnly = regexp.MustCompile(`\bsearches\s*:\s*true\b`)
var providerAssignment = regexp.MustCompile(`^\s*(?:(?:\[[^\]\n]+\]|\.[A-Za-z_$][A-Za-z0-9_$]*)\s*)?(?:\?\?=|\|\|=|=)\s*\{`)

func parseRegistry(b []byte) ([]Item, error) {
	var registry struct {
		Plugins []struct {
			Package   string            `json:"package"`
			Name      string            `json:"name"`
			Kind      string            `json:"kind"`
			Icon      string            `json:"icon"`
			Providers []string          `json:"providers"`
			Community bool              `json:"community"`
			Summary   map[string]string `json:"summary"`
		} `json:"plugins"`
	}
	if e := json.Unmarshal(b, &registry); e != nil {
		return nil, e
	}
	out := []Item{}
	seen := map[string]bool{}
	for _, v := range registry.Plugins {
		if !v.Community || v.Kind != "" || len(v.Providers) == 0 || !npmName.MatchString(v.Package) || seen[v.Package] {
			continue
		}
		seen[v.Package] = true
		if v.Name == "" {
			v.Name = v.Package
		}
		description := v.Summary["zh"]
		if description == "" {
			description = v.Summary["en"]
		}
		out = append(out, Item{Package: v.Package, Name: v.Name, Description: briefDescription(description, v.Name), Icon: icon(v.Icon, true), Community: true, Providers: v.Providers})
	}
	if len(out) == 0 {
		return nil, errors.New("社区清单没有供应商")
	}
	return out, nil
}

type manifest struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	Main        string          `json:"main"`
	Module      string          `json:"module"`
	Exports     json.RawMessage `json:"exports"`
	Repository  json.RawMessage `json:"repository"`
	Magpie      struct {
		Icon       string          `json:"icon"`
		Providers  []string        `json:"providers"`
		Middleware json.RawMessage `json:"middleware"`
		Agent      json.RawMessage `json:"agent"`
		Tools      json.RawMessage `json:"tools"`
		Page       json.RawMessage `json:"page"`
	} `json:"magpie"`
	Dist struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func (p manifest) item(pkg string) Item {
	return Item{Package: pkg, Name: p.Name, Description: briefDescription(p.Description, p.Name), Version: p.Version, Icon: icon(p.Magpie.Icon, false), Providers: p.Magpie.Providers, Repository: repository(p.Repository)}
}
func (p manifest) onlyOtherKind() bool {
	if len(p.Magpie.Providers) > 0 {
		return false
	}
	for _, raw := range []json.RawMessage{p.Magpie.Middleware, p.Magpie.Agent, p.Magpie.Tools, p.Magpie.Page} {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		switch value := value.(type) {
		case nil:
		case string:
			if strings.TrimSpace(value) != "" {
				return true
			}
		case bool:
			if value {
				return true
			}
		case map[string]any:
			if len(value) > 0 {
				return true
			}
		case []any:
			if len(value) > 0 {
				return true
			}
		}
	}
	return false
}

// Auth is also used by tools to store their keys. A supplier must additionally
// declare a model hook or register a usable model/transport configuration. This
// is discovery evidence only; installation still probes the actual exports.
func supplierSource(raw []byte) bool {
	source := withoutComments(string(raw))
	auth := authBlock.MatchString(source)
	for _, match := range providerBlock.FindAllStringIndex(source, -1) {
		block := objectSource(source, match[1]-1)
		if !searchOnly.MatchString(block) && modelHook.MatchString(block) && (providerIDField.MatchString(block) || auth) {
			return true
		}
	}
	if !auth || !configHook.MatchString(source) {
		return false
	}
	for _, match := range providerMember.FindAllStringIndex(source, -1) {
		assignment := providerAssignment.FindStringIndex(source[match[1]:])
		if assignment == nil {
			continue
		}
		block := objectSource(source, match[1]+assignment[1]-1)
		if !searchOnly.MatchString(block) && (transportField.MatchString(block) || nonemptyModels.MatchString(block)) {
			return true
		}
	}
	return false
}

// Keep each registration's evidence inside its own object; a later tool's API
// fields must not turn an earlier sign-in-only provider shell into a supplier.
func objectSource(source string, start int) string {
	depth := 0
	var quote byte
	for i := start; i < len(source); i++ {
		c := source[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == '{' {
			depth++
		}
		if c == '}' {
			depth--
			if depth == 0 {
				return source[start : i+1]
			}
		}
	}
	return ""
}

var descriptionBadge = regexp.MustCompile(`\[\s*!\[[^\]]*\]\([^)]*\)\s*\]\([^)]*\)`)
var descriptionImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
var descriptionLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
var descriptionHTML = regexp.MustCompile(`<[^>]*>`)
var descriptionURL = regexp.MustCompile(`https?://\S+`)

func briefDescription(raw, fallback string) string {
	s := descriptionBadge.ReplaceAllString(raw, "")
	s = descriptionImage.ReplaceAllString(s, "")
	s = descriptionLink.ReplaceAllString(s, "$1")
	s = descriptionHTML.ReplaceAllString(s, "")
	s = descriptionURL.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var lines []string
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "" {
			if len(lines) > 0 {
				break
			}
			continue
		}
		lines = append(lines, line)
	}
	s = strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	if s == "" {
		s = strings.Join(strings.Fields(fallback), " ")
	}
	runes := []rune(s)
	if len(runes) > 180 {
		s = string(runes[:179]) + "…"
	}
	return s
}

// Preserve string literals while removing comments so a description of a
// provider, or a commented-out example, cannot supply the discovery evidence.
func withoutComments(source string) string {
	out := []byte(source)
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c != '/' || i+1 >= len(out) {
			continue
		}
		if out[i+1] == '/' {
			out[i] = ' '
			i++
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		} else if out[i+1] == '*' {
			out[i] = ' '
			i++
			out[i] = ' '
			for i+1 < len(out) {
				if out[i] == '*' && out[i+1] == '/' {
					out[i] = ' '
					i++
					out[i] = ' '
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		}
	}
	return string(out)
}
func icon(s string, registry bool) string {
	s = strings.TrimSpace(s)
	if len(s) > 3<<19 {
		return ""
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "data:image/") {
		return s
	}
	if registry && regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(s) {
		return s
	}
	return ""
}
func repository(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var v struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &v)
		s = v.URL
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "git+"), ".git")
	s = strings.Replace(s, "git://", "https://", 1)
	if strings.HasPrefix(s, "github:") {
		s = "https://github.com/" + strings.TrimPrefix(s, "github:")
	}
	if gitName.MatchString(s) {
		s = "https://github.com/" + s
	}
	if !strings.HasPrefix(s, "https://") {
		return ""
	}
	return s
}
func npmPath(name string) string { return strings.Replace(url.PathEscape(name), "%40", "@", 1) }
func (m *Manager) npmManifest(ctx context.Context, name, version string) (manifest, error) {
	var p manifest
	b, e := m.get(ctx, m.npmURL+"/"+npmPath(name)+"/"+url.PathEscape(version), 2<<20)
	if e == nil {
		e = json.Unmarshal(b, &p)
	}
	if e == nil && (p.Name != name || p.Version == "" || (version != "latest" && p.Version != version)) {
		e = errors.New("npm包信息与请求不一致")
	}
	return p, e
}

func (m *Manager) refresh(ctx context.Context, query string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	type result struct {
		key   string
		items []Item
		err   error
	}
	results := make(chan result, 3)
	go func() {
		b, e := m.get(ctx, m.registryURL, 2<<20)
		var list []Item
		if e == nil {
			list, e = parseRegistry(b)
		}
		if e == nil {
			m.enrich(ctx, list)
		}
		results <- result{"community", list, e}
	}()
	go func() {
		list, e := m.searchNPM(ctx, query)
		key := "npm"
		if query != "" {
			key = "query:" + query
		}
		results <- result{key, list, e}
	}()
	go func() { list, e := m.searchGitHub(ctx); results <- result{"github", list, e} }()
	var errs []error
	m.mu.Lock()
	m.refreshing = true
	m.mu.Unlock()
	for range 3 {
		r := <-results
		m.mu.Lock()
		if r.err == nil {
			m.cache.Sources[r.key] = r.items
			if r.key == "community" {
				m.trusted = map[string]Item{}
				for _, it := range r.items {
					m.trusted[it.Package] = it
				}
			}
		} else {
			errs = append(errs, r.err)
		}
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshing = false
	if len(errs) == 0 {
		m.cache.UpdatedAt = time.Now()
		m.warning = ""
	} else {
		m.warning = "部分市场来源暂时不可用，已保留缓存"
		m.attempt = time.Now().Add(-9 * time.Minute)
	}
	if err := writeJSON(m.root+"/catalogue.json", m.cache); err != nil {
		errs = append(errs, err)
		m.warning = "市场缓存保存失败"
	}
	return errors.Join(errs...)
}
func (m *Manager) enrich(ctx context.Context, items []Item) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			p, e := m.npmManifest(ctx, items[i].Package, "latest")
			if e != nil {
				if d, e := m.distManifest(ctx, items[i].Package, "latest"); e == nil {
					p = d
				} else {
					return
				}
			}
			items[i].Version = p.Version
			items[i].Repository = repository(p.Repository)
		}(i)
	}
	wg.Wait()
}

func (m *Manager) searchNPM(ctx context.Context, query string) ([]Item, error) {
	type searchResult struct {
		Objects []struct {
			Package struct {
				Name        string   `json:"name"`
				Description string   `json:"description"`
				Keywords    []string `json:"keywords"`
			} `json:"package"`
		} `json:"objects"`
	}
	var r searchResult
	for _, term := range []string{"opencode auth provider", "keywords:magpie-plugin"} {
		if query != "" {
			term = query + " " + term
		}
		v := url.Values{"text": {term}, "size": {"30"}}
		b, e := m.get(ctx, m.npmURL+"/-/v1/search?"+v.Encode(), 4<<20)
		if e != nil {
			return nil, e
		}
		var page searchResult
		if e = json.Unmarshal(b, &page); e != nil {
			return nil, e
		}
		r.Objects = append(r.Objects, page.Objects...)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	out := []Item{}
	seen := map[string]bool{}
	var failures []error
	for _, obj := range r.Objects {
		v := obj.Package
		text := strings.ToLower(v.Name + " " + v.Description + " " + strings.Join(v.Keywords, " "))
		if seen[v.Name] || !npmName.MatchString(v.Name) || ((!strings.Contains(text, "magpie-plugin") && !strings.Contains(text, "opencode")) || (!strings.Contains(text, "auth") && !strings.Contains(text, "provider"))) {
			continue
		}
		seen[v.Name] = true
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			p, e := m.npmManifest(ctx, name, "latest")
			if e != nil {
				if !notFound(e) {
					mu.Lock()
					failures = append(failures, e)
					mu.Unlock()
				}
				return
			}
			if p.onlyOtherKind() {
				return
			}
			if len(p.Magpie.Providers) == 0 {
				// Search labels alone do not establish a supplier. Check the actual
				// published entry for supplier hooks before offering the candidate;
				// installation still requires successful runtime inspection.
				files, e := m.npmSource(ctx, p)
				if e != nil {
					mu.Lock()
					failures = append(failures, e)
					mu.Unlock()
					return
				}
				entry, e := entryInFiles(p, files)
				if e != nil || !supplierSource(files[entry]) {
					return
				}
			}
			it := p.item(name)
			mu.Lock()
			out = append(out, it)
			mu.Unlock()
		}(v.Name)
	}
	wg.Wait()
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return out, ctx.Err()
}

func (m *Manager) searchGitHub(ctx context.Context) ([]Item, error) {
	type repo struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Archived      bool   `json:"archived"`
		Fork          bool   `json:"fork"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	var repos []repo
	for page := 1; page <= 10; page++ {
		v := url.Values{"q": {"topic:magpie-plugin archived:false fork:false"}, "sort": {"stars"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		b, e := m.get(ctx, m.githubURL+"/search/repositories?"+v.Encode(), 4<<20)
		if e != nil {
			return nil, e
		}
		var r struct {
			Items []repo `json:"items"`
			Total int    `json:"total_count"`
		}
		if e = json.Unmarshal(b, &r); e != nil {
			return nil, e
		}
		repos = append(repos, r.Items...)
		if len(r.Items) < 100 || page*100 >= r.Total {
			break
		}
	}
	out := []Item{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	var failures []error
	for _, r := range repos {
		if r.Fork || r.Archived || strings.EqualFold(r.Owner.Login, "magpie-community") || !gitName.MatchString(r.FullName) {
			continue
		}
		wg.Add(1)
		go func(r repo) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			branch := r.DefaultBranch
			if branch == "" {
				branch = "HEAD"
			}
			base := m.rawURL + "/" + r.FullName + "/" + url.PathEscape(branch) + "/"
			b, e := m.get(ctx, base+"package.json", 2<<20)
			var p manifest
			if e != nil {
				if !notFound(e) {
					mu.Lock()
					failures = append(failures, e)
					mu.Unlock()
				}
				return
			}
			if json.Unmarshal(b, &p) != nil || !npmName.MatchString(p.Name) || p.onlyOtherKind() {
				return
			}
			it := p.item("github:" + r.FullName)
			it.Repository = "https://github.com/" + r.FullName
			if published, e := m.npmManifest(ctx, p.Name, "latest"); e == nil && strings.EqualFold(repository(published.Repository), it.Repository) {
				p = published
				it = p.item(p.Name)
			}
			if len(p.Magpie.Providers) == 0 {
				if it.Package == p.Name {
					files, e := m.npmSource(ctx, p)
					if e != nil {
						mu.Lock()
						failures = append(failures, e)
						mu.Unlock()
						return
					}
					entry, e := entryInFiles(p, files)
					if e != nil || !supplierSource(files[entry]) {
						return
					}
				} else {
					entry, e := packageEntry(p)
					if e != nil {
						return
					}
					found := false
					for _, candidate := range entryCandidates(entry) {
						source, e := m.get(ctx, base+candidate, 2<<20)
						if e == nil {
							found = supplierSource(source)
							break
						}
						if !notFound(e) {
							mu.Lock()
							failures = append(failures, e)
							mu.Unlock()
							return
						}
					}
					if !found {
						return
					}
				}
			}
			mu.Lock()
			out = append(out, it)
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	if len(failures) > 0 {
		return nil, errors.Join(failures...)
	}
	return out, nil
}
