package console

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"torouter/internal/store"
)

func modelSpecRoutes(r chi.Router, s *store.Store) {
	r.Get("/model-specs", func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.ListModelSpecs()
		if err != nil {
			fail(w, 500, "cannot read model specifications")
			return
		}
		respond(w, 200, rows)
	})
	r.Put("/model-specs", func(w http.ResponseWriter, r *http.Request) {
		var row store.ModelSpec
		if !decode(w, r, &row) {
			return
		}
		if err := s.SaveModelSpecs(row); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, row)
	})
}
