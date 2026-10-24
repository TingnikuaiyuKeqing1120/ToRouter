package gateway

import "testing"

func TestRuntimeReasonKeepsHTTPFailureDistinctFromStream(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"upstream returned HTTP 429", "upstream returned HTTP 429"},
		{"the upstream returned HTTP 503; private fixture details", "upstream returned HTTP 503"},
		{"upstream request rejected: private fixture details", "request could not be completed"},
		{"upstream connection failed: private fixture details", "connection failed"},
		{"stream ended: private fixture details", "stream"},
	} {
		if got := safeRuntimeReason(tc.input); got != tc.want {
			t.Errorf("reason for %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
