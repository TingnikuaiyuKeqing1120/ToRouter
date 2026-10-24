package console

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"
	"torouter/internal/codex"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

func credentialOperationRoutes(r chi.Router, st *store.Store, ops *codex.Operations, managers ...*plugins.Manager) {
	services := oauthHandlers{}
	if len(managers) > 0 {
		services.plugins = managers[0]
	}
	r.Put("/channels/{channelID}/credential-order", func(w http.ResponseWriter, r *http.Request) {
		id, _, ok := credentialOperationTarget(w, r, false)
		if !ok {
			return
		}
		var body struct {
			IDs []string `json:"credential_ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := st.SetCredentialOrder(id, body.IDs); err != nil {
			credentialOperationError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	r.Put("/channels/{channelID}/credentials/{credentialID}/enabled", func(w http.ResponseWriter, r *http.Request) {
		id, cid, ok := credentialOperationTarget(w, r, true)
		if !ok {
			return
		}
		var body struct {
			Enabled            *bool  `json:"enabled"`
			ExpectedGeneration *int64 `json:"expected_generation"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Enabled == nil {
			fail(w, 400, "enabled must be true or false")
			return
		}
		generation, err := st.SetCredentialEnabledAtGeneration(id, cid, *body.Enabled, body.ExpectedGeneration)
		if err != nil {
			credentialOperationError(w, err)
			return
		}
		respond(w, 200, map[string]any{"ok": true, "generation": generation})
	})
	base := "/channels/{channelID}/credentials/{credentialID}/operations"
	r.Get(base, func(w http.ResponseWriter, r *http.Request) {
		id, cid, ok := credentialOperationTarget(w, r, true)
		if !ok {
			return
		}
		ops, _, resolveErr := services.operationService(r.Context(), false, ops)
		if resolveErr != nil {
			respondManagementTool(w, nil, resolveErr)
			return
		}
		report, err := ops.Report(id, cid)
		if err != nil {
			credentialOperationError(w, err)
			return
		}
		respond(w, 200, report)
	})
	r.Put(base, func(w http.ResponseWriter, r *http.Request) {
		id, cid, ok := credentialOperationTarget(w, r, true)
		if !ok {
			return
		}
		ops, _, resolveErr := services.operationService(r.Context(), false, ops)
		if resolveErr != nil {
			respondManagementTool(w, nil, resolveErr)
			return
		}
		var body struct {
			Identity string `json:"expected_identity"`
			store.CredentialOperationsPatch
		}
		if !decode(w, r, &body) {
			return
		}
		report, err := ops.PatchConfig(id, cid, body.Identity, body.CredentialOperationsPatch)
		if err != nil {
			credentialOperationError(w, err)
			return
		}
		respond(w, 200, report)
	})
	for _, action := range []string{"refresh-reset-cards", "warmup", "consume-reset", "acknowledge"} {
		action := action
		r.Post(base+"/"+action, func(w http.ResponseWriter, r *http.Request) {
			id, cid, ok := credentialOperationTarget(w, r, true)
			if !ok {
				return
			}
			ops, ctx, resolveErr := services.operationService(r.Context(), action != "acknowledge", ops)
			if resolveErr != nil {
				respondManagementTool(w, nil, resolveErr)
				return
			}
			var body struct {
				Identity  string `json:"expected_identity"`
				RequestID string `json:"request_id"`
				Kind      string `json:"kind"`
			}
			if !decode(w, r, &body) {
				return
			}
			var report codex.OperationReport
			var err error
			switch action {
			case "refresh-reset-cards":
				report, err = ops.RefreshCards(ctx, id, cid, body.Identity)
			case "warmup":
				report, err = ops.Warmup(ctx, id, cid, body.Identity, body.RequestID)
			case "consume-reset":
				report, err = ops.ConsumeReset(ctx, id, cid, body.Identity, body.RequestID)
			case "acknowledge":
				report, err = ops.Acknowledge(id, cid, body.Identity, body.Kind)
			}
			if err != nil {
				credentialOperationError(w, err)
				return
			}
			// The receipt describes the outcome, including a failed or unknown
			// upstream result. HTTP 200 only confirms this report was recorded.
			respond(w, 200, report)
		})
	}
}

func credentialOperationTarget(w http.ResponseWriter, r *http.Request, requireCredential bool) (int64, string, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "channelID"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "invalid channel ID")
		return 0, "", false
	}
	cid := chi.URLParam(r, "credentialID")
	if r.URL.RawPath != "" {
		cid, err = url.PathUnescape(cid)
	}
	if err != nil || requireCredential && cid == "" {
		fail(w, 400, "invalid credential ID")
		return 0, "", false
	}
	return id, cid, true
}

func credentialOperationError(w http.ResponseWriter, err error) {
	if pluginError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "渠道或凭证不存在，请刷新后重试")
	case errors.Is(err, store.ErrCredentialIdentityChanged):
		fail(w, 409, "凭证或配置已变化，请刷新后重试")
	case errors.Is(err, store.ErrCredentialActionBusy):
		fail(w, 409, "此凭证还有操作进行中或结果待核对")
	case errors.Is(err, store.ErrInvalidCredentialOperation):
		fail(w, 400, err.Error())
	default:
		fail(w, 500, "无法保存凭证操作，请刷新后重试")
	}
}
