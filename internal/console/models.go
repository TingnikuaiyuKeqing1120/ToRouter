package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"torouter/internal/gateway"
	"torouter/internal/store"
)

func modelRoutes(r chi.Router, s *store.Store) {
	r.Get("/models/specs", (&gateway.Gateway{Store: s}).ModelSpecs)
	modelSpecRoutes(r, s)
	r.Get("/models", func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.ListDownstreamModels()
		if err != nil {
			fail(w, 500, "cannot read models")
			return
		}
		channels, err := s.ListChannels()
		if err != nil {
			fail(w, 500, "cannot read channels")
			return
		}
		byID := map[int64]store.Channel{}
		for _, c := range channels {
			byID[c.ID] = c
		}
		settings, err := s.GetSettings()
		if err != nil {
			fail(w, 500, "cannot read settings")
			return
		}
		tags, groups, err := modelTagDefinitions(s)
		if err != nil {
			fail(w, 500, "cannot read tags")
			return
		}
		out := []map[string]any{}
		for _, m := range rows {
			m.Tags = store.FilterDownstreamModelTags(m.Tags, tags, groups)
			bindings := []map[string]any{}
			for _, b := range m.Bindings {
				c, ok := byID[b.ChannelID]
				entry := map[string]any{
					"channel_id": b.ChannelID, "model": b.Model,
					"priority": b.Priority,
					"enabled":  b.Enabled, "channel_exists": ok,
				}
				if ok {
					entry["channel_name"] = c.Name
					entry["protocol"] = c.Family()
					// Effective values differ per binding; report each one.
					resolved := store.ResolveConfig(c.ModelConfigs[b.Model])
					entry["effective"] = map[string]any{"config": resolved.Config, "sources": resolved.Sources}
				}
				bindings = append(bindings, entry)
			}
			out = append(out, map[string]any{
				"id": m.ID, "display_name": m.DisplayName, "enabled": m.Enabled, "fallback": m.Fallback,
				"sort_order":            m.SortOrder,
				"bindings":              bindings,
				"tags":                  m.Tags,
				"effective_tags":        gateway.ComputeEffectiveTags(m, byID, tags, groups),
				"effective_tag_details": gateway.ComputeEffectiveTagsDetail(m, byID, tags, groups),
				"catalog_model_ref":     m.CatalogModelRef,
				"associated_prices":     modelPrices(settings.Prices, m.ID),
			})
		}
		respond(w, 200, out)
	})

	r.Put("/models/reorder", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			fail(w, 400, "invalid body: "+err.Error())
			return
		}
		var order []string
		trimmed := strings.TrimSpace(string(body))
		if strings.HasPrefix(trimmed, "[") {
			if err := json.Unmarshal(body, &order); err != nil {
				fail(w, 400, "invalid JSON array: "+err.Error())
				return
			}
		} else {
			var wrapper struct {
				Order []string `json:"order"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				fail(w, 400, "invalid JSON object: "+err.Error())
				return
			}
			order = wrapper.Order
		}
		if err := s.ReorderDownstreamModels(order); err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})

	r.Put("/models/batch/status", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs     []string `json:"ids"`
			Enabled bool     `json:"enabled"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := s.BatchUpdateDownstreamModelsStatus(body.IDs, body.Enabled); err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})

	r.Post("/models/batch-delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IDs []string `json:"ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := s.BatchDeleteDownstreamModels(body.IDs); err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})

	r.Post("/models/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode   string            `json:"mode"`
			Models []json.RawMessage `json:"models"`
			Prices json.RawMessage   `json:"prices"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Mode != "merge" && body.Mode != "overwrite" {
			fail(w, 400, "mode must be 'merge' or 'overwrite'")
			return
		}
		models := []store.DownstreamModel{}
		for _, raw := range body.Models {
			m, err := store.DecodeDownstreamModelImport(raw)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			models = append(models, m)
		}
		var prices []store.Price
		if len(body.Prices) > 0 && string(body.Prices) != "null" {
			var err error
			prices, err = store.DecodePricesImport(body.Prices)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		if err := s.ImportDownstreamModelsWithPrices(models, prices, body.Mode == "overwrite"); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]any{"ok": true, "count": len(body.Models)})
	})

	save := func(w http.ResponseWriter, r *http.Request) {
		var m store.DownstreamModel
		if !decode(w, r, &m) {
			return
		}
		if r.Method == "PUT" {
			id, ok := decodedPathID(w, r)
			if !ok {
				return
			}
			if id != m.ID {
				fail(w, 400, "body ID does not match URL")
				return
			}
		} else {
			m.ID = strings.TrimSpace(m.ID)
		}
		if !normalizeModelTags(w, s, &m) {
			return
		}
		// A re-enabled model whose bindings point at channel models the
		// catalogue no longer lists still saves: the owner asked for it, and the
		// binding is valid while the channel has the model enabled. They just
		// get to hear it (downstreamReenableNotice). The stored row is read
		// before the save: "was it disabled?" can only be answered by what the
		// model was, not by what it just became.
		previous := storedModel(s, m.ID)
		if r.Method == "PUT" && m.SortOrder == 0 && previous != nil {
			m.SortOrder = previous.SortOrder
		}
		var err error
		if r.Method == http.MethodPost {
			err = s.CreateDownstreamModel(m)
		} else {
			err = s.SaveDownstreamModel(m)
		}
		if err != nil {
			modelSaveError(w, err)
			return
		}
		if notice := downstreamReenableNotice(s, previous, m); notice != "" {
			respond(w, 200, map[string]any{"model": m, "notice": notice})
			return
		}
		respond(w, 200, m)
	}
	r.Post("/models", save)
	r.Put("/models/{id}", save)
	r.Post("/models/{id}/rename", func(w http.ResponseWriter, r *http.Request) {
		var m store.DownstreamModel
		if !decode(w, r, &m) {
			return
		}
		oldID, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		if !normalizeModelTags(w, s, &m) {
			return
		}
		previous := storedModel(s, oldID)
		if err := s.RenameDownstreamModel(oldID, m); err != nil {
			modelSaveError(w, err)
			return
		}
		if notice := downstreamReenableNotice(s, previous, m); notice != "" {
			respond(w, 200, map[string]any{"model": m, "notice": notice})
			return
		}
		respond(w, 200, m)
	})
	r.Delete("/models/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteDownstreamModel(id); err != nil {
			if err == store.ErrNotFound {
				fail(w, 404, "model not found")
				return
			}
			fail(w, 500, "delete failed")
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
}

func modelTagDefinitions(s *store.Store) ([]store.Tag, []store.TagGroup, error) {
	tags, err := s.ListTags()
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.ListTagGroups()
	return tags, groups, err
}

func normalizeModelTags(w http.ResponseWriter, s *store.Store, m *store.DownstreamModel) bool {
	tags, groups, err := modelTagDefinitions(s)
	if err != nil {
		fail(w, 500, "cannot read tags")
		return false
	}
	m.Tags = store.FilterDownstreamModelTags(m.Tags, tags, groups)
	m.EffectiveTags = nil
	return true
}

func modelSaveError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrModelExists) || errors.Is(err, store.ErrModelPriceConflict) {
		status = http.StatusConflict
	} else if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	}
	fail(w, status, err.Error())
}

func modelPrices(prices []store.Price, model string) []store.Price {
	result := []store.Price{}
	for _, p := range prices {
		if p.ChannelID == 0 && p.Model == model {
			result = append(result, p)
		}
	}
	return result
}

// downstreamReenableNotice names the binding models a save re-enables while the
// channel's catalogue no longer lists them, so the operator can see what a
// catalogue replacement took off the shelf. The catalogue is the only evidence
// consulted, and it is evidence about the last fetch, not a verdict: a model the
// fetch did not return may have been renamed, retired, or simply not fetched
// this time, so the notice says exactly that and never blocks the save.
func downstreamReenableNotice(s *store.Store, previous *store.DownstreamModel, saved store.DownstreamModel) string {
	if !saved.Enabled || previous == nil || previous.Enabled {
		// Not a re-enable: a brand-new model has no history to compare against,
		// and an already-enabled one was told when it was enabled.
		return ""
	}
	channels, err := s.ListChannels()
	if err != nil {
		return ""
	}
	missing := []string{}
	for _, b := range saved.Bindings {
		for _, c := range channels {
			if c.ID != b.ChannelID {
				continue
			}
			if !slices.Contains(c.AvailableModels, b.Model) && !slices.Contains(missing, b.Model) {
				missing = append(missing, b.Model)
			}
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "上游当前目录中没有绑定模型 " + strings.Join(missing, "、") + "（上游可能已下线或已更名，也可能这次未拉取到）；已照常启用，请自行确认可用性。"
}

// storedModel is the stored row carrying an id, nil when nothing does. It is
// read before a save so the re-enable notice compares against the model as it
// was; a read failure answers nil, which costs the notice and nothing else.
func storedModel(s *store.Store, id string) *store.DownstreamModel {
	rows, err := s.ListDownstreamModels()
	if err != nil {
		return nil
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}
