//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"torouter/internal/app"
	"torouter/internal/desktop"
)

// Minimal Win32 bindings. golang.org/x/sys exports only a small slice of the
// API surface needed for a tray icon, so the rest is declared here to keep the
// implementation pure Go (no CGO) while still producing a single exe.
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")

	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostMessage         = user32.NewProc("PostMessageW")
	procPostThreadMessage   = user32.NewProc("PostThreadMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procGetCurrentThreadID  = kernel32.NewProc("GetCurrentThreadId")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procCheckMenuRadioItem  = user32.NewProc("CheckMenuRadioItem")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procMessageBox          = user32.NewProc("MessageBoxW")
	procLoadImage           = user32.NewProc("LoadImageW")
	procDestroyObject       = gdi32.NewProc("DeleteObject")
	procOpenClipboard       = user32.NewProc("OpenClipboard")
	procEmptyClipboard      = user32.NewProc("EmptyClipboard")
	procCloseClipboard      = user32.NewProc("CloseClipboard")
	procGetClipboardData    = user32.NewProc("GetClipboardData")
	procSetClipboardData    = user32.NewProc("SetClipboardData")
	procGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	procGlobalLock          = kernel32.NewProc("GlobalLock")
	procGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	procGlobalFree          = kernel32.NewProc("GlobalFree")
	procShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
)

// Window messages and menu styles used below.
const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmQuit          = 0x0012
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	cwmCallback     = wmApp + 1
	wmTrayQuit      = wmApp + 2

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	niiInfo    = 0x01

	mfString    = 0x0000
	mfPopup     = 0x0010
	mfSeparator = 0x0800
	mfChecked   = 0x0008
	mfUnchecked = 0x0000

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	imageIcon      = 1
	lrLoadFromFile = 0x0010

	mbOK              = 0x00000000
	mbIconInformation = 0x00000040
	mbIconError       = 0x00000010

	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	hwndMessage = ^uintptr(2) // (HWND)-3, a message-only parent window

	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "to-router"
)

