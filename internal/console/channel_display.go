package console

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func channelDisplayRoutes(router chi.Router, st *store.Store) {
	router.Get("/channels/display", func(w http.ResponseWriter, r *http.Request) {
		v, err := st.GetChannelDisplay()
		if err != nil {
			fail(w, 500, "cannot read channel display preferences")
			return
		}
		respond(w, 200, v)
	})
	router.Put("/channels/display", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SortMode json.RawMessage `json:"sort_mode"`
			Order    json.RawMessage `json:"order"`
			Icon     *struct {
				ChannelID int64   `json:"channel_id"`
				Value     *string `json:"value"`
			} `json:"icon"`
		}
		if !decode(w, r, &body) {
			return
		}
		patch := store.ChannelDisplayPatch{}
		if body.SortMode != nil {
			var mode string
			if err := json.Unmarshal(body.SortMode, &mode); err != nil || (mode != "name" && mode != "manual") {
				fail(w, 400, "sort_mode must be name or manual")
				return
			}
			patch.SortMode = &mode
		}
		if body.Order != nil {
			var order []int64
			if err := json.Unmarshal(body.Order, &order); err != nil || order == nil {
				fail(w, 400, "order must be an array of channel IDs")
				return
			}
			patch.Order = &order
		}
		if body.Icon != nil {
			if body.Icon.Value == nil {
				fail(w, 400, "icon requires a string value")
				return
			}
			patch.Icon = &store.ChannelDisplayIcon{ChannelID: body.Icon.ChannelID, Value: *body.Icon.Value}
		}
		v, err := st.PatchChannelDisplay(patch)
		if err != nil {
			if errors.Is(err, store.ErrInvalidChannelDisplay) {
				fail(w, 400, err.Error())
			} else {
				fail(w, 500, "cannot save channel display preferences")
			}
			return
		}
		respond(w, 200, v)
	})
}
