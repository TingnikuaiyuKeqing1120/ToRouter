// Package app holds the platform-independent server: store access, port
// selection, the HTTP handler tree, the log file and graceful shutdown.
// Presentation (terminal or tray) lives in the cmd packages, so the same
// server can run with a console window or silently from the system tray.
package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"torouter/internal/auth"
	"torouter/internal/buildinfo"
	"torouter/internal/companion"
	"torouter/internal/console"
	"torouter/internal/desktop"
	"torouter/internal/gateway"
	"torouter/internal/pluginhost"
	"torouter/internal/pluginmarket"
	"torouter/internal/plugins"
	"torouter/internal/quota"
	"torouter/internal/store"
	"torouter/internal/updater"
	webfs "torouter/web"
)

// DefaultPort is the address clients are configured for. It sits above the
// Windows dynamic (ephemeral) port range, so outbound connections never claim
// it, and it is uncommon enough that other tools rarely do.
const DefaultPort = 17878

// Hooks let the web console stop or relaunch the process. They are wired to the
// same shutdown path the tray and SIGINT use, so there is exactly one way to
// stop the server.
type Hooks struct {
	// Shutdown stops the server gracefully; Serve then returns and the process
	// exits normally.
	Shutdown func()
	// Restart relaunches this executable and stops the current process.
	Restart func()
}

// Server is a running to-router instance.
type Server struct {
	activeIdentity daemonReady
	lifecycle      *daemonLifecycle
	Store          *store.Store
	DataDir        string
	Port           int // port actually bound
	Wanted         int // port requested from the flag or configuration

	stop   chan struct{}
	stopMu sync.Once
	log    *Logger

	closeOnce         sync.Once
	closeErr          error
	listener          net.Listener
	http              *http.Server
	console           http.Handler
	restartMu         sync.Mutex
	restartRequested  bool
	updateRequested   bool
	restartTargetPort int
	restartPortErr    error
	restartFrozen     bool
	restartToken      string
	relaunch          func(string, int) error
	requestMu         sync.Mutex
	draining          bool
	requests          sync.WaitGroup
	requestContext    context.Context
	cancelRequests    context.CancelFunc
	shutdownOnce      sync.Once
	shutdownErr       error
	handlersDone      chan struct{}
	shutdownGrace     time.Duration
	cancelWait        time.Duration
	plugins           *plugins.Manager
	automationCancel  context.CancelFunc
	updates           *updater.Manager
	launchUpdate      func(*updater.Prepared, updater.HelperOptions) (int, error)
}

// Origin is the loopback base URL clients should use.
func (s *Server) Origin() string { return fmt.Sprintf("http://127.0.0.1:%d", s.Port) }

// Log returns the process logger, writing redacted events to the data dir.
func (s *Server) Log() *Logger { return s.log }

