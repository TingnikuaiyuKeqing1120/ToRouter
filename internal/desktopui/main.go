// Package desktopui attaches a native console or floating pet to the daemon.
package desktopui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"torouter/internal/app"
	"torouter/internal/desktop"
	"torouter/web"
)

// Wails' Window wraps winc.Form but creates a different Win32 window class.
// Keep creation and discovery explicit so resizing/focus target the same HWND.
const nativeWindowClass = "wailsWindow"

// Run opens or recalls the selected native view of the shared daemon.
func Run(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cfg, err := parseLaunch(args, cwd)
	if err != nil {
		return err
	}
	backend, err := connectBackend(cfg)
	if err != nil {
		return err
	}
	cfg.port = backend.port
	// Wails forwards os.Args to the existing window. Forward the resolved context,
	// including when this launch used a relative data path or a saved port.
	os.Args = append(os.Args[:1], cfg.args()...)
	opts := newAppOptions(cfg, backend)
	release, err := acquirePetLaunchGate(cfg.windowID())
	if err != nil {
		return err
	}
	defer release()
	startup := opts.OnStartup
	opts.OnStartup = func(ctx context.Context) {
		startup(ctx)
		// Wails' single-instance message window now exists. A racing launch
		// can safely notify it instead of creating another main window.
		release()
	}
	return wails.Run(opts)
}

type backendConnection struct {
	origin         string
	proxy          http.Handler
	cookie         *http.Cookie
	port           int
	installationID string
}

func connectBackend(cfg launchConfig) (*backendConnection, error) {
	port, err := app.ConnectDaemon(cfg.dataDir, cfg.port)
	if err != nil {
		return nil, err
	}
	cfg.port = port
	installation, err := app.InstallationID(cfg.dataDir)
	if err != nil {
		return nil, err
	}
	cookie := app.ObtainInstallationSessionCookie(cfg.port, installation)
	if cookie == nil {
		return nil, fmt.Errorf("无法建立本地管理会话 (%s)", cfg.origin())
	}
	target, err := url.Parse(cfg.origin())
	if err != nil {
		return nil, err
	}
	proxy := installationProxy(port, installation, httputil.NewSingleHostReverseProxy(target))
	return &backendConnection{origin: cfg.origin(), port: port, proxy: proxy, cookie: cookie, installationID: installation}, nil
}

