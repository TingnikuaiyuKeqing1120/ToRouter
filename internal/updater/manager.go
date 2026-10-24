package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Config struct {
	Version           string
	FeedURL           string
	PublicKey         string
	TargetPath        string
	AllowLoopbackHTTP bool // Only for an explicitly configured isolated test feed.
	Client            *http.Client
}

type Status struct {
	Enabled        bool       `json:"enabled"`
	CurrentVersion string     `json:"current_version"`
	State          string     `json:"state"`
	Available      *Release   `json:"available,omitempty"`
	LastChecked    *time.Time `json:"last_checked,omitempty"`
	Error          string     `json:"error,omitempty"`
	LastResult     *Result    `json:"last_result,omitempty"`
}

type Prepared struct {
	Release        Release
	TargetPath     string
	StagedPath     string
	WorkDir        string
	CurrentSHA256  string
	CurrentVersion string
	Manifest       []byte
}

type Manager struct {
	mu        sync.Mutex
	config    Config
	key       ed25519.PublicKey
	client    *http.Client
	status    Status
	manifest  []byte
	checking  chan struct{}
	preparing chan struct{}
	prepared  *Prepared
}

func New(config Config) (*Manager, error) {
	m := &Manager{config: config, status: Status{CurrentVersion: config.Version, State: "disabled"}}
	if config.TargetPath == "" {
		config.TargetPath, _ = os.Executable()
	}
	if config.TargetPath != "" {
		config.TargetPath, _ = filepath.Abs(config.TargetPath)
		m.config = config
		if result, err := ReadResult(config.TargetPath); err == nil {
			m.status.LastResult = &result
		}
	}
	if config.FeedURL == "" && config.PublicKey == "" {
		return m, nil
	}
	if err := validateURL(config.FeedURL, config.AllowLoopbackHTTP); err != nil {
		return nil, err
	}
	key, err := ParsePublicKey(config.PublicKey)
	if err != nil {
		return nil, err
	}
	m.key = key
	if _, err = versionParts(config.Version); err != nil {
		return nil, fmt.Errorf("current version: %w", err)
	}
	if config.TargetPath == "" {
		config.TargetPath, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	config.TargetPath, err = filepath.Abs(config.TargetPath)
	if err != nil {
		return nil, err
	}
	m.config = config
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = 30 * time.Second
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many update redirects")
		}
		return validateURL(req.URL.String(), config.AllowLoopbackHTTP)
	}
	m.client = &copyClient
	m.status.Enabled = true
	m.status.State = "idle"
	return m, nil
}

func (m *Manager) Status() Status {
	result, resultErr := ReadResult(m.config.TargetPath)
	m.mu.Lock()
	defer m.mu.Unlock()
	if resultErr == nil {
		m.status.LastResult = &result
	}
	return cloneStatus(m.status)
}

// ErrorMessage is suitable for management responses: filesystem and transport
// errors must not leak the user's installation paths or feed query strings.
func ErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "update file operation failed"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "update network request failed"
	}
	return err.Error()
}
func cloneStatus(s Status) Status {
	if s.LastResult != nil {
		r := *s.LastResult
		s.LastResult = &r
	}
	if s.Available != nil {
		r := *s.Available
		s.Available = &r
	}
	if s.LastChecked != nil {
		v := *s.LastChecked
		s.LastChecked = &v
	}
	return s
}

func (m *Manager) Check(ctx context.Context) (Status, error) {
	m.mu.Lock()
	if !m.status.Enabled {
		s := cloneStatus(m.status)
		m.mu.Unlock()
		return s, nil
	}
	if wait := m.checking; wait != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return m.Status(), ctx.Err()
		case <-wait:
			s := m.Status()
			if s.Error != "" {
				return s, errors.New(s.Error)
			}
			return s, nil
		}
	}
	if m.preparing != nil || m.prepared != nil {
		s := cloneStatus(m.status)
		m.mu.Unlock()
		return s, nil
	}
	done := make(chan struct{})
	m.checking = done
	m.status.State = "checking"
	m.status.Error = ""
	m.mu.Unlock()
	data, err := m.fetch(ctx, m.config.FeedURL, MaxManifestSize)
	var release Release
	if err == nil {
		release, err = VerifyManifest(data, m.key)
	}
	if err == nil && (release.OS != runtime.GOOS || release.Arch != runtime.GOARCH) {
		err = errors.New("release is for another platform")
	}
	if err == nil {
		err = validateURL(release.URL, m.config.AllowLoopbackHTTP)
	}
	var available bool
	if err == nil {
		available, err = newer(release.Version, m.config.Version)
	}
	m.mu.Lock()
	now := time.Now().UTC()
	m.status.LastChecked = &now
	m.status.Available = nil
	m.manifest = nil
	m.status.State = "idle"
	if err != nil {
		m.status.State = "error"
		m.status.Error = ErrorMessage(err)
	} else if available {
		m.status.State = "available"
		m.status.Available = &release
		m.manifest = append([]byte(nil), data...)
	}
	m.checking = nil
	close(done)
	s := cloneStatus(m.status)
	m.mu.Unlock()
	return s, err
}