// Handler returns the HTTP request handler for the server.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Start opens the store, chooses a port and binds it. portFlag 0 means "use the
// configured port, else DefaultPort". restartGrace > 0 makes binding wait that
// long for a predecessor to release the port, which is how the console restart
// button hands over.
func Start(dataDir string, portFlag int, restartGrace time.Duration) (*Server, error) {
	if err := updater.CheckLaunch(dataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	pluginManager, err := plugins.New(st, pluginhost.NewInstallation(dataDir))
	if err == nil {
		err = pluginManager.AttachMarket(pluginmarket.New(dataDir))
	}
	if err != nil {
		st.Close()
		return nil, errors.New("cannot load plugin preferences")
	}
	port := portFlag
	if port == 0 {
		configured, portErr := st.Port()
		if portErr == nil && configured > 0 {
			port = configured
		} else {
			port = DefaultPort
		}
	}
	if port < 1 || port > 65535 {
		st.Close()
		return nil, errors.New("port must be between 1 and 65535")
	}
	logger, err := NewLogger(dataDir, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	ready, identity, lifecycle, err := consoleReadyHandlerWithLifecycle(st, port)
	if err != nil {
		logger.Close()
		st.Close()
		return nil, err
	}
	// Bind exactly the port that was chosen. A silent move to another port
	// would break every client config that points here, so a refusal is an
	// error with the remedies spelled out instead.
	listener, err := Listen(port, restartGrace)
	if err != nil {
		logger.Close()
		st.Close()
		return nil, err
	}
	s := &Server{
		Store:          st,
		plugins:        pluginManager,
		activeIdentity: identity,
		lifecycle:      lifecycle,
		DataDir:        dataDir,
		Port:           port,
		Wanted:         port,
		stop:           make(chan struct{}),
		listener:       listener,
		log:            logger,
		relaunch:       relaunch,
		shutdownGrace:  10 * time.Second,
		cancelWait:     5 * time.Second,
	}
	s.requestContext, s.cancelRequests = context.WithCancel(context.Background())
	s.initProgramUpdates()
	origin := s.Origin()
	mux := http.NewServeMux()
	mux.Handle(consoleReadyPath, ready)
	authentication := auth.New(st)
	// One lazy registry owns all credential lifecycles across requests,
	// management, quota and automation. Optional providers remain uncreated.
	live := companion.New()
	// All ingress protocols observe the same outgoing combinations and success
	// paths. Quota reads only participate when selecting a new credential.
	chatGateway := gateway.NewWithPlugins(st, pluginManager)
	chatGateway.Live = live
	quotaSvc := quota.NewWithPlugins(st, pluginManager, nil)
	pluginManager.OnChange(func(credentialType string, _ bool) {
		channels, readErr := st.ListChannels()
		if readErr != nil {
			logger.Error("cannot invalidate plugin quota cache", "error", readErr)
			return
		}
		for _, channel := range channels {
			if channel.CredentialType == credentialType {
				quotaSvc.Reset(channel.ID, "")
			}
		}
	})
	chatGateway.QuotaCheck = func(ctx context.Context, c store.Channel, cred store.CredentialItem) quota.Observation {
		return quotaSvc.Check(ctx, c, cred, false)
	}
	mux.Handle("/api/auth/", authentication.Handler())
	s.console = console.NewWithPlugins(st, origin, console.Hooks{
		Quotas:             quotaSvc,
		ObserveQuota:       chatGateway.ObserveQuota,
		Shutdown:           s.Shutdown,
		Restart:            s.Restart,
		UpdateStatus:       s.programUpdateStatus,
		CheckUpdate:        s.checkProgramUpdate,
		PrepareUpdate:      s.prepareProgramUpdate,
		Companion:          live.Snapshot,
		OpenCompanion:      func() error { return desktop.OpenCompanion(origin, dataDir) },
		OpenConsole:        func() error { return desktop.OpenNativeConsole(origin, dataDir) },
		OpenChannelConsole: func(id int64, cid string) error { return openChannelConsole(origin, id, cid) },
		CloseCompanion:     func() error { return desktop.CloseCompanion(origin, dataDir) },
		CompanionRunning:   func() bool { return desktop.CompanionRunning(origin, dataDir) },
		RuntimeStatus:      chatGateway.RuntimeStatus,
		ResetStatus: func(channelID int64, credentialID, model string) error {
			if err := chatGateway.ResetStatus(channelID, credentialID, model); err != nil {
				return err
			}
			if model == "" {
				quotaSvc.Reset(channelID, credentialID)
			}
			return nil
		},
		RefreshQuota: func(ctx context.Context, c store.Channel, cred store.CredentialItem) error {
			_, observation := quotaSvc.RefreshSnapshot(ctx, c, cred, true)
			chatGateway.ObserveQuota(c, cred, observation)
			if observation.Error != "" {
				return errors.New(observation.Error)
			}
			return nil
		},
	}, pluginManager)
	mux.Handle("/api/", authentication.RequireAdmin(http.StripPrefix("/api", s.console)))
	mux.Handle("/mcp", console.ManagementHandler(s.console))
	mux.Handle("/v1/", authentication.RequireClient(chatGateway))
	mux.Handle("/v1beta/", authentication.RequireClient(chatGateway))
	mux.Handle("/", webfs.Handler())
	mux.HandleFunc("/_internal/restart-ready", func(w http.ResponseWriter, r *http.Request) {
		if s.restartToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-To-Router-Restart")), []byte(s.restartToken)) != 1 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(restartReady{PID: os.Getpid(), Port: s.Port, Version: buildinfo.Version})
	})
	s.http = &http.Server{Handler: s.trackRequests(LocalOnly(mux)), BaseContext: func(net.Listener) context.Context { return s.requestContext }, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	logger.Event("start", "origin", origin, "data", dataDir, "port", port)
	return s, nil
}

// Shutdown asks the server to stop gracefully. It is what the tray menu, the
// console buttons and SIGINT all end up calling.
func (s *Server) Shutdown() {
	s.stopMu.Do(func() { close(s.stop) })
}

// Restart queues a handover. Serve owns drain, persistence and relaunch so the
// HTTP acknowledgment means requested, and never means the child is ready.
func (s *Server) Restart() {
	s.restartMu.Lock()
	if !s.restartFrozen && !s.updateRequested {
		s.restartRequested = true
	}
	s.restartMu.Unlock()
	s.Shutdown()
}

// SetRestartReadiness configures the child-only handshake before Serve starts.
// Its random token is supplied by the predecessor, never a management key.
func (s *Server) SetRestartReadiness(token string) { s.restartToken = token }

func (s *Server) trackRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requestMu.Lock()
		if s.draining {
			s.requestMu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		s.requests.Add(1)
		s.requestMu.Unlock()
		defer s.requests.Done()
		next.ServeHTTP(w, r)
	})
}

// restartHandoverSeconds is how long a relaunched child waits for the port.
const restartHandoverSeconds = 15

// Serve blocks until the process is asked to stop or the server fails, then
// shuts down gracefully.
func (s *Server) Serve(parent context.Context) error {
	if err := s.publishActiveDaemon(); err != nil {
		return err
	}
	if s.updates != nil {
		s.updates.StartBackground(s.requestContext, 24*time.Hour)
	}
	s.requestMu.Lock()
	if !s.draining && s.plugins != nil && s.automationCancel == nil {
		ctx, cancel := context.WithCancel(s.requestContext)
		s.automationCancel = cancel
		s.requests.Add(1)
		go func() { defer s.requests.Done(); s.plugins.Run(ctx) }()
	}
	s.requestMu.Unlock()
	failed := make(chan error, 1)
	go func() { failed <- s.http.Serve(s.listener) }()
	reason := "request"
	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			s.log.Event("server stopped")
			return s.shutdown("closed")
		}
		s.log.Error("server stopped unexpectedly", "error", err)
		return errors.Join(err, s.shutdown("failed"))
	case <-parent.Done():
		reason = "signal"
	case <-s.stop:
	}
	if err := s.shutdown(reason); err != nil {
		return err
	}
	s.restartMu.Lock()
	restart := s.restartRequested
	targetPort, portErr := s.restartTargetPort, s.restartPortErr
	s.restartMu.Unlock()
	if err := s.Close(); err != nil {
		return err
	}
	if portErr != nil {
		return fmt.Errorf("cannot read restart port: %w", portErr)
	}
	if restart {
		s.log.Event("relaunching", "port", targetPort)
		if err := s.relaunch(s.DataDir, targetPort); err != nil {
			s.log.Error("restart failed", "port", targetPort, "error", err)
			return fmt.Errorf("restart failed: %w", err)
		}
		s.log.Event("restart ready", "port", targetPort)
	}
	return nil
}

