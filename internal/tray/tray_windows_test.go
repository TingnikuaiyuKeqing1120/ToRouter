//go:build windows

package tray

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"torouter/internal/desktop"
)

// Regression for the "tray stops responding, then the menu appears where the
// pointer has drifted to" report: quit must not run the shutdown on the window
// thread. It only has to post the quit message, so it returns immediately and
// the message loop can exit and run the hook afterwards.
func TestQuitDoesNotRunShutdownOnWindowThread(t *testing.T) {
	s := &trayState{cfg: Config{OnQuit: func() {
		t.Error("OnQuit ran inside quit(); it would block the message pump")
		time.Sleep(5 * time.Second)
	}}}
	current = s
	defer func() { current = nil }()

	done := make(chan struct{})
	go func() { s.quit(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("quit blocked; the window thread would freeze with it")
	}
}

func TestTrayMenuOrderAndLiveCompanionCheck(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	s := &trayState{cfg: Config{Origin: "http://127.0.0.1:19202", DataDir: t.TempDir()}}
	labels := []string{"打开 Web", "打开 Desktop", "打开 CLI", "默认启动方式", "", "桌宠", "", "复制管理员密钥", "打开日志", "", "开机自启", "", "退出"}
	ids := []uintptr{idOpenConsole, idOpenDesktop, idOpenCLI, 0xffffffff, 0, idOpenCompanion, 0, idCopyKey, idOpenLog, 0, idAutostart, 0, idQuit}
	check := func(running bool) {
		t.Helper()
		menu, err := s.buildMenu()
		if err != nil {
			t.Fatal(err)
		}
		defer procDestroyMenu.Call(menu)
		count, _, _ := user32.NewProc("GetMenuItemCount").Call(menu)
		if int(count) != len(labels) {
			t.Fatalf("menu item count %d", count)
		}
		for i, label := range labels {
			state, _, _ := user32.NewProc("GetMenuState").Call(menu, uintptr(i), 0x400) // MF_BYPOSITION
			if label == "" {
				if state&mfSeparator == 0 {
					t.Errorf("item %d must be a separator", i)
				}
				continue
			}
			id, _, _ := user32.NewProc("GetMenuItemID").Call(menu, uintptr(i))
			if id != ids[i] {
				t.Errorf("item %d id=%d", i, id)
			}
			var text [128]uint16
			user32.NewProc("GetMenuStringW").Call(menu, uintptr(i), uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)), 0x400)
			if got := windows.UTF16ToString(text[:]); got != label {
				t.Errorf("item %d label=%q", i, got)
			}
			if id == idOpenCompanion && (state&mfChecked != 0) != running {
				t.Errorf("pet checked state disagrees with actual window: %v", running)
			}
		}
	}
	check(false)
	class := windows.StringToUTF16Ptr("STATIC")
	title := windows.StringToUTF16Ptr(desktop.PetWindowTitle(s.cfg.Origin, s.cfg.DataDir))
	window, _, err := user32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 10, 10, 0, 0, 0, 0)
	if window == 0 {
		t.Fatalf("create companion test window: %v", err)
	}
	defer func() {
		if window != 0 {
			user32.NewProc("DestroyWindow").Call(window)
		}
	}()
	check(false) // A hidden startup window is not ready yet.
	user32.NewProc("ShowWindow").Call(window, 4)
	check(true)
	user32.NewProc("DestroyWindow").Call(window)
	window = 0
	check(false)
}

func TestDefaultLaunchMenuIsSingleRadioSelection(t *testing.T) {
	for _, mode := range []string{"web", "desktop", "cli"} {
		s := &trayState{cfg: Config{DefaultLaunchMode: func() string { return mode }}}
		menu, err := s.buildMenu()
		if err != nil {
			t.Fatal(err)
		}
		submenu, _, _ := user32.NewProc("GetSubMenu").Call(menu, 3)
		if submenu == 0 {
			t.Fatal("default submenu must immediately follow the three opens")
		}
		count, _, _ := user32.NewProc("GetMenuItemCount").Call(submenu)
		if count != 3 {
			t.Fatalf("default choices = %d", count)
		}
		for i, choice := range []string{"web", "desktop", "cli"} {
			state, _, _ := user32.NewProc("GetMenuState").Call(submenu, uintptr(i), 0x400)
			if (state&mfChecked != 0) != (mode == choice) {
				t.Fatalf("%s checked state for %s: %x", mode, choice, state)
			}
		}
		procDestroyMenu.Call(menu)
	}
}