// Menu command IDs.
const (
	idOpenConsole    uintptr = 1
	idCopyKey        uintptr = 2
	idOpenLog        uintptr = 3
	idAutostart      uintptr = 4
	idQuit           uintptr = 5
	idOpenCompanion  uintptr = 6
	idOpenDesktop    uintptr = 7
	idOpenCLI        uintptr = 8
	idDefaultWeb     uintptr = 9
	idDefaultDesktop uintptr = 10
	idDefaultCLI     uintptr = 11
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type winPoint struct {
	X, Y int32
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	_       uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   winPoint
}

type notifyIconData struct {
	cbSize            uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateFlags      uint32
	szInfo            [256]uint16
	uVersionOrTimeout uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      uintptr
}

var (
	className   = windows.StringToUTF16Ptr("to-router-tray-window")
	wndCallback = windows.NewCallback(wndProc)
)

type trayState struct {
	cfg             Config
	mu              sync.Mutex
	hwnd            uintptr
	icon            uintptr
	serverErr       error
	lastDefaultOpen time.Time
}

var current *trayState

// Run starts the tray and blocks until the server stops or the user quits.
func Run(cfg Config, serveDone <-chan error) error {
	// The window belongs to the thread that creates it, and GetMessage only
	// returns the calling thread's queue. Locking here — before the window
	// exists — keeps the window, the pump and every window procedure on one OS
	// thread for the life of the process. Locking inside loop() instead is too
	// late: the scheduler may migrate the goroutine in between, and the tray
	// then stops answering clicks at all.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	st := &trayState{cfg: cfg}
	current = st
	if err := st.createWindow(); err != nil {
		return err
	}
	if err := st.loadIcon(); err != nil {
		// The tray still works without a custom icon.
		fmt.Fprintln(os.Stderr, "tray icon unavailable:", err)
	}
	if err := st.addIcon(); err != nil {
		return err
	}
	go func() {
		if err := <-serveDone; err != nil {
			st.mu.Lock()
			st.serverErr = err
			st.mu.Unlock()
		}
		// Post to the window, not the thread: PostMessage is safe from any
		// thread and the owning message loop retrieves it. Posting to this
		// goroutine's own thread would land on a thread with no queue and the
		// tray would hang with the icon still visible.
		procPostMessage.Call(st.hwnd, wmTrayQuit, 0, 0)
	}()
	st.loop()
	// The icon comes off before anything slow happens, so the tray entry
	// disappears the moment the user picks 退出.
	st.removeIcon()
	if st.icon != 0 {
		procDestroyObject.Call(st.icon)
	}
	// The quit hook runs here, on the goroutine that owns the process, never
	// on the window thread. Stopping the server can take seconds.
	if st.cfg.OnQuit != nil {
		// Belt and braces: a wedged shutdown must not trap the user in a
		// process that will not exit. The tray icon is already gone.
		net := time.AfterFunc(20*time.Second, func() {
			st.removeIcon()
			os.Exit(0)
		})
		st.cfg.OnQuit()
		net.Stop()
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.serverErr
}

// createWindow must run on the same goroutine that later pumps messages: a
// window belongs to the thread that created it, and GetMessage with a NULL
// filter only returns the calling thread's queue. Run does both.
func (s *trayState) createWindow() error {
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	var wc wndClassEx
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	wc.lpfnWndProc = wndCallback
	wc.hInstance = instance
	wc.lpszClassName = className
	if atom, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 && err != windows.ERROR_CLASS_ALREADY_EXISTS {
		return fmt.Errorf("register tray window class: %w", err)
	}
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("create tray window: %w", err)
	}
	s.hwnd = hwnd
	return nil
}

func (s *trayState) loadIcon() error {
	if s.cfg.IconPath == "" {
		return fmt.Errorf("no icon path")
	}
	path, err := windows.UTF16PtrFromString(s.cfg.IconPath)
	if err != nil {
		return err
	}
	handle, _, callErr := procLoadImage.Call(0, uintptr(unsafe.Pointer(path)), imageIcon, 32, 32, lrLoadFromFile)
	if handle == 0 {
		if callErr != nil {
			return callErr
		}
		return fmt.Errorf("LoadImage failed for %s", s.cfg.IconPath)
	}
	s.icon = handle
	return nil
}

func (s *trayState) data() notifyIconData {
	nid := notifyIconData{}
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = s.hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = cwmCallback
	nid.hIcon = s.icon
	fillUTF16(nid.szTip[:], s.cfg.Tooltip)
	return nid
}

func (s *trayState) addIcon() error {
	trayClass := windows.StringToUTF16Ptr("Shell_TrayWnd")
	hTray, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(trayClass)), 0)
	if hTray == 0 {
		return nil
	}
	nid := s.data()
	var lastErr error
	for i := 0; i < 20; i++ {
		ret, _, err := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
		if ret != 0 {
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("add tray icon: %w", lastErr)
	}
	return errors.New("add tray icon failed")
}

