package keydiscovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	_ "modernc.org/sqlite"
)

const resourceJSONLimit = 8 << 20
const resourceDatabaseLimit = 32 << 20

var errResource = errors.New("source resource could not be read safely")

type resourceFile struct {
	data        []byte
	fingerprint string
	missing     bool
}
type resources struct {
	mu               sync.Mutex
	root             *os.Root
	rootPath         string
	fileAliases      map[string]string
	rootIdentity     os.FileInfo
	allowed          map[string]bool
	guard            func() error
	files            map[string]resourceFile
	databases        map[string]*sql.DB
	snapshotRoot     string
	snapshotIdentity os.FileInfo
	closed           bool
}

func resourceName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:\x00") && !filepath.IsAbs(name)
}
func resourceUNC(path string) bool {
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//")
}

// Source roots are pinned to a concrete directory. A changed link must not
// silently redirect a pending preview to another set of credential files.
func resourceRootIdentity(path string) (os.FileInfo, error) {
	// Go on Windows can classify junctions as ModeIrregular and leave them
	// unchanged in EvalSymlinks. Inspect every component without following its
	// final link, including ancestors, before attempting canonical resolution.
	clean := filepath.Clean(path)
	anchor := filepath.VolumeName(clean) + string(filepath.Separator)
	current := anchor
	info, err := os.Lstat(anchor)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissing
		}
		return nil, errResource
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, ErrConflict
	}
	for _, component := range strings.Split(strings.TrimPrefix(clean, anchor), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err = os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrMissing
			}
			return nil, errResource
		}
		if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil, ErrConflict
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissing
		}
		return nil, errResource
	}
	equal := filepath.Clean(path) == filepath.Clean(resolved)
	if runtime.GOOS == "windows" {
		equal = strings.EqualFold(filepath.Clean(path), filepath.Clean(resolved))
	}
	if resourceUNC(resolved) || !equal {
		return nil, ErrConflict
	}
	return info, nil
}
func newResources(root string, source Source, guard func() error) (*resources, error) {
	if guard != nil {
		if err := guard(); err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(root) || resourceUNC(root) {
		return nil, ErrInvalid
	}
	allowed := map[string]bool{}
	for _, name := range source.Files {
		if !resourceName(name) {
			return nil, ErrInvalid
		}
		allowed[name] = true
	}
	approved := filepath.Clean(root)
	before, err := resourceRootIdentity(approved)
	if err != nil {
		return nil, err
	}
	opened, err := os.OpenRoot(approved)
	if err != nil {
		return nil, errResource
	}
	accepted := false
	defer func() {
		if !accepted {
			opened.Close()
		}
	}()
	identity, err := opened.Stat(".")
	if err != nil {
		return nil, errResource
	}
	after, err := resourceRootIdentity(approved)
	if err != nil || !os.SameFile(before, identity) || !os.SameFile(identity, after) {
		return nil, ErrConflict
	}
	r := &resources{root: opened, rootPath: approved, rootIdentity: identity, allowed: allowed, guard: guard, files: map[string]resourceFile{}, databases: map[string]*sql.DB{}}
	if err := r.checkRoot(context.Background()); err != nil {
		return nil, err
	}
	if source.ID == "source-codex-manager" {
		override := os.Getenv("CODEXMANAGER_DB_PATH")
		if filepath.IsAbs(override) && filepath.Clean(filepath.Dir(override)) == filepath.Clean(root) {
			filename := filepath.Base(override)
			if !resourceName(filename) {
				r.Close()
				return nil, ErrInvalid
			}
			r.fileAliases = map[string]string{"codexmanager.db": filename, "codexmanager.db-wal": filename + "-wal"}
		}
	}
	accepted = true
	return r, nil
}
func (r *resources) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.closed {
		return ErrInvalid
	}
	if r.guard != nil {
		if err := r.guard(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Bind actual source reads and material-return boundaries to the directory
// handle opened above, including cached bytes and SQLite snapshot results.
func (r *resources) checkRoot(ctx context.Context) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	identity, err := resourceRootIdentity(r.rootPath)
	if err != nil || !os.SameFile(identity, r.rootIdentity) {
		return ErrConflict
	}
	return ctx.Err()
}
func resourceFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func resourceDB(name string) bool { return strings.EqualFold(filepath.Ext(name), ".db") }

// All source access uses this pinned Root. No SQLite connection is ever opened
// on the source files. Nonblocking open also prevents a swapped FIFO hanging us.
func (r *resources) readRaw(ctx context.Context, name string, limit int64) (value resourceFile, err error) {
	if err = r.checkRoot(ctx); err != nil {
		return value, err
	}
	defer func() {
		if gate := r.checkRoot(ctx); gate != nil {
			clear(value.data)
			value = resourceFile{}
			err = gate
		}
	}()
	physical := name
	if alias := r.fileAliases[name]; alias != "" {
		physical = alias
	}
	info, err := r.root.Stat(physical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resourceFile{fingerprint: "missing", missing: true}, ErrMissing
		}
		return value, errResource
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return value, errResource
	}
	file, err := r.root.OpenFile(physical, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return resourceFile{fingerprint: "missing", missing: true}, ErrMissing
		}
		return value, errResource
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return value, errResource
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		clear(data)
		return value, errResource
	}
	return resourceFile{data: data, fingerprint: resourceFingerprint(data)}, nil
}
func (r *resources) Read(ctx context.Context, filename string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkRoot(ctx); err != nil {
		return nil, err
	}
	if !r.allowed[filename] {
		return nil, ErrInvalid
	}
	value, exists := r.files[filename]
	if !exists {
		limit := int64(resourceJSONLimit)
		if resourceDB(filename) {
			limit = resourceDatabaseLimit
		}
		var err error
		value, err = r.readRaw(ctx, filename, limit)
		if err != nil && !errors.Is(err, ErrMissing) {
			return nil, err
		}
		r.files[filename] = value
	}
	if err := r.checkRoot(ctx); err != nil {
		return nil, err
	}
	if value.missing {
		return nil, ErrMissing
	}
	data := bytes.Clone(value.data)
	if err := r.checkRoot(ctx); err != nil {
		clear(data)
		return nil, err
	}
	return data, nil
}
func (r *resources) Fingerprints() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make(map[string]string, len(r.files))
	for name, value := range r.files {
		values[name] = value.fingerprint
	}
	return values
}
func withinResourceRoot(root, target string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return false
	}
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func (r *resources) snapshot(ctx context.Context, filename string) (*sql.DB, error) {
	if db := r.databases[filename]; db != nil {
		return db, nil
	}
	names := []string{filename, filename + "-wal"}
	first := make(map[string]resourceFile, 2)
	defer func() {
		for _, value := range first {
			clear(value.data)
		}
	}()
	for _, name := range names {
		value, err := r.readRaw(ctx, name, resourceDatabaseLimit)
		if err != nil && !errors.Is(err, ErrMissing) {
			return nil, err
		}
		if cached, ok := r.files[name]; ok && cached.fingerprint != value.fingerprint {
			clear(value.data)
			return nil, ErrConflict
		}
		first[name] = value
	}
	// DB and WAL must both be byte-identical across two complete passes. This
	// detects a checkpoint/commit during copying rather than silently switching.
	for _, name := range names {
		value, err := r.readRaw(ctx, name, resourceDatabaseLimit)
		if err != nil && !errors.Is(err, ErrMissing) {
			return nil, err
		}
		equal := value.fingerprint == first[name].fingerprint
		clear(value.data)
		if !equal {
			return nil, ErrConflict
		}
	}
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	for name, value := range first {
		if _, exists := r.files[name]; !exists {
			r.files[name] = resourceFile{data: bytes.Clone(value.data), fingerprint: value.fingerprint, missing: value.missing}
		}
	}
	if first[filename].missing {
		return nil, ErrMissing
	}
	if r.snapshotRoot == "" {
		parent, err := filepath.Abs(os.TempDir())
		if err != nil || withinResourceRoot(r.rootPath, parent) {
			return nil, errResource
		}
		private, err := os.MkdirTemp(parent, "torouter-key-discovery-")
		if err != nil {
			return nil, errResource
		}
		r.snapshotRoot = private
		info, err := os.Lstat(private)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errResource
		}
		r.snapshotIdentity = info
	}
	for _, name := range names {
		value := first[name]
		if value.missing {
			continue
		}
		target := filepath.Join(r.snapshotRoot, name)
		if !withinResourceRoot(r.snapshotRoot, target) {
			return nil, ErrInvalid
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, errResource
		}
		_, writeErr := file.Write(value.data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			return nil, errResource
		}
	}
	path := filepath.Join(r.snapshotRoot, filename)
	// URL encoding preserves spaces, #, ?, and Windows drive letters in paths.
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	databaseURL := (&url.URL{Scheme: "file", Path: uriPath}).String()
	db, err := sql.Open("sqlite", databaseURL+"?_pragma=query_only(1)")
	if err != nil {
		return nil, errResource
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errResource
	}
	if err = r.check(ctx); err != nil {
		db.Close()
		return nil, err
	}
	r.databases[filename] = db
	return db, nil
}
func (r *resources) Query(ctx context.Context, filename, query string) ([][]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkRoot(ctx); err != nil {
		return nil, err
	}
	statement := strings.TrimSpace(query)
	if !r.allowed[filename] || !resourceDB(filename) || len(statement) < 7 || !strings.EqualFold(statement[:6], "select") || !strings.ContainsAny(statement[6:7], " \t\r\n(") || strings.ContainsAny(statement, ";\x00") || strings.Contains(statement, "--") || strings.Contains(statement, "/*") {
		return nil, ErrInvalid
	}
	db, err := r.snapshot(ctx, filename)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		if gate := r.check(ctx); gate != nil {
			return nil, gate
		}
		return nil, errResource
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) > 20 {
		return nil, errResource
	}
	result := [][]string{}
	total := 0
	for rows.Next() {
		if len(result) >= 1000 {
			return nil, errResource
		}
		if err := r.check(ctx); err != nil {
			return nil, err
		}
		values := make([]sql.RawBytes, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, errResource
		}
		row := make([]string, len(columns))
		for i, value := range values {
			total += len(value)
			if len(value) > resourceJSONLimit || total > 16<<20 {
				return nil, errResource
			}
			row[i] = string(value) // SQL NULL is nil RawBytes and becomes the empty string.
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		if gate := r.check(ctx); gate != nil {
			return nil, gate
		}
		return nil, errResource
	}
	if err := r.checkRoot(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
func (r *resources) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	failed := false
	for _, db := range r.databases {
		if db.Close() != nil {
			failed = true
		}
	}
	for name, value := range r.files {
		clear(value.data)
		delete(r.files, name)
	}
	r.databases = nil
	if r.root.Close() != nil {
		failed = true
	}
	if r.snapshotRoot != "" {
		// Never recurse into a replaced path, a sibling, or a caller-supplied root.
		absolute, err := filepath.Abs(r.snapshotRoot)
		info, statErr := os.Lstat(r.snapshotRoot)
		if err != nil || absolute != r.snapshotRoot || !withinResourceRoot(r.snapshotRoot, absolute) || statErr != nil || r.snapshotIdentity == nil || !os.SameFile(info, r.snapshotIdentity) || info.Mode()&os.ModeSymlink != 0 {
			failed = true
		} else if os.RemoveAll(absolute) != nil {
			failed = true
		}
		r.snapshotRoot = ""
		r.snapshotIdentity = nil
	}
	if failed {
		return errResource
	}
	return nil
}

func verifyFiles(ctx context.Context, root string, source Source, expected map[string]string, guard func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, err := newResources(root, source, guard)
	if err != nil {
		if errors.Is(err, ErrMissing) || errors.Is(err, errResource) {
			return ErrConflict
		}
		return err
	}
	defer r.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, fingerprint := range expected {
		allowed := r.allowed[name]
		if strings.HasSuffix(name, "-wal") {
			allowed = r.allowed[strings.TrimSuffix(name, "-wal")] && resourceDB(strings.TrimSuffix(name, "-wal"))
		}
		if !allowed || !resourceName(name) {
			return ErrInvalid
		}
		limit := int64(resourceJSONLimit)
		if resourceDB(name) || strings.HasSuffix(name, "-wal") {
			limit = resourceDatabaseLimit
		}
		value, err := r.readRaw(ctx, name, limit)
		if err != nil && !errors.Is(err, ErrMissing) {
			if errors.Is(err, errResource) {
				return ErrConflict
			}
			return err
		}
		equal := fingerprint == value.fingerprint
		clear(value.data)
		if !equal {
			return ErrConflict
		}
	}
	return r.checkRoot(ctx)
}
