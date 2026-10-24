// Package pluginhost implements the private transport for verified provider
// packages. Installation, credential storage and authorization remain outside.
package pluginhost

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed host.mjs
var Bridge []byte

const maxLine = 24 << 20

var (
	ErrClosed   = errors.New("plugin worker is not running")
	ErrProtocol = errors.New("plugin worker protocol failed")
	// An interrupted process may have sent inference or received a rotation.
	// Callers must not replay requests or pretend the account stayed unchanged.
	ErrOutcomeUnknown = errors.New("plugin worker stopped with an unknown operation outcome")
)

type Event struct {
	Name        string          `json:"event"`
	ID          int64           `json:"event_id"`
	OperationID string          `json:"operation_id"`
	Payload     json.RawMessage `json:"payload"`
}
type reply struct {
	Event
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return "plugin: " + e.Message }

// Handler must acknowledge auth only after durable, version/identity-checked
// storage. Other events use the caller's cancellable context. Chunk handlers
// acknowledge consumption, not merely queueing, to preserve backpressure.
type Handler func(context.Context, Event) (json.RawMessage, error)

type Worker struct {
	cmd          *exec.Cmd
	input        io.WriteCloser
	messages     chan reply
	done         chan struct{}
	closed       chan struct{}
	writeMu      sync.Mutex
	callGate     chan struct{}
	closeOnce    sync.Once
	sequence     uint64
	drainTimeout time.Duration
}

// Start accepts only paths already verified by the installation boundary.
// No shell, package manager, runtime flags or inherited NODE_OPTIONS are used.
// Credentials go through private stdin, never argv, environment or a state file.
func Start(runtimePath, bridgePath string) (*Worker, error) {
	if runtimePath == "" || bridgePath == "" {
		return nil, errors.New("plugin runtime or bridge is missing")
	}
	cmd := exec.Command(runtimePath, bridgePath)
	// Deliberately omit ambient credentials, runtime injection flags and proxy
	// settings. The integration supplies its approved network configuration.
	cmd.Env = cleanEnvironment()
	hideWindow(cmd)
	cmd.Stderr = io.Discard // Plugin logs are not trusted diagnostics.
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("cannot open plugin input")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, errors.New("cannot open plugin output")
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, errors.New("cannot start configured plugin runtime")
	}
	w := &Worker{cmd: cmd, input: input, messages: make(chan reply, 1), done: make(chan struct{}), closed: make(chan struct{}), callGate: make(chan struct{}, 1), drainTimeout: 35 * time.Second}
	go w.read(output)
	return w, nil
}

func cleanEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "SYSTEMROOT": true, "WINDIR": true, "TEMP": true, "TMP": true, "PATHEXT": true, "LANG": true, "LC_ALL": true, "TZ": true}
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if allowed[strings.ToUpper(name)] {
			env = append(env, entry)
		}
	}
	return env
}
func (w *Worker) read(output io.ReadCloser) {
	defer close(w.done)
	defer close(w.messages)
	defer w.cmd.Wait()
	defer output.Close()
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	for scanner.Scan() {
		var r reply
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			w.Close()
			return
		}
		// Events and operation replies have distinct identifiers and shapes.
		if (r.Name == "") == (r.ID == "") || r.Name != "" && (r.Event.ID < 1 || r.OperationID == "") {
			w.Close()
			return
		}
		select {
		case w.messages <- r:
		case <-w.closed:
			return
		}
	}
	if scanner.Err() != nil {
		w.Close()
	}
}

func (w *Worker) send(v any) error {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > maxLine-1 {
		return ErrProtocol
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	select {
	case <-w.closed:
		return ErrClosed
	case <-w.done:
		return ErrClosed
	default:
	}
	_, err = w.input.Write(append(raw, '\n'))
	if err != nil {
		return ErrClosed
	}
	return nil
}

// Invoke is serial per worker/account. Cancelling stops new network and stream
// work, but drains a returned rotation to its storage ACK before retirement.
// If the bounded drain fails, report unknown outcome rather than auto-retrying.
func (w *Worker) Invoke(ctx context.Context, method string, input any, handler Handler) (json.RawMessage, error) {
	select {
	case w.callGate <- struct{}{}:
		defer func() { <-w.callGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.closed:
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.sequence++
	id := "op-" + strconv.FormatUint(w.sequence, 10)
	if err := w.send(map[string]any{"id": id, "method": method, "input": input}); err != nil {
		return nil, err
	}
	cancelSignal := ctx.Done()
	var drain <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-cancelSignal:
			cancelSignal = nil
			if method == "authorize" || method == "complete" {
				// Revoked new consent must not be stored, and a provider's
				// polling callback may swallow AbortError indefinitely.
				w.Close()
				return nil, ctx.Err()
			}
			if err := w.send(map[string]any{"method": "cancel", "operation_id": id}); err != nil {
				return nil, ErrOutcomeUnknown
			}
			timer = time.NewTimer(w.drainTimeout)
			drain = timer.C
		case <-drain:
			w.Close()
			return nil, ErrOutcomeUnknown
		case r, ok := <-w.messages:
			if !ok {
				return nil, ErrOutcomeUnknown
			}
			if r.Name == "" {
				if r.ID != id {
					w.Close()
					return nil, ErrProtocol
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if r.Error != "" {
					return nil, &RemoteError{Message: r.Error}
				}
				return r.Result, nil
			}
			if r.OperationID != id {
				w.Close()
				return nil, ErrProtocol
			}
			var result json.RawMessage
			var err error
			switch r.Name {
			case "network", "head", "chunk", "auth":
				if handler == nil {
					err = errors.New("plugin callback is not available")
				} else {
					eventContext := ctx
					if r.Name == "auth" {
						eventContext = context.WithoutCancel(ctx)
					}
					if eventContext.Err() != nil {
						err = eventContext.Err()
					} else {
						result, err = handler(eventContext, r.Event)
					}
				}
			default:
				err = errors.New("unsupported plugin callback")
			}
			answer := map[string]any{"reply_to": r.Event.ID, "result": result}
			if err != nil {
				// Do not put Store errors or raw vendor responses in JS logs.
				answer = map[string]any{"reply_to": r.Event.ID, "error": "parent rejected plugin callback"}
			}
			if err = w.send(answer); err != nil {
				return nil, ErrOutcomeUnknown
			}
		}
	}
}
func (w *Worker) Close() error {
	var err error
	w.closeOnce.Do(func() { close(w.closed); _ = w.input.Close(); err = w.cmd.Process.Kill() })
	return err
}
