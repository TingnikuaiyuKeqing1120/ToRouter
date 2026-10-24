package console

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/quota"
	"torouter/internal/store"
)

type quotaRefreshInput struct {
	ChannelID    int64  `json:"channel_id"`
	CredentialID string `json:"credential_id"`
	Force        bool   `json:"force"`
}

type quotaList struct {
	Items          []quota.Snapshot `json:"items"`
	RefreshSeconds int              `json:"refresh_seconds"`
}

func listQuotas(st *store.Store, svc *quota.Service) (quotaList, error) {
	out := quotaList{Items: []quota.Snapshot{}, RefreshSeconds: svc.RefreshSeconds()}
	channels, err := st.ListChannels()
	if err != nil {
		return out, err
	}
	for _, c := range channels {
		for _, cred := range c.Credentials {
			if quota.Supported(c, cred) {
				out.Items = append(out.Items, svc.Snapshot(c, cred))
			}
		}
	}
	return out, nil
}

func validateQuotaTargets(st *store.Store, targets *[]store.QuotaTarget) error {
	if targets == nil {
		return nil
	}
	if len(*targets) > 100 {
		return errors.New("最多关注100个额度凭证")
	}
	seen := make(map[store.QuotaTarget]bool)
	for _, target := range *targets {
		if target.ChannelID <= 0 || target.CredentialID == "" || seen[target] {
			return errors.New("额度关注项必须为不重复的精确凭证")
		}
		seen[target] = true
		c, err := st.Channel(target.ChannelID)
		if err != nil {
			return errors.New("额度关注渠道不存在")
		}
		found := false
		for _, cred := range c.Credentials {
			if cred.ID == target.CredentialID {
				found = quota.Supported(c, cred)
				break
			}
		}
		if !found {
			return errors.New("额度关注凭证不存在或不支持额度查询")
		}
	}
	return nil
}

func quotaDisplayRoutes(router chi.Router, st *store.Store, hooks Hooks) {
	router.Get("/quotas", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		v, err := listQuotas(st, hooks.Quotas)
		if err != nil {
			fail(w, 500, "无法读取额度快照")
			return
		}
		respond(w, 200, v)
	})
	router.Post("/quotas/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in quotaRefreshInput
		if !decode(w, r, &in) {
			return
		}
		c, cred, ok := readStatusTarget(w, st, statusTarget{ChannelID: in.ChannelID, CredentialID: in.CredentialID})
		if !ok {
			return
		}
		if !quota.Supported(c, cred) {
			fail(w, 400, "此凭证不支持额度查询")
			return
		}
		v, observation := hooks.Quotas.RefreshSnapshot(r.Context(), c, cred, in.Force)
		if hooks.ObserveQuota != nil {
			hooks.ObserveQuota(c, cred, observation)
		}
		respond(w, 200, v)
	})
}
