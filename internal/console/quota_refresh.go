package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func quotaRefreshRoutes(router chi.Router, st *store.Store) {
	router.Get("/settings/quota-refresh", func(w http.ResponseWriter, r *http.Request) {
		v, err := st.GetQuotaRefresh()
		if err != nil {
			fail(w, 500, "cannot read quota refresh setting")
			return
		}
		respond(w, 200, v)
	})
	router.Put("/settings/quota-refresh", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Seconds *int `json:"quota_refresh_seconds"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		// Do not return decoder errors: unknown field names and type errors can
		// contain caller-supplied secret text.
		if err := decoder.Decode(&body); err != nil || body.Seconds == nil {
			fail(w, 400, "body requires only an integer quota_refresh_seconds")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(w, 400, "body must contain one JSON object")
			return
		}
		v, err := st.PatchQuotaRefresh(*body.Seconds)
		if err != nil {
			if errors.Is(err, store.ErrInvalidQuotaRefresh) {
				fail(w, 400, store.ErrInvalidQuotaRefresh.Error())
			} else {
				fail(w, 500, "cannot save quota refresh setting")
			}
			return
		}
		respond(w, 200, v)
	})
}
