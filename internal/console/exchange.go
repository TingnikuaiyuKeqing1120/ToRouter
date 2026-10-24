package console

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"torouter/internal/pricing"
	"torouter/internal/store"
)

func exchangeRoutes(r chi.Router, s *store.Store) {
	exchangeRoutesWithService(r, s, pricing.NewExchangeService(s))
}
func exchangeRoutesWithService(r chi.Router, s *store.Store, fx *pricing.ExchangeService) {
	rate := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var v store.ExchangeRate
		var err error
		if r.Method == http.MethodPost {
			v, err = fx.Refresh(r.Context())
		} else {
			v, err = fx.Get(r.Context())
		}
		if err != nil {
			fail(w, 500, "无法保存汇率查询结果")
			return
		}
		respond(w, 200, v)
	}
	r.Get("/exchange-rate", rate)
	r.Post("/exchange-rate", rate)
	sources := func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Sources []string `json:"sources"`
		}
		if r.Method == http.MethodPut {
			if !decode(w, r, &body) {
				return
			}
			if err := s.SaveExchangeSources(body.Sources); err != nil {
				fail(w, 400, "来源配置无效或无法保存")
				return
			}
		}
		ids, err := s.GetExchangeSources()
		if err != nil {
			fail(w, 500, "无法读取汇率来源")
			return
		}
		respond(w, 200, map[string]any{"sources": ids, "available": store.AvailableExchangeSources()})
	}
	r.Get("/exchange-rate/sources", sources)
	r.Put("/exchange-rate/sources", sources)
}