func (s *trayState) removeIcon() {
	nid := s.data()
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func (s *trayState) balloon(title, body string) {
	nid := s.data()
	nid.uFlags |= nifInfo
	nid.dwInfoFlags = niiInfo
	fillUTF16(nid.szInfoTitle[:], title)
	fillUTF16(nid.szInfo[:], body)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// fillUTF16 copies a string into a fixed-size UTF-16 field, always NUL
// terminated.
func fillUTF16(field []uint16, text string) {
	runes := []rune(text)
	limit := len(field) - 1
	for i := 0; i < limit && i < len(runes); i++ {
		field[i] = uint16(runes[i])
	}
	field[min(len(runes), limit)] = 0
}

// loop pumps messages until WM_QUIT. It must run on the thread that created
// the window; Run guarantees that by locking the OS thread for its whole body.
func (s *trayState) loop() {
	for {
		var msg winMsg
		ret, _, err := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 {
			return
		}
		if ret == ^uintptr(0) {
			if err != nil {
				// A spurious failure must not kill the tray.
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case cwmCallback:
		switch lParam {
		case wmRButtonUp:
			current.showMenu()
		case wmLButtonUp, wmLButtonDblClk:
			current.openDefault()
		}
		return 0
	case wmTrayQuit, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return ret
}

// buildMenu reads current window state whenever the popup opens; no stale
// preference can leave a checkmark behind after an external close.
func (s *trayState) buildMenu() (uintptr, error) {
	menu, _, err := procCreatePopupMenu.Call()
	if menu == 0 {
		return 0, fmt.Errorf("create tray menu: %w", err)
	}
	appendItem(menu, idOpenConsole, "打开 Web")
	appendItem(menu, idOpenDesktop, "打开 Desktop")
	appendItem(menu, idOpenCLI, "打开 CLI")
	defaults, _, createErr := procCreatePopupMenu.Call()
	if defaults == 0 {
		procDestroyMenu.Call(menu)
		return 0, fmt.Errorf("create default launch menu: %w", createErr)
	}
	appendItem(defaults, idDefaultWeb, "Web")
	appendItem(defaults, idDefaultDesktop, "Desktop")
	appendItem(defaults, idDefaultCLI, "CLI")
	selected := idDefaultWeb
	switch s.cfg.defaultMode() {
	case "desktop":
		selected = idDefaultDesktop
	case "cli":
		selected = idDefaultCLI
	}
	procCheckMenuRadioItem.Call(defaults, idDefaultWeb, idDefaultCLI, selected, 0)
	procAppendMenu.Call(menu, mfPopup, defaults, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("默认启动方式"))))
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	appendCheckedItem(menu, idOpenCompanion, "桌宠", desktop.CompanionRunning(s.cfg.Origin, s.cfg.DataDir))
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	appendItem(menu, idCopyKey, "复制管理员密钥")
	appendItem(menu, idOpenLog, "打开日志")
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	appendCheckedItem(menu, idAutostart, "开机自启", AutostartEnabledForData(s.cfg.DataDir))
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
	appendItem(menu, idQuit, "退出")
	return menu, nil
}

func (s *trayState) showMenu() {
	menu, err := s.buildMenu()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer procDestroyMenu.Call(menu)

	var point winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	// Required so the menu dismisses correctly when clicking elsewhere.
	procSetForegroundWindow.Call(s.hwnd)
	chosen, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd, uintptr(uint32(point.X)), uintptr(uint32(point.Y)), 0, s.hwnd, 0)
	procPostMessage.Call(s.hwnd, wmNull, 0, 0)
	switch chosen {
	case idOpenDesktop:
		s.openDesktop()
	case idOpenConsole:
		s.openConsole()
	case idOpenCompanion:
		if err := s.toggleCompanion(); err != nil {
			ShowError("无法切换桌宠", err.Error())
		}
	case idOpenCLI:
		s.openCLI()
	case idDefaultWeb, idDefaultDesktop, idDefaultCLI:
		mode := map[uintptr]string{idDefaultWeb: "web", idDefaultDesktop: "desktop", idDefaultCLI: "cli"}[chosen]
		if s.cfg.SetDefaultLaunchMode != nil {
			if err := s.cfg.SetDefaultLaunchMode(mode); err != nil {
				ShowError("无法保存默认启动方式", err.Error())
			}
		}
	case idCopyKey:
		s.copyKey()
	case idOpenLog:
		s.openLog()
	case idAutostart:
		s.toggleAutostart()
	case idQuit:
		s.quit()
	}
}

func (s *trayState) toggleCompanion() error {
	if desktop.CompanionRunning(s.cfg.Origin, s.cfg.DataDir) {
		return desktop.CloseCompanion(s.cfg.Origin, s.cfg.DataDir)
	}
	return desktop.OpenCompanion(s.cfg.Origin, s.cfg.DataDir)
}

func appendCheckedItem(menu, id uintptr, text string, checked bool) {
	flags := uintptr(mfString)
	if checked {
		flags |= mfChecked
	}
	procAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
}

func appendItem(menu uintptr, id uintptr, text string) {
	procAppendMenu.Call(menu, mfString, id, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))))
}

func (s *trayState) openDesktop() {
	if err := desktop.OpenNativeConsole(s.cfg.Origin, s.cfg.DataDir); err != nil {
		ShowError("无法打开桌面控制台", err.Error())
	}
}

func (s *trayState) openCLI() {
	if err := desktop.OpenCLI(s.cfg.Origin, s.cfg.DataDir); err != nil {
		ShowError("无法打开命令行视图", err.Error())
	}
}