func (s *Server) shutdown(reason string) error {
	s.shutdownOnce.Do(func() {
		// Freeze the handover decision before informing views. Its target must
		// equal the eventual relaunch port, including a pending saved port.
		s.restartMu.Lock()
		s.restartFrozen = true
		s.restartTargetPort = s.Port
		state := "stopped"
		if s.updateRequested {
			state = "updating"
		} else if s.restartRequested {
			configured, err := s.Store.Port()
			s.restartPortErr = err
			if err == nil {
				state = "restarting"
				if configured > 0 {
					s.restartTargetPort = configured
				}
			}
		}
		target := 0
		if state == "restarting" || state == "updating" {
			target = s.restartTargetPort
		}
		s.restartMu.Unlock()
		// Release observers before Shutdown waits for registered requests.
		s.lifecycle.finish(state, target)
		// No Add may race Wait: close admission under the registration mutex.
		s.requestMu.Lock()
		s.draining = true
		if s.automationCancel != nil {
			s.automationCancel()
		}
		s.requestMu.Unlock()
		s.handlersDone = make(chan struct{})
		go func() { s.requests.Wait(); close(s.handlersDone) }()
		ctx, cancel := context.WithTimeout(context.Background(), s.shutdownGrace)
		defer cancel()
		if err := s.http.Shutdown(ctx); err != nil {
			s.log.Warn("graceful shutdown timed out; canceling in-flight requests", "error", err)
			s.cancelRequests()
			_ = s.http.Close()
		}
		_ = s.listener.Close()
		// Socket closure alone cannot prove usage/token writers have returned.
		select {
		case <-s.handlersDone:
			s.cancelRequests()
			console.ReleaseOAuthListeners(s.console)
			s.log.Event("server stopped", "reason", reason)
		case <-time.After(s.cancelWait):
			s.shutdownErr = errors.New("request handlers did not exit after cancellation; Store remains open and restart was aborted")
			s.log.Error("shutdown failed", "error", s.shutdownErr)
		}
	})
	return s.shutdownErr
}

