package desktopui

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPetCloseAfterBackendStoppedUsesOwnWindow(t *testing.T) {
	c := newNativeClient(launchConfig{pet: true}, nil)
	closed := make(chan struct{}, 1)
	c.quit = func() { closed <- struct{}{} }
	close(c.ready)
	r := httptest.NewRequest("POST", "/api/desktop/close-pet", nil)
	r.Header.Set("X-To-Router", "1")
	w := httptest.NewRecorder()
	c.serveAPI(w, r)
	if w.Code != 200 {
		t.Fatalf("local close failed without backend: HTTP %d", w.Code)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("own pet window was not closed")
	}
}

func TestLocalPetCloseMethodHeaderAndReadiness(t *testing.T) {
	for _, tc := range []struct {
		method, header string
		ready          bool
		want           int
	}{{"GET", "1", true, 405}, {"POST", "", true, 403}, {"POST", "1", false, 503}} {
		c := newNativeClient(launchConfig{pet: true}, nil)
		if tc.ready {
			close(c.ready)
		}
		c.quit = func() { t.Error("invalid close request invoked quit") }
		r := httptest.NewRequest(tc.method, "/api/desktop/close-pet", nil)
		r.Header.Set("X-To-Router", tc.header)
		w := httptest.NewRecorder()
		c.serveAPI(w, r)
		if w.Code != tc.want {
			t.Fatalf("close %s/%q/ready=%v HTTP %d want %d", tc.method, tc.header, tc.ready, w.Code, tc.want)
		}
	}
}
