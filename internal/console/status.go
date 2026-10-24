package console

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/quota"
	"torouter/internal/store"
)

type statusTarget struct {
	ChannelID    int64  `json:"channel_id"`
	CredentialID string `json:"credential_id"`
	Model        string `json:"model,omitempty"`
}

func statusRoutes(router chi.Router, st *store.Store, hooks Hooks) {
	router.Get("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if hooks.RuntimeStatus == nil {
			fail(w, http.StatusServiceUnavailable, "运行状态暂不可用")
			return
		}
		respond(w, http.StatusOK, hooks.RuntimeStatus())
	})
	router.Post("/status/reset", func(w http.ResponseWriter, r *http.Request) {
		var target statusTarget
		if !decode(w, r, &target) {
			return
		}
		if _, _, ok := readStatusTarget(w, st, target); !ok {
			return
		}
		if hooks.ResetStatus == nil {
			fail(w, http.StatusServiceUnavailable, "此运行环境无法恢复状态")
			return
		}
		if err := hooks.ResetStatus(target.ChannelID, target.CredentialID, target.Model); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
	router.Post("/status/refresh-quota", func(w http.ResponseWriter, r *http.Request) {
		var target statusTarget
		if !decode(w, r, &target) {
			return
		}
		channel, credential, ok := readStatusTarget(w, st, target)
		if !ok {
			return
		}
		if target.Model != "" || !quota.Supported(channel, credential) {
			fail(w, http.StatusBadRequest, "此凭据不支持额度查询")
			return
		}
		if hooks.RefreshQuota == nil {
			fail(w, http.StatusServiceUnavailable, "此运行环境无法刷新额度")
			return
		}
		if err := hooks.RefreshQuota(r.Context(), channel, credential); err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		respond(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// Resolve only configured identities. A malformed target must not turn a local
// restore or quota refresh into a channel-wide operation.
func readStatusTarget(w http.ResponseWriter, st *store.Store, target statusTarget) (store.Channel, store.CredentialItem, bool) {
	if target.ChannelID <= 0 || target.CredentialID == "" {
		fail(w, http.StatusBadRequest, "请选择渠道及凭据")
		return store.Channel{}, store.CredentialItem{}, false
	}
	channel, err := st.Channel(target.ChannelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, http.StatusNotFound, "渠道不存在")
		} else {
			fail(w, http.StatusInternalServerError, "无法读取渠道")
		}
		return store.Channel{}, store.CredentialItem{}, false
	}
	for _, cred := range channel.Credentials {
		if cred.ID == target.CredentialID {
			return channel, cred, true
		}
	}
	fail(w, http.StatusNotFound, "凭据不存在")
	return store.Channel{}, store.CredentialItem{}, false
}