func (s *trayState) openDefault() {
	// Shell_NotifyIcon delivers button-up and double-click callbacks for one
	// double click. Coalesce them so the Web default opens only one tab.
	interval, _, _ := user32.NewProc("GetDoubleClickTime").Call()
	now := time.Now()
	if now.Sub(s.lastDefaultOpen) < time.Duration(interval)*time.Millisecond+50*time.Millisecond {
		return
	}
	s.lastDefaultOpen = now
	if err := OpenMode(s.cfg.defaultMode(), s.cfg.Origin, s.cfg.DataDir); err != nil {
		ShowError("无法打开控制台", err.Error())
	}
}

func (s *trayState) openConsole() {
	if err := openWithShell(s.cfg.Origin); err != nil {
		ShowError("无法打开浏览器", err.Error())
	}
}

func (s *trayState) copyKey() {
	key, err := app.ReadAdminKey(s.cfg.DataDir)
	if err != nil {
		ShowError("无法读取管理员密钥", err.Error())
		return
	}
	verified, copyErr := CopyVerified(key)
	if copyErr != nil || !verified {
		// No clipboard in this session (or the write did not land): show the
		// key rather than claiming a copy that did not happen.
		ShowInfo("管理员登录密钥", key)
		if copyErr != nil {
			s.balloon("剪贴板不可用", "已直接显示密钥，请手动复制。")
		}
		return
	}
	s.balloon("已复制", "管理员登录密钥已复制到剪贴板，粘贴到控制台登录框即可。")
}

func (s *trayState) openLog() {
	if s.cfg.LogPath == "" {
		return
	}
	if err := openWithShell(fileURL(s.cfg.LogPath)); err != nil {
		ShowError("无法打开日志", err.Error())
	}
}

func (s *trayState) toggleAutostart() {
	if AutostartEnabledForData(s.cfg.DataDir) {
		if err := SetAutostartForData(false, s.cfg.DataDir); err != nil {
			ShowError("无法关闭开机自启", err.Error())
			return
		}
		s.balloon("开机自启已关闭", "to-router 不再随系统启动。")
		return
	}
	if err := SetAutostartForData(true, s.cfg.DataDir); err != nil {
		ShowError("无法开启开机自启", err.Error())
		return
	}
	s.balloon("开机自启已开启", "登录 Windows 后 to-router 会自动启动。")
}

// quit asks the message loop to exit. It deliberately does NOT run the
// shutdown itself: this runs inside DispatchMessage on the window thread, and
// a shutdown that takes even a second freezes the tray. Clicks that arrive
// meanwhile queue up and are dispatched afterwards, which replays the menu at
// whatever position the pointer has drifted to — the "menu appears somewhere
// else, then nothing responds" symptom. The quit hook runs once the loop has
// returned; see Run.
func (s *trayState) quit() {
	procPostMessage.Call(s.hwnd, wmTrayQuit, 0, 0)
}

// AutostartEnabled reports whether to-router is registered to start at login.
func AutostartEnabled() bool {
	return autostartEntry() != ""
}

func autostartEntry() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	value, _, err := key.GetStringValue(runValue)
	if err != nil {
		return ""
	}
	return value
}

// Only an entry for this executable and data directory appears enabled. An old
// executable or another installation is never silently migrated at startup;
// choosing Enable explicitly registers the command for the current instance.
func AutostartEnabledForData(dataDir string) bool {
	executable, err := os.Executable()
	return err == nil && autostartMatches(autostartEntry(), executable, dataDir)
}

func autostartMatches(command, executable, dataDir string) bool {
	args, err := windows.DecomposeCommandLine(command)
	if err != nil || len(args) < 2 {
		return false
	}
	registeredData := app.DefaultDir()
	silent := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-autostart":
			silent = true
		case "-data":
			i++
			if i == len(args) {
				return false
			}
			registeredData = args[i]
		default:
			return false
		}
	}
	if !silent {
		return false
	}
	wantExe, err := filepath.Abs(executable)
	if err != nil {
		return false
	}
	gotExe, err := filepath.Abs(args[0])
	if err != nil {
		return false
	}
	wantData, err := filepath.Abs(dataDir)
	if err != nil {
		return false
	}
	gotData, err := filepath.Abs(registeredData)
	return err == nil && strings.EqualFold(wantExe, gotExe) && strings.EqualFold(wantData, gotData)
}