func TestAutostartCommandPreservesExecutableAndCustomDataDirectory(t *testing.T) {
	executable := `C:\Program Files\Synthetic Router\ToRouter.exe`
	dataDir := `C:\Synthetic Data\Router Profile\`
	command, err := autostartCommand(executable, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	args, err := windows.DecomposeCommandLine(command)
	want := []string{executable, "-autostart", "-data", filepath.Clean(dataDir)}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("autostart command does not round trip: %v %v", args, err)
	}
	if !autostartMatches(command, executable, dataDir) {
		t.Fatal("current command not recognized")
	}
	if autostartMatches(command, `C:\Different Install\ToRouter.exe`, dataDir) {
		t.Fatal("another executable reported enabled")
	}
	if autostartMatches(command, executable, `C:\Different Data`) {
		t.Fatal("another data directory reported enabled")
	}
}

// The quit message must actually reach the loop, otherwise the process would
// only ever leave through the 20-second safety net.
func TestQuitMessageExitsLoop(t *testing.T) {
	// The window and the pump must live on the same thread: GetMessage with a
	// NULL filter only returns the calling thread's queue, so a window created
	// elsewhere would never see the quit message. Run does both on one
	// goroutine; this mirrors that exactly.
	s := &trayState{cfg: Config{}}
	ready, done := make(chan struct{}), make(chan struct{})
	go func() {
		// Same contract Run provides: window and pump on one OS thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := s.createWindow(); err != nil {
			t.Errorf("create tray window: %v", err)
			current = nil
			close(done)
			return
		}
		current = s
		close(ready)
		s.loop()
		current = nil
		close(done)
	}()
	<-ready
	s.quit()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("message loop did not exit after quit")
	}
}

// End to end: Run with a real window and icon, quit through the same message
// the menu posts, and the hook must run after the loop has returned. Before the
// fix the hook ran inside DispatchMessage and froze the tray.
//
// The quit is driven by closing serveDone, which is what happens when the
// server stops on its own (console "退出服务", crash). It ends in the same
// PostMessage(wmTrayQuit) as the menu path, and unlike reaching into the
// package-level `current` it cannot race with Run's own initialisation.
func TestRunQuitPathEndToEnd(t *testing.T) {
	trayClass := windows.StringToUTF16Ptr("Shell_TrayWnd")
	hTray, _, _ := user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(trayClass)), 0)
	if hTray == 0 {
		t.Skip("system tray (Shell_TrayWnd) is unavailable in this desktop session")
	}
	icon := filepath.Join(t.TempDir(), "tray.ico")
	if err := WriteIcon(icon); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	hook := make(chan struct{})
	returned := make(chan error, 1)
	go func() {
		returned <- Run(Config{
			Title: "test", Tooltip: "test", IconPath: icon,
			OnQuit: func() { close(hook) },
		}, serveDone)
	}()
	time.Sleep(100 * time.Millisecond)
	close(serveDone)
	select {
	case <-hook:
	case err := <-returned:
		t.Fatalf("Run returned %v before the quit hook ran", err)
	case <-time.After(10 * time.Second):
		t.Fatal("quit hook never ran after the loop exited")
	}
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after quit")
	}
}

// The clipboard and registry procedures are hand-declared too; a wrong DLL
// only shows up as a panic at call time (see the DeleteObject/gdi32 fix), so
// exercise them once here.
func TestClipboardAndRegistryProceduresResolve(t *testing.T) {
	const text = "to-router-tray-test"
	if err := CopyToClipboard(text); err != nil {
		t.Skipf("clipboard unavailable in this session: %v", err)
	}
	got, err := clipboardText()
	if err != nil {
		t.Fatalf("clipboard read back: %v", err)
	}
	if got != text {
		t.Fatalf("clipboard round trip: %q", got)
	}
	// Read-only: just prove the registry procedure resolves and answers.
	_ = AutostartEnabled()
}
