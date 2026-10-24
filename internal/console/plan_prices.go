package console

import (
	"errors"
	"net/http"
	"strconv"
	"time"
	"torouter/internal/store"

	"github.com/go-chi/chi/v5"
)

func planPricesRoutes(r chi.Router, s *store.Store) {
	channelID := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id <= 0 {
			fail(w, http.StatusBadRequest, "渠道编号无效")
			return 0, false
		}
		return id, true
	}
	saveError := func(w http.ResponseWriter, err error) {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, http.StatusNotFound, "渠道或套餐配置不存在")
		} else {
			fail(w, http.StatusBadRequest, err.Error())
		}
	}
	r.Get("/plan-prices", func(w http.ResponseWriter, r *http.Request) {
		month := r.URL.Query().Get("month")
		if month == "" {
			// Callers choose their browser's calendar month. UTC is a stable
			// fallback and never depends on the server's local timezone.
			month = time.Now().UTC().Format("2006-01")
		}
		if err := store.ValidatePlanMonth(month); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		data, err := s.GetPlanPrices(month, time.Now())
		if err != nil {
			fail(w, http.StatusInternalServerError, "无法读取套餐价格")
			return
		}
		respond(w, http.StatusOK, data)
	})
	r.Put("/plan-prices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := channelID(w, r)
		if !ok {
			return
		}
		var body struct {
			store.PlanPriceConfig
			Charge *store.PlanCharge `json:"charge,omitempty"`
		}
		if !decode(w, r, &body) {
			return
		}
		body.ChannelID = id
		if err := s.SavePlanPriceWithCharge(body.PlanPriceConfig, body.Charge, time.Now()); err != nil {
			saveError(w, err)
			return
		}
		respond(w, http.StatusOK, body.PlanPriceConfig)
	})
	r.Delete("/plan-prices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := channelID(w, r)
		if !ok {
			return
		}
		if err := s.DeletePlanPrice(id, time.Now()); err != nil {
			saveError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
	r.Put("/plan-prices/{id}/charges/{month}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := channelID(w, r)
		if !ok {
			return
		}
		var charge store.PlanCharge
		if !decode(w, r, &charge) {
			return
		}
		charge.ChannelID, charge.Month = id, chi.URLParam(r, "month")
		if err := s.SavePlanCharge(charge, time.Now()); err != nil {
			saveError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
	r.Put("/channels/billing", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Changes []store.ChannelBillingChange `json:"changes"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Changes == nil {
			fail(w, http.StatusBadRequest, "套餐声明格式无效")
			return
		}
		if err := s.SaveChannelBilling(body.Changes, time.Now()); err != nil {
			saveError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
}