func autostartCommand(executable, dataDir string) (string, error) {
	absoluteData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	return windows.EscapeArg(filepath.Clean(executable)) + " -autostart -data " + windows.EscapeArg(absoluteData), nil
}

// SetAutostart registers or clears the login-time start entry. The entry passes
// -autostart so a login-time start stays silent instead of opening a browser
// tab on every login.
func SetAutostart(enable bool) error {
	return SetAutostartForData(enable, app.DefaultDir())
}

func SetAutostartForData(enable bool, dataDir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enable {
		if err := key.DeleteValue(runValue); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	command, err := autostartCommand(executable, dataDir)
	if err != nil {
		return err
	}
	return key.SetStringValue(runValue, command)
}

// CopyToClipboard places text on the Windows clipboard as UTF-16 text.
func CopyToClipboard(text string) error {
	if ret, _, _ := procOpenClipboard.Call(0); ret == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()
	if ret, _, _ := procEmptyClipboard.Call(); ret == 0 {
		return fmt.Errorf("EmptyClipboard failed")
	}
	// Include the terminating NUL so the text is a valid C string.
	utf16 := append(windows.StringToUTF16(text), 0)
	handle, _, err := procGlobalAlloc.Call(gmemMoveable, uintptr(len(utf16)*2))
	if handle == 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("GlobalAlloc failed")
	}
	pointer, lockErr := globalLock(handle)
	if lockErr != nil {
		procGlobalFree.Call(handle)
		return lockErr
	}
	target := unsafe.Slice((*uint16)(pointer), len(utf16))
	copy(target, utf16)
	procGlobalUnlock.Call(handle)
	if ret, _, _ := procSetClipboardData.Call(cfUnicodeText, handle); ret == 0 {
		// Ownership transfers to the system only on success.
		procGlobalFree.Call(handle)
	}
	return nil
}

// globalLock resolves a locked global block to its address. Reinterpreting the
// returned uintptr through a variable's storage is the documented way to hand a
// system-owned block to generic helpers; the block is not Go heap memory, so no
// garbage collector can move it.
func globalLock(handle uintptr) (unsafe.Pointer, error) {
	address, _, err := procGlobalLock.Call(handle)
	if address == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("GlobalLock failed")
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(&address)), nil
}

// clipboardText reads the clipboard back so a copy can be confirmed instead of
// reported on faith. Sessions without a desktop (services, some remotes) cannot
// open the clipboard at all.
func clipboardText() (string, error) {
	if ret, _, _ := procOpenClipboard.Call(0); ret == 0 {
		return "", fmt.Errorf("无法访问剪贴板")
	}
	defer procCloseClipboard.Call()
	handle, _, err := procGetClipboardData.Call(cfUnicodeText)
	if handle == 0 {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("剪贴板中没有文本")
	}
	pointer, lockErr := globalLock(handle)
	if lockErr != nil {
		return "", lockErr
	}
	defer procGlobalUnlock.Call(handle)
	return windows.UTF16PtrToString((*uint16)(pointer)), nil
}

// CopyVerified places text on the clipboard and reports whether reading it back
// returns exactly what was written.
func CopyVerified(text string) (bool, error) {
	if err := CopyToClipboard(text); err != nil {
		return false, err
	}
	got, err := clipboardText()
	if err != nil {
		return false, err
	}
	return got == text, nil
}

// ShowError and ShowInfo use the native dialog so a tray build can still report
// a problem to a user who is not watching a console.
func ShowError(title, body string) {
	procMessageBox.Call(0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(body))), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(title))), mbOK|mbIconError)
}

func ShowInfo(title, body string) {
	procMessageBox.Call(0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(body))), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(title))), mbOK|mbIconInformation)
}

func fileURL(path string) string {
	return "file:///" + filepath.ToSlash(path)
}

// openWithShell uses rundll32 so no console window flashes.
func openWithShell(target string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
}
