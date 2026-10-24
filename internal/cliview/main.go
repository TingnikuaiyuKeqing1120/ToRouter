// Package cliview attaches a terminal log view to the shared daemon.
package cliview

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"torouter/internal/app"
	"torouter/internal/console"
)

// Run displays an attached CLI view; closing it leaves the daemon running.
func Run(args []string) error {
	return run(args, flag.NewFlagSet(os.Args[0], flag.ContinueOnError))
}

func run(args []string, flags *flag.FlagSet) error {
	port := flags.Int("port", 0, "HTTP listen port (loopback only); omitted uses the configured port")
	dir := flags.String("data", app.DefaultDir(), "data directory")
	flags.Lookup("data").DefValue = console.Redact(*dir)
	_ = flags.Bool("attach", false, "attach a CLI view to the daemon even with an explicit port")
	showKey := flags.Bool("show-key", false, "print administrator login key and exit")
	_ = flags.Int("restart-grace", 0, "seconds to wait for the previous instance to release the port")
	_ = flags.Bool("autostart", false, "silent restart compatibility; terminal never opens a browser")
	_ = flags.String("restart-ready", "", "internal restart readiness challenge")
	// Flag's default usage includes argv[0], and parse errors may echo arbitrary
	// argument values. Mask the full output, not only individual default values.
	flags.SetOutput(redactedWriter{flags.Output()})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *port != 0 && (*port < 1 || *port > 65535) {
		return errors.New("port must be between 1 and 65535")
	}
	if *showKey {
		key, err := app.ReadAdminKey(*dir)
		if err != nil {
			return err
		}
		fmt.Println(key)
		return nil
	}
	release, existing, viewErr := claimCLIView(*dir)
	if existing {
		if viewErr != nil {
			fmt.Fprintln(os.Stderr, console.Redact(viewErr.Error()))
		}
		return nil
	}
	if viewErr != nil {
		return viewErr
	}
	defer release()
	targetPort, err := app.ConnectDaemon(*dir, *port)
	if err != nil {
		return err
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", targetPort)
	logPath := app.LogPath(*dir)
	writeBanner(os.Stdout, origin, *dir, true)
	fmt.Println("[CLI 视图] 已连接至后台服务，正在实时输出日志流...")
	fmt.Println("提示: 按 Ctrl+C 或关闭此窗口仅退出命令行视图，后台路由与托盘服务将继续保持运行。")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	installation, err := app.InstallationID(*dir)
	if err != nil {
		return err
	}
	viewCtx, disconnect := context.WithCancelCause(ctx)
	defer disconnect(nil)
	go func() {
		disconnect(followDaemon(viewCtx, origin, installation, func() (int, error) { return app.ResolvePort(*dir, 0) }))
	}()
	return streamLogFile(viewCtx, logPath)
}

type redactedWriter struct{ io.Writer }

func (w redactedWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(w.Writer, console.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writeBanner(w io.Writer, origin, dataDir string, attached bool) {
	format := "to-router: %s\nData: %s\nLog: %s\nAdmin login key: ToRouter.exe -mode cli -show-key (same -data directory). Create client API keys in Client Access.\n"
	if attached {
		format = "to-router 后端已在运行: %s\n数据目录: %s\n日志文件: %s\n"
	}
	_, _ = fmt.Fprint(w, console.Redact(fmt.Sprintf(format, origin, dataDir, app.LogPath(dataDir))))
}

func streamLogFile(ctx context.Context, logPath string) error {
	for i := 0; i < 15; i++ {
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	f, err := os.Open(logPath)
	if err != nil {
		fmt.Printf("无法打开日志文件: %s\n", console.Redact(err.Error()))
		<-ctx.Done()
		return nil
	}
	defer f.Close()

	if fi, err := f.Stat(); err == nil && fi.Size() > 4096 {
		_, _ = f.Seek(-4096, io.SeekEnd)
		// Discard partial line
		r := bufio.NewReader(f)
		_, _ = r.ReadString('\n')
	}

	reader := bufio.NewReader(f)
	for {
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), errDaemonStopped) {
				fmt.Println("\n后台服务已停止，CLI 视图退出。")
			} else if errors.Is(context.Cause(ctx), errDaemonUnavailable) {
				fmt.Println("\n后台服务连接已断开，CLI 视图退出。")
			} else {
				fmt.Println("\nCLI 视图已断开，后台服务继续运行。")
			}
			return nil
		default:
			line, err := reader.ReadString('\n')
			if err == nil {
				fmt.Print(console.Redact(line))
			} else {
				time.Sleep(150 * time.Millisecond)
			}
		}
	}
}
