package console

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func oauthAutoOpenRoutes(router chi.Router, st *store.Store) {
	router.Put("/settings/oauth-auto-open", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled *bool `json:"oauth_auto_open"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Enabled == nil {
			fail(w, 400, "body requires only a boolean oauth_auto_open")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(w, 400, "body must contain one JSON object")
			return
		}
		v, err := st.PatchOAuthAutoOpen(*body.Enabled)
		if err != nil {
			fail(w, 500, "cannot save OAuth auto-open setting")
			return
		}
		respond(w, 200, v)
	})
}