// Close refuses to release Store before all persistence handlers have exited.
func (s *Server) Close() error {
	s.Shutdown()
	_ = s.shutdown("close")
	select {
	case <-s.handlersDone:
	default:
		return s.shutdownErr
	}
	s.closeOnce.Do(func() {
		s.clearActiveDaemon()
		s.log.Close()
		s.closeErr = s.Store.Close()
	})
	return s.closeErr
}

// Listen binds exactly one port. When grace > 0 it retries until the grace
// period elapses, which is how a restarted instance waits for its predecessor
// to release the port; otherwise a refusal fails immediately.
func Listen(port int, grace time.Duration) (net.Listener, error) {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", address)
	if err == nil {
		return listener, nil
	}
	if grace <= 0 {
		return nil, listenError(port, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if listener, err = net.Listen("tcp", address); err == nil {
			return listener, nil
		}
	}
	return nil, listenError(port, err)
}

func listenError(port int, err error) error {
	return fmt.Errorf("cannot listen on 127.0.0.1:%d: %w\n常见原因：端口被其他程序占用，或有残留的 to-router 进程没退出（可在任务管理器结束）。\n解决：用 -port 指定其他端口，或在控制台「系统设置」里修改端口后重启。", port, err)
}

// LocalOnly keeps the service on the loopback interface.
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "local host required", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if !localConsoleProbe(r) && (err != nil || u.Scheme != "http" || (u.Host != "127.0.0.1:5173" && u.Host != "localhost:5173" && u.Host != r.Host)) {
				http.Error(w, "origin not allowed", 403)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// DefaultDir is where the database, key and log live.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".to-router"
	}
	return filepath.Join(home, ".to-router")
}

// LogPath is the process log file inside the data directory.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "to-router.log") }

// ReadAdminKey returns the console login key for a data directory. The console
// and the tray both need it without starting a listener.
func ReadAdminKey(dataDir string) (string, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return "", err
	}
	defer st.Close()
	settings, err := st.GetSettings()
	if err != nil {
		return "", err
	}
	return settings.GatewayKey, nil
}
