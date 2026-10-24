package management

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type afterResponseKey struct{}
type afterResponseScopeKey struct{}
type afterResponseQueue struct {
	mu      sync.Mutex
	entries []afterResponseEntry
}
type afterResponseEntry struct {
	principal   Principal
	permissions []string
	callbacks   []func()
}
type afterResponseScope struct {
	mu        sync.Mutex
	queue     *afterResponseQueue
	closed    bool
	callbacks []func()
}

// AfterResponse schedules a runtime hook from a newly executing write tool.
// The hook runs only after its completed receipt is durable, permissions remain
// valid, and the HTTP response was written and flushed successfully. Replayed
// requests never invoke the domain handler and therefore never schedule hooks.
func AfterResponse(ctx context.Context, callback func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope, _ := ctx.Value(afterResponseScopeKey{}).(*afterResponseScope)
	if callback == nil || scope == nil || scope.queue == nil {
		return NewError("unsupported", "Runtime hooks require an active HTTP management write request.")
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return NewError("conflict", "The management operation has already finished.")
	}
	scope.callbacks = append(scope.callbacks, callback)
	return nil
}

func newAfterResponseScope(ctx context.Context) (context.Context, *afterResponseScope) {
	queue, _ := ctx.Value(afterResponseKey{}).(*afterResponseQueue)
	scope := &afterResponseScope{queue: queue}
	return context.WithValue(ctx, afterResponseScopeKey{}, scope), scope
}

func (scope *afterResponseScope) finish(p Principal, permissions []string, completed bool) {
	scope.mu.Lock()
	defer scope.mu.Unlock()
	scope.closed = true
	if !completed || scope.queue == nil || len(scope.callbacks) == 0 {
		return
	}
	scope.queue.mu.Lock()
	defer scope.queue.mu.Unlock()
	scope.queue.entries = append(scope.queue.entries, afterResponseEntry{principal: p, permissions: append([]string(nil), permissions...), callbacks: append([]func(){}, scope.callbacks...)})
}

func (q *afterResponseQueue) run(ctx context.Context) {
	q.mu.Lock()
	entries := q.entries
	q.entries = nil
	q.mu.Unlock()
	for _, entry := range entries {
		for _, callback := range entry.callbacks {
			allowed := true
			for _, permission := range entry.permissions {
				if entry.principal.Require(ctx, permission) != nil {
					allowed = false
					break
				}
			}
			if allowed {
				callback()
			}
		}
	}
}

func (q *afterResponseQueue) pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries) > 0
}

// Unwrap lets ResponseController use the underlying server's Flush/Hijack
// support without advertising capabilities the underlying writer lacks.
type responseOutcome struct {
	http.ResponseWriter
	status  int
	written int64
	err     error
}

func (w *responseOutcome) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseOutcome) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseOutcome) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.written += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}
