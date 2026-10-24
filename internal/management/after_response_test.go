package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"torouter/internal/store"
)

func hookRequest(origin, token, id string) *http.Request {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "runtime_hook", "arguments": map[string]any{"request_id": id}}})
	r := httptest.NewRequest("POST", origin+"/mcp", strings.NewReader(string(body)))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}

func TestAfterResponseDurableFlushedAndExactlyOnce(t *testing.T) {
	st, _ := testStore(t)
	c, token, err := st.CreateManagementConnection("runtime", []string{Read, Maintain})
	if err != nil {
		t.Fatal(err)
	}
	var calls, handlers atomic.Int32
	done := make(chan error, 1)
	recorder := httptest.NewRecorder()
	op := Operation{Name: "runtime_hook", Capability: "runtime.test", Permission: Maintain, InputSchema: map[string]any{"type": "object", "additionalProperties": false}, Handler: func(ctx context.Context, _ Principal, _ json.RawMessage) (any, error) {
		handlers.Add(1)
		if err := AfterResponse(ctx, func() {
			calls.Add(1)
			r, err := st.ManagementReceipt(c.ID, "once")
			if err == nil && (r.Status != "completed" || !recorder.Flushed || !strings.Contains(recorder.Body.String(), `"status":"completed"`)) {
				err = errors.New("hook ran before durable receipt and flushed response")
			}
			done <- err
		}); err != nil {
			return nil, err
		}
		return map[string]any{"accepted": true}, nil
	}}
	srv := New(st, "http://127.0.0.1:12345", []Operation{op})
	srv.Handler().ServeHTTP(recorder, hookRequest(srv.origin, token, "once"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook was not executed")
	}
	if calls.Load() != 1 || handlers.Load() != 1 {
		t.Fatal("first hook missing")
	}
	replayed := httptest.NewRecorder()
	srv.Handler().ServeHTTP(replayed, hookRequest(srv.origin, token, "once"))
	if !strings.Contains(replayed.Body.String(), `"status":"completed"`) || calls.Load() != 1 || handlers.Load() != 1 {
		t.Fatalf("replay executed hook: %s calls=%d handlers=%d", replayed.Body, calls.Load(), handlers.Load())
	}
}

func TestAfterResponseNotArmedOnFailure(t *testing.T) {
	for _, mode := range []string{"handler_error", "marshal_error", "receipt_save_error", "permission_revoked", "unknown_result"} {
		t.Run(mode, func(t *testing.T) {
			st, _ := testStore(t)
			c, token, err := st.CreateManagementConnection("runtime", []string{Read, Maintain})
			if err != nil {
				t.Fatal(err)
			}
			p := Principal{ID: c.ID, Permissions: c.Permissions, store: st, token: token}
			srv := New(st, "http://127.0.0.1:12345", nil)
			queue := &afterResponseQueue{}
			ctx := context.WithValue(context.Background(), afterResponseKey{}, queue)
			calls := 0
			op := Operation{Name: "runtime_hook", Permission: Maintain, Handler: func(ctx context.Context, _ Principal, _ json.RawMessage) (any, error) {
				if err := AfterResponse(ctx, func() { calls++ }); err != nil {
					return nil, err
				}
				switch mode {
				case "handler_error":
					return nil, NewError("invalid_arguments", "Rejected")
				case "marshal_error":
					return make(chan int), nil
				case "receipt_save_error":
					_ = st.Close()
				case "permission_revoked":
					disabled := false
					_, err := st.UpdateManagementConnection(c.ID, store.ManagementPatch{Enabled: &disabled})
					if err != nil {
						return nil, err
					}
				case "unknown_result":
					return map[string]any{"status": "unknown"}, nil
				}
				return map[string]any{"accepted": true}, nil
			}}
			_, _ = srv.invoke(ctx, p, op, json.RawMessage(`{"request_id":"failure"}`))
			queue.run(context.Background())
			if calls != 0 {
				t.Fatal("failed operation armed a hook")
			}
		})
	}
}

type failingHookWriter struct {
	*httptest.ResponseRecorder
	writeFailure, flushFailure bool
}

func (w *failingHookWriter) Write(data []byte) (int, error) {
	if w.writeFailure {
		return 0, errors.New("synthetic write failure")
	}
	return w.ResponseRecorder.Write(data)
}
func (w *failingHookWriter) FlushError() error {
	if w.flushFailure {
		return errors.New("synthetic flush failure")
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestAfterResponseNotExecutedOnHTTPFailureOrLateRevocation(t *testing.T) {
	for _, mode := range []string{"write", "flush", "late_revoke"} {
		t.Run(mode, func(t *testing.T) {
			st, _ := testStore(t)
			c, token, err := st.CreateManagementConnection("runtime", []string{Read, Maintain})
			if err != nil {
				t.Fatal(err)
			}
			calls := atomic.Int32{}
			var queue *afterResponseQueue
			op := Operation{Name: "runtime_hook", Permission: Maintain, InputSchema: map[string]any{"type": "object", "additionalProperties": false}, Handler: func(ctx context.Context, _ Principal, _ json.RawMessage) (any, error) {
				queue, _ = ctx.Value(afterResponseKey{}).(*afterResponseQueue)
				err := AfterResponse(ctx, func() { calls.Add(1) })
				return map[string]any{"accepted": true}, err
			}}
			srv := New(st, "http://127.0.0.1:12345", []Operation{op})
			writer := &failingHookWriter{ResponseRecorder: httptest.NewRecorder(), writeFailure: mode == "write", flushFailure: true}
			srv.Handler().ServeHTTP(writer, hookRequest(srv.origin, token, "http-failure"))
			if queue == nil {
				t.Fatal("handler did not execute")
			}
			if mode == "late_revoke" {
				disabled := false
				if _, err = st.UpdateManagementConnection(c.ID, store.ManagementPatch{Enabled: &disabled}); err != nil {
					t.Fatal(err)
				}
				queue.run(context.Background())
			}
			if calls.Load() != 0 {
				t.Fatal("HTTP failure or revoked authorization executed runtime hook")
			}
		})
	}
}

func TestAfterResponseRejectsOutsideWrite(t *testing.T) {
	if err := AfterResponse(context.Background(), func() {}); err == nil {
		t.Fatal("hook accepted without a management write")
	}
}