func (m *Manager) StartBackground(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				_, _ = m.Check(ctx)
				timer.Reset(interval)
			}
		}
	}()
}

func (m *Manager) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update server returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, errors.New("update response too large")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("update response too large")
	}
	return data, nil
}

func (m *Manager) Prepare(ctx context.Context) (*Prepared, error) {
	return m.PrepareVersion(ctx, "")
}

// PrepareVersion binds installation to the version explicitly confirmed by the
// user. A feed advancing while a confirmation dialog is open requires review.
func (m *Manager) PrepareVersion(ctx context.Context, expectedVersion string) (*Prepared, error) {
	return m.PrepareRelease(ctx, expectedVersion, "")
}

// PrepareRelease pins the confirmed version and content hash. Signing another
// artifact under the same version must not silently change an open dialog.
func (m *Manager) PrepareRelease(ctx context.Context, expectedVersion, expectedSHA256 string) (*Prepared, error) {
	if _, err := m.Check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.prepared != nil {
		p := clonePrepared(m.prepared)
		m.mu.Unlock()
		if !matchesRelease(p.Release, expectedVersion, expectedSHA256) {
			return nil, errors.New("release changed; review the available update again")
		}
		return p, nil
	}
	if wait := m.preparing; wait != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.prepared != nil {
				if !matchesRelease(m.prepared.Release, expectedVersion, expectedSHA256) {
					return nil, errors.New("release changed; review the available update again")
				}
				return clonePrepared(m.prepared), nil
			}
			return nil, errors.New(m.status.Error)
		}
	}
	if m.status.Available == nil {
		m.mu.Unlock()
		return nil, errors.New("no verified newer release available")
	}
	if !matchesRelease(*m.status.Available, expectedVersion, expectedSHA256) {
		m.mu.Unlock()
		return nil, errors.New("release changed; review the available update again")
	}
	release := *m.status.Available
	manifest := append([]byte(nil), m.manifest...)
	done := make(chan struct{})
	m.preparing = done
	m.status.State = "downloading"
	m.status.Error = ""
	m.mu.Unlock()
	p, err := m.download(ctx, release, manifest)
	m.mu.Lock()
	m.preparing = nil
	if err != nil {
		m.status.State = "error"
		m.status.Error = ErrorMessage(err)
	} else {
		m.prepared = p
		m.status.State = "ready"
	}
	close(done)
	m.mu.Unlock()
	return clonePrepared(p), err
}

func matchesRelease(release Release, version, sha256 string) bool {
	return (version == "" || release.Version == version) && (sha256 == "" || release.SHA256 == sha256)
}

func clonePrepared(p *Prepared) *Prepared {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Manifest = append([]byte(nil), p.Manifest...)
	return &copy
}

func (m *Manager) download(ctx context.Context, r Release, manifest []byte) (result *Prepared, err error) {
	current, err := HashFile(m.config.TargetPath)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(m.config.TargetPath), ".torouter-update-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(filepath.Join(dir, "update.exe"))
			_ = os.Remove(dir)
		}
	}()
	path := filepath.Join(dir, "update.exe")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > 0 && resp.ContentLength != r.Size {
		return nil, errors.New("download size does not match signed release")
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, r.Size+1))
	if err != nil {
		return nil, err
	}
	if n != r.Size || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return nil, errors.New("download does not match signed size and SHA256")
	}
	if err = out.Sync(); err != nil {
		return nil, err
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	return &Prepared{Release: r, TargetPath: m.config.TargetPath, StagedPath: path, WorkDir: dir, CurrentSHA256: current, CurrentVersion: m.config.Version, Manifest: manifest}, nil
}

func (m *Manager) MarkInstalling() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.State = "installing"
	m.status.Error = ""
}
func (m *Manager) SetInstallError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.State = "error"
	if err != nil {
		m.status.Error = ErrorMessage(err)
	}
}

func HashFile(path string) (string, error) {
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