// A duplicate launch must not reload an editor just because the new launcher
// obtained another cookie. Validate the existing connection/session instead.
func reusableBackend(cfg launchConfig, backend *backendConnection) bool {
	if backend == nil || cfg.origin() != backend.origin || backend.installationID == "" || backend.cookie == nil {
		return false
	}
	if err := app.CheckInstallation(cfg.port, backend.installationID); err != nil {
		return false
	}
	req, err := http.NewRequest(http.MethodGet, backend.origin+"/api/auth/session", nil)
	if err != nil {
		return false
	}
	req.AddCookie(backend.cookie)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Check the selected installation before any authenticated request, including
// automatic local-session recovery. A different daemon may have taken this
// port since startup. Public readiness is identity evidence, not a secret.
func installationProxy(port int, installation string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := app.CheckInstallation(port, installation); err != nil {
			http.Error(w, "selected gateway installation is unavailable", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type nativeClient struct {
	cfg           launchConfig
	mu            sync.RWMutex
	backend       *backendConnection
	ctx           context.Context
	ready         chan struct{}
	readyOnce     sync.Once
	switchMu      sync.Mutex
	launchMu      sync.Mutex
	launchQueue   []options.SecondInstanceData
	launchWorker  bool
	connect       func(launchConfig) (*backendConnection, error)
	resize        func(int) error
	activate      func(string, bool)
	prepareWindow func() error
	placement     petPlacement
	quit          func()
	waitLifecycle func(context.Context, string, string) (app.DaemonLifecycle, error)
	followCancel  context.CancelFunc
}

func newNativeClient(cfg launchConfig, backend *backendConnection) *nativeClient {
	c := &nativeClient{cfg: cfg, backend: backend, ready: make(chan struct{}), connect: connectBackend}
	c.placement = newPetPlacement(cfg.dataDir)
	c.resize = c.placement.resize
	c.quit = func() {
		c.mu.RLock()
		ctx := c.ctx
		c.mu.RUnlock()
		if ctx != nil {
			runtime.Quit(ctx)
		}
	}
	c.prepareWindow = func() error {
		if c.cfg.pet {
			return makePetToolWindow()
		}
		return nil
	}
	c.waitLifecycle = app.WaitDaemonLifecycle
	c.activate = func(title string, reload bool) {
		c.mu.RLock()
		ctx := c.ctx
		c.mu.RUnlock()
		if ctx == nil {
			return
		}
		if err := c.prepareWindow(); err != nil {
			app.LogStartupFailure(c.cfg.dataDir, err)
			return
		}
		runtime.WindowSetTitle(ctx, title)
		if runtime.WindowIsMinimised(ctx) {
			runtime.WindowUnminimise(ctx)
		}
		runtime.WindowShow(ctx)
		focusPetWindow()
		if reload {
			runtime.WindowReload(ctx)
		}
	}
	return c
}

func (c *nativeClient) snapshot() *backendConnection {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backend
}

// Do not block Wails' second-instance callback: Windows delivers notifications
// through a bounded channel from its UI thread. Repeated launches could fill
// that channel while a callback waits on a synchronous window operation.
func (c *nativeClient) queueLaunch(data options.SecondInstanceData) {
	c.launchMu.Lock()
	c.launchQueue = append(c.launchQueue, data)
	if c.launchWorker {
		c.launchMu.Unlock()
		return
	}
	c.launchWorker = true
	c.launchMu.Unlock()
	go func() {
		for {
			c.launchMu.Lock()
			if len(c.launchQueue) == 0 {
				c.launchWorker = false
				c.launchMu.Unlock()
				return
			}
			next := c.launchQueue[0]
			c.launchQueue = c.launchQueue[1:]
			c.launchMu.Unlock()
			if err := c.secondLaunch(next); err != nil {
				app.LogStartupFailure(c.cfg.dataDir, err)
			}
		}
	}()
}

// Transitions run in launch order without blocking Wails' message processor.
func (c *nativeClient) secondLaunch(data options.SecondInstanceData) error {
	c.switchMu.Lock()
	defer c.switchMu.Unlock()
	cfg, err := parseLaunch(data.Args, data.WorkingDirectory)
	if err != nil {
		return err
	}
	if cfg.windowID() != c.cfg.windowID() {
		return fmt.Errorf("desktop window installation or mode mismatch")
	}
	if existing := c.snapshot(); reusableBackend(cfg, existing) {
		<-c.ready
		c.activate(c.windowTitle(existing.origin), false)
		return nil
	}
	backend, err := c.connect(cfg)
	if err != nil {
		return err
	}
	<-c.ready
	c.mu.Lock()
	c.backend = backend
	c.mu.Unlock()
	// Replace an expired session or changed target, preserving the current route.
	c.activate(c.windowTitle(backend.origin), true)
	return nil
}

func (c *nativeClient) secondLaunchArgs(cfg launchConfig) error {
	return c.secondLaunch(options.SecondInstanceData{Args: cfg.args()})
}

func (c *nativeClient) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/desktop/close-pet" && c.cfg.pet {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST required", 405)
			return
		}
		if r.Header.Get("X-To-Router") != "1" {
			http.Error(w, "missing X-To-Router", 403)
			return
		}
		select {
		case <-c.ready:
		default:
			http.Error(w, "window is not ready", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"requested":true}`)
		go c.quit()
		return
	}
	if r.URL.Path == "/api/desktop/resize-pet" {
		c.serveResize(w, r)
		return
	}
	backend := c.snapshot()
	if backend == nil || backend.proxy == nil {
		http.Error(w, "gateway service unavailable", http.StatusServiceUnavailable)
		return
	}
	// Launch/close use the same authenticated backend handlers as the browser.
	r = r.Clone(r.Context())
	r.Host = strings.TrimPrefix(backend.origin, "http://")
	if r.Header.Get("Origin") != "" {
		r.Header.Set("Origin", backend.origin)
	}
	if backend.cookie != nil {
		if _, err := r.Cookie("to_router_session"); err != nil {
			r.AddCookie(backend.cookie)
		}
	}
	backend.proxy.ServeHTTP(w, r)
}

func (c *nativeClient) serveResize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-To-Router") != "1" {
		http.Error(w, "missing X-To-Router", http.StatusForbidden)
		return
	}
	if !c.cfg.pet {
		http.Error(w, "this window is not a desktop companion", http.StatusConflict)
		return
	}
	var body struct {
		Height int `json:"height"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid resize body", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF || body.Height < 200 || body.Height > 700 {
		http.Error(w, "height must be an integer between 200 and 700", http.StatusBadRequest)
		return
	}
	select {
	case <-c.ready:
	default:
		http.Error(w, "window is not ready", http.StatusServiceUnavailable)
		return
	}
	if err := c.resize(body.Height); err != nil {
		http.Error(w, "window resize failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func bootstrap(pet bool, origin string) string {
	marker, _ := json.Marshal(struct {
		Pet    bool   `json:"pet"`
		Origin string `json:"origin"`
	}{pet, origin})
	script := `if(!window.__TO_ROUTER_DESKTOP__){Object.defineProperty(window,'__TO_ROUTER_DESKTOP__',{value:Object.freeze(` + string(marker) + `),writable:false,configurable:false});}`
	notify := `window.dispatchEvent(new Event('to-router-desktop-ready'));`
	if pet {
		return script + `if(!window.location.hash||window.location.hash==='#/'){window.location.hash='#/pet';}document.documentElement.classList.add('pet-mode');` + notify
	}
	return script + `if(window.location.hash==='#/pet'||window.location.hash==='#/floating-pet'){window.location.hash='#/usage';}` + notify
}

func (c *nativeClient) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			index, err := fs.ReadFile(web.DistFS, "index.html")
			if err == nil {
				origin := c.cfg.origin()
				if backend := c.snapshot(); backend != nil {
					origin = backend.origin
					if backend.cookie != nil {
						rootCookie := *backend.cookie
						rootCookie.Path = "/"
						http.SetCookie(w, &rootCookie)
						http.SetCookie(w, backend.cookie)
					}
				}
				content := strings.Replace(string(index), "</head>", "<script>"+bootstrap(c.cfg.pet, origin)+"</script></head>", 1)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, content)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func newAppOptions(cfg launchConfig, backend *backendConnection) *options.App {
	return newNativeClient(cfg, backend).options()
}

func (c *nativeClient) windowTitle(origin string) string {
	if c.cfg.pet {
		return desktop.PetWindowTitle(origin, c.cfg.dataDir)
	}
	return "to-router 控制台"
}

func (c *nativeClient) options() *options.App {
	opts := &options.App{
		Title: "to-router 控制台", Width: 1200, Height: 800, MinWidth: 960, MinHeight: 600,
		BackgroundColour: &options.RGBA{R: 10, G: 13, B: 20, A: 255},
		AssetServer:      &assetserver.Options{Assets: web.DistFS, Handler: http.HandlerFunc(c.serveAPI), Middleware: c.middleware},
		Windows:          &windows.Options{Theme: windows.Dark, WindowClassName: nativeWindowClass},
		OnStartup: func(ctx context.Context) {
			c.mu.Lock()
			c.ctx = ctx
			followCtx, cancel := context.WithCancel(ctx)
			c.followCancel = cancel
			c.mu.Unlock()
			go c.followBackend(followCtx)
			if c.cfg.pet {
				go c.rememberPosition(followCtx)
			}
		},
		OnDomReady: func(ctx context.Context) {
			origin := c.cfg.origin()
			if backend := c.snapshot(); backend != nil {
				origin = backend.origin
			}
			if err := c.prepareWindow(); err != nil {
				app.LogStartupFailure(c.cfg.dataDir, err)
				runtime.Quit(ctx)
				return
			}
			if c.cfg.pet {
				if err := c.placement.restore(); err != nil {
					app.LogStartupFailure(c.cfg.dataDir, err)
				}
			}
			c.readyOnce.Do(func() { close(c.ready) })
			runtime.WindowExecJS(ctx, bootstrap(c.cfg.pet, origin))
			if c.cfg.pet {
				runtime.WindowShow(ctx)
			}
		},
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: c.cfg.windowID(), OnSecondInstanceLaunch: c.queueLaunch},
		OnBeforeClose: func(context.Context) bool {
			if c.cfg.pet {
				_ = c.placement.save()
			}
			return false
		},
		OnShutdown: func(context.Context) {
			c.mu.RLock()
			cancel := c.followCancel
			c.mu.RUnlock()
			if cancel != nil {
				cancel()
			}
		},
	}
	if c.cfg.pet {
		opts.Title = desktop.PetWindowTitle(c.cfg.origin(), c.cfg.dataDir)
		opts.Width, opts.Height, opts.MinWidth, opts.MinHeight = 260, 200, 0, 0
		opts.Frameless, opts.AlwaysOnTop, opts.DisableResize = true, true, true
		opts.StartHidden = true
		opts.BackgroundColour = &options.RGBA{}
		opts.Windows = &windows.Options{WebviewIsTransparent: true, WindowIsTranslucent: true, BackdropType: windows.None, DisableFramelessWindowDecorations: true, Theme: windows.SystemDefault, WindowClassName: nativeWindowClass}
	}
	return opts
}
