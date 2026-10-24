package gateway

import (
	"encoding/json"
	"net/http"

	"torouter/internal/companion"
	"torouter/internal/store"
)

// Keep observation on the existing response writer. In particular, do not
// parse, buffer, or rewrite response bytes to drive the mascot.
type companionWriter struct {
	http.ResponseWriter
	request      *companion.Request
	status       int
	resultStatus int
	failed       bool
	outcome      string
	code         string
}

func (w *companionWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *companionWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	if n > 0 && w.status >= 200 && w.status < 300 {
		w.request.FirstByte()
	}
	if err != nil {
		w.failed = true
	}
	return n, err
}
func (w *companionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *companionWriter) observeError(status int, rec *recovery) {
	if observed, ok := w.ResponseWriter.(interface{ observeError(int, *recovery) }); ok {
		observed.observeError(status, rec)
	}
	w.outcome = "error"
	if rec != nil {
		w.code = rec.Code
	} else if status < 500 {
		w.code = "request_rejected"
	} else if w.code == "" {
		w.code = "upstream_error"
	}
}

func (w *companionWriter) result(u store.Usage) {
	w.resultStatus = u.Status
	if u.Status == 499 {
		w.outcome, w.code = "cancelled", "client_cancelled"
		return
	}
	if u.Status >= 400 {
		w.outcome, w.code = "error", "stream_interrupted"
		return
	}
	if u.Error != "" {
		w.outcome, w.code = "error", "response_incomplete"
		return
	}
	w.outcome, w.code = "success", ""
}

func (w *companionWriter) jsonResult(u store.Usage, format string, body []byte) {
	w.result(u)
	if w.outcome != "success" || format != store.FormatOpenAIResponses {
		return
	}
	var result struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &result)
	if result.Status == "queued" || result.Status == "in_progress" {
		// The HTTP exchange ended, but background inference has not. Do not
		// invent a completion (or keep a request active without a live stream).
		w.outcome = "accepted"
	}
}

func (w *companionWriter) finish(r *http.Request) {
	status, outcome, code := w.status, w.outcome, w.code
	if w.resultStatus != 0 {
		status = w.resultStatus
	}
	if r.Context().Err() != nil || w.failed {
		status, outcome, code = 499, "cancelled", "client_cancelled"
	}
	if status == 0 {
		status = 500
	}
	if outcome == "" {
		outcome = "error"
	}
	w.request.Finish(status, outcome, code)
}
