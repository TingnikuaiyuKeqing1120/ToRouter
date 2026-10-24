package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"torouter/internal/buildinfo"
)

type restartReady struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Version string `json:"version"`
}

func restartClient() *http.Client {
	return &http.Client{Timeout: 500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func restartArgs(dataDir string, port int, token string) []string {
	return []string{"-mode", "daemon", "-data", dataDir, "-port", strconv.Itoa(port), "-autostart", "-restart-grace", strconv.Itoa(restartHandoverSeconds), "-restart-ready", token}
}

// Readiness requires a real HTTP response to the random challenge from the
// expected child process. No administrator credential goes to an occupied port.
func relaunch(dataDir string, port int) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	token := hex.EncodeToString(nonce)
	command := exec.Command(executable, restartArgs(dataDir, port, token)...)
	detachRestart(command)
	if err := command.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	client := restartClient()
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(time.Duration(restartHandoverSeconds+5) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("child exited before becoming ready: %w", errOrUnexpectedExit(err))
		case <-deadline.C:
			_ = command.Process.Kill()
			<-exited
			return errors.New("child did not become ready before the restart deadline; child stopped")
		case <-ticker.C:
			req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/_internal/restart-ready", port), nil)
			req.Header.Set("X-To-Router-Restart", token)
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			var ready restartReady
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&ready)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && ready.PID == command.Process.Pid && ready.Port == port && ready.Version == buildinfo.Version {
				select {
				case err := <-exited:
					return fmt.Errorf("child exited during readiness: %w", errOrUnexpectedExit(err))
				default:
					return nil
				}
			}
		}
	}
}

func errOrUnexpectedExit(err error) error {
	if err == nil {
		return errors.New("unexpected successful exit")
	}
	return err
}
