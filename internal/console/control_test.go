package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"torouter/internal/store"
)

type controlBody struct {
	io.Reader
	consumed bool
}

func (b *controlBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.consumed = true
	}
	return n, err
}

type controlResponse struct {
	*httptest.ResponseRecorder
	body  *controlBody
	early bool
}

func (w *controlResponse) WriteHeader(status int) {
	if !w.body.consumed {
		w.early = true
	}
	w.ResponseRecorder.WriteHeader(status)
}
func (w *controlResponse) Write(p []byte) (int, error) {
	if !w.body.consumed {
		w.early = true
	}
	return w.ResponseRecorder.Write(p)
}
func (w *controlResponse) Flush() {
	if !w.body.consumed {
		w.early = true
	}
	w.ResponseRecorder.Flush()
}

func TestControlAcknowledgmentConsumesBodyBeforeResponse(t *testing.T) {
	for _, path := range []string{"/restart", "/shutdown"} {
		t.Run(path, func(t *testing.T) {
			s, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			done := make(chan struct{}, 1)
			action := func() { done <- struct{}{} }
			h := New(s, "http://127.0.0.1:17878", Hooks{Restart: action, Shutdown: action})
			body := &controlBody{Reader: strings.NewReader("{}")}
			req := httptest.NewRequest("POST", path, nil)
			req.Body, req.ContentLength, req.Close = io.NopCloser(body), 2, true
			w := &controlResponse{ResponseRecorder: httptest.NewRecorder(), body: body}
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK || w.Body.String() != "{\"ok\":true}\n" {
				t.Fatalf("incomplete acknowledgment: %d %q", w.Code, w.Body.String())
			}
			if w.early || !body.consumed {
				t.Fatal("control response was written or flushed with the POST body still unread")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("accepted action was not executed")
			}
		})
	}
}

func TestControlRequestAcceptsEmptyBodyAndRejectsInvalidBody(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty", "", 200}, {"object", "{}", 200}, {"malformed", "{", 400},
		{"trailing", "{}{}", 400}, {"unknown_parameter", `{"unused":true}`, 400},
		{"too_large", "{}" + strings.Repeat(" ", 4096), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := make(chan struct{}, 1)
			w := httptest.NewRecorder()
			stopAfterResponse(w, httptest.NewRequest("POST", "/restart", strings.NewReader(tc.body)), func() { action <- struct{}{} })
			if w.Code != tc.status {
				t.Fatalf("control response=%d want=%d", w.Code, tc.status)
			}
			if tc.status == 200 {
				select {
				case <-action:
				case <-time.After(time.Second):
					t.Fatal("accepted action was not executed")
				}
			} else {
				select {
				case <-action:
					t.Fatal("invalid request executed control action")
				case <-time.After(450 * time.Millisecond):
				}
			}
		})
	}
}
