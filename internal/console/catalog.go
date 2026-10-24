package console

import (
	"context"
	"errors"
	"github.com/go-chi/chi/v5"
	"net/http"
	"time"
	"torouter/internal/catalog"
	"torouter/internal/pricing"
	"torouter/internal/store"
)

func catalogOperationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, pricing.ErrPreviewConflict) {
		status = http.StatusConflict
	}
	fail(w, status, err.Error())
}
func catalogRoutes(r chi.Router, s *store.Store) {
	r.Get("/catalog/models", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.ListCatalogItems(r.URL.Query().Get("source"), r.URL.Query().Get("query"))
		if err != nil {
			fail(w, 500, "cannot query catalog")
			return
		}
		if items == nil {
			items = []store.CatalogItem{}
		}
		respond(w, 200, items)
	})
	r.Get("/catalog/sources", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.GetCatalogSources()
		if err != nil {
			fail(w, 500, "cannot read sources")
			return
		}
		respond(w, 200, v)
	})
	r.Put("/catalog/sources", func(w http.ResponseWriter, r *http.Request) {
		var v []store.CatalogSourceStatus
		if !decode(w, r, &v) {
			return
		}
		if err := s.SaveCatalogSources(v); err != nil {
			catalogOperationError(w, err)
			return
		}
		v, err := s.GetCatalogSources()
		if err != nil {
			fail(w, 500, "cannot read sources")
			return
		}
		respond(w, 200, v)
	})
	r.Post("/catalog/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source string `json:"source"`
		}
		if !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var items []store.CatalogItem
		var err error
		switch req.Source {
		case "openrouter":
			items, err = catalog.FetchOpenRouter(ctx)
		case "litellm":
			items, err = catalog.FetchLiteLLM(ctx)
		default:
			fail(w, 400, "unsupported source")
			return
		}
		if err != nil {
			_ = s.UpdateCatalogSourceError(req.Source, err)
			fail(w, 502, "fetch failed: "+err.Error())
			return
		}
		if err = s.SaveExternalCatalog(req.Source, items); err != nil {
			fail(w, 500, "cannot save catalog")
			return
		}
		respond(w, 200, map[string]any{"source": req.Source, "count": len(items)})
	})
	r.Post("/catalog/offerings/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source string `json:"source"`
			ID     string `json:"id"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Source != "openrouter" || req.ID == "" {
			fail(w, 400, "choose an exact OpenRouter model")
			return
		}
		items, err := s.ListCatalogItems(req.Source, "")
		if err != nil {
			fail(w, 500, "cannot read catalog")
			return
		}
		var selected *store.CatalogItem
		for _, item := range items {
			if item.ID == req.ID {
				v := item
				selected = &v
				break
			}
		}
		if selected == nil {
			fail(w, 404, "catalog model missing; fetch directory first")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		offers, err := catalog.FetchOpenRouterEndpoints(ctx, req.ID)
		if err != nil {
			fail(w, 502, "offering fetch failed: "+err.Error())
			return
		}
		selected.Offerings = offers
		if err = s.SaveCatalogItem(req.Source, req.ID, *selected); err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, selected)
	})
	r.Get("/catalog/matches", func(w http.ResponseWriter, r *http.Request) {
		var ref *store.CatalogModelRef
		if source := r.URL.Query().Get("source"); source != "" && r.URL.Query().Get("id") != "" {
			ref = &store.CatalogModelRef{Source: source, ID: r.URL.Query().Get("id")}
		}
		matches, err := s.CatalogMatches(r.URL.Query().Get("model"), ref)
		if err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, matches)
	})
	r.Post("/catalog/prefill/preview", func(w http.ResponseWriter, r *http.Request) {
		var req store.PrefillRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := s.PreviewCatalogPrefill(req)
		if err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, v)
	})
	r.Post("/catalog/prefill/apply", func(w http.ResponseWriter, r *http.Request) {
		var req store.PrefillRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := s.ApplyCatalogPrefill(req)
		if err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, v)
	})
	r.Post("/usage/backfill/preview", func(w http.ResponseWriter, r *http.Request) {
		var req pricing.BackfillRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := pricing.PreviewBackfill(s, req)
		if err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, v)
	})
	r.Post("/usage/backfill/apply", func(w http.ResponseWriter, r *http.Request) {
		var req pricing.BackfillRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := pricing.ApplyBackfill(s, req)
		if err != nil {
			catalogOperationError(w, err)
			return
		}
		respond(w, 200, v)
	})
}
