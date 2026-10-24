package console

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/accessguard"
	"torouter/internal/keydiscovery"
	"torouter/internal/management"
	"torouter/internal/store"
)

func discoveryError(err error) error {
	switch {
	case errors.Is(err, keydiscovery.ErrConflict):
		return management.NewError("conflict", "来源、插件状态或目标已变化，请重新扫描后核对。")
	case errors.Is(err, keydiscovery.ErrExpired):
		return management.NewError("not_found", "预览已取消、过期或不属于当前连接，请重新扫描。")
	case errors.Is(err, keydiscovery.ErrInvalid), errors.Is(err, store.ErrInvalidAPIKeyImport):
		return invalid("来源、选择或目标无效，请重新核对预览。")
	}
	return err
}

func keyDiscoveryRoutes(r chi.Router, service *keydiscovery.Service) {
	reply := func(w http.ResponseWriter, value any, err error) {
		if err == nil {
			respond(w, http.StatusOK, value)
			return
		}
		err = discoveryError(err)
		if pluginError(w, err) {
			return
		}
		var public *management.Error
		if errors.As(err, &public) {
			status := http.StatusBadRequest
			switch public.Code {
			case "forbidden":
				status = http.StatusForbidden
			case "not_found":
				status = http.StatusNotFound
			case "conflict":
				status = http.StatusConflict
			}
			fail(w, status, public.Message)
			return
		}
		// Source paths and database diagnostics can contain private material.
		fail(w, http.StatusInternalServerError, "操作未完成，请核对来源状态后重新扫描。")
	}
	r.Get("/key-discovery/sources", func(w http.ResponseWriter, r *http.Request) {
		sources, err := service.Sources()
		reply(w, map[string]any{"sources": sources}, err)
	})
	r.Post("/key-discovery/sessions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SourceIDs []string `json:"source_ids"`
			keydiscovery.BeginOptions
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := service.Begin(r.Context(), "console", in.SourceIDs, in.BeginOptions)
		reply(w, out, err)
	})
	r.Post("/key-discovery/sessions/{id}/scan", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SourceID string `json:"source_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := service.Scan(r.Context(), "console", chi.URLParam(r, "id"), in.SourceID)
		reply(w, out, err)
	})
	r.Get("/key-discovery/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := service.Get(r.Context(), "console", chi.URLParam(r, "id"))
		reply(w, out, err)
	})
	r.Delete("/key-discovery/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := service.Cancel("console", chi.URLParam(r, "id"))
		reply(w, map[string]any{"cancelled": true}, err)
	})
	r.Post("/key-discovery/sessions/{id}/copy", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CandidateIDs []string `json:"candidate_ids"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := service.Copy(r.Context(), "console", chi.URLParam(r, "id"), in.CandidateIDs)
		reply(w, out, err)
	})
	r.Post("/key-discovery/sessions/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Items []keydiscovery.Selection `json:"items"`
		}
		if !decode(w, r, &in) {
			return
		}
		out, err := service.Import(r.Context(), "console", chi.URLParam(r, "id"), in.Items)
		reply(w, out, err)
	})
}

type keyScanInput struct {
	command
	keydiscovery.BeginOptions
	SourceIDs []string `json:"source_ids"`
}
type keyPreviewInput struct {
	PreviewID string `json:"preview_id"`
}
type keyCancelInput struct {
	command
	PreviewID string `json:"preview_id"`
}
type keyCopyInput struct {
	PreviewID    string   `json:"preview_id"`
	CandidateIDs []string `json:"candidate_ids"`
}
type keyImportInput struct {
	command
	PreviewID string                   `json:"preview_id"`
	Items     []keydiscovery.Selection `json:"items"`
}

func sensitiveDiscovery(ctx context.Context, p management.Principal) (context.Context, error) {
	guarded := accessguard.With(ctx, func() error {
		if err := accessguard.Check(ctx); err != nil {
			return err
		}
		return p.Require(ctx, management.Sensitive)
	})
	if err := accessguard.Check(guarded); err != nil {
		return nil, err
	}
	return guarded, nil
}

func (t *managementTools) discoveryOperations() []management.Operation {
	service := t.services.discovery
	return []management.Operation{
		typedOperation[emptyInput]("list_api_key_sources", "key_discovery.sources", management.Read, "列出内置凭证发现来源与启用状态；不读取文件或返回私有路径。", true, func(context.Context, management.Principal, emptyInput) (any, error) {
			sources, err := service.Sources()
			return map[string]any{"sources": sources}, discoveryError(err)
		}),
		typedOperation[keyScanInput]("scan_api_keys", "key_discovery.scan", management.Manage, "只读扫描所选工具的默认配置，含支持的API Key和完整OAuth材料；另需sensitive。不续期、不执行源程序。返回十分钟脱敏预览。", false, func(ctx context.Context, p management.Principal, in keyScanInput) (any, error) {
			ctx, err := sensitiveDiscovery(ctx, p)
			if err != nil {
				return nil, err
			}
			out, err := service.Begin(ctx, actor(p), in.SourceIDs, in.BeginOptions)
			if err != nil {
				return nil, discoveryError(err)
			}
			for _, id := range in.SourceIDs {
				out, err = service.Scan(ctx, actor(p), out.ID, id)
				if err != nil {
					return nil, discoveryError(err)
				}
			}
			return out, nil
		}),
		typedOperation[keyPreviewInput]("get_api_key_preview", "key_discovery.preview", management.Manage, "读取当前连接自己的未过期脱敏预览；另需sensitive。来源停用或取消后需重新扫描。", true, func(ctx context.Context, p management.Principal, in keyPreviewInput) (any, error) {
			ctx, err := sensitiveDiscovery(ctx, p)
			if err != nil {
				return nil, err
			}
			out, err := service.Get(ctx, actor(p), in.PreviewID)
			return out, discoveryError(err)
		}),
		typedOperation[keyCancelInput]("cancel_api_key_preview", "key_discovery.cancel", management.Manage, "丢弃自己的Key预览；不删除已经导入的渠道或源文件。", false, func(ctx context.Context, p management.Principal, in keyCancelInput) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			err := service.Cancel(actor(p), in.PreviewID)
			return map[string]any{"cancelled": err == nil}, discoveryError(err)
		}),
		typedOperation[keyCopyInput]("copy_credentials", "key_discovery.copy", management.Manage, "显式复制当前连接已选凭证的完整材料、URL、协议与来源；另需sensitive。API Key返回api_key，OAuth返回可导入的原生文件内容。仅在用户明确要求复制后调用。", true, func(ctx context.Context, p management.Principal, in keyCopyInput) (any, error) {
			ctx, err := sensitiveDiscovery(ctx, p)
			if err != nil {
				return nil, err
			}
			out, err := service.Copy(ctx, actor(p), in.PreviewID, in.CandidateIDs)
			return out, discoveryError(err)
		}),
		typedOperation[keyImportInput]("import_api_keys", "key_discovery.import", management.Manage, "导入明确勾选的新凭证，另需sensitive。全局按URL及类型分组新建启用渠道；渠道内仅追加扫描会话固定目标，不接受更换目标。已有材料跳过，来源变化拒绝。", false, func(ctx context.Context, p management.Principal, in keyImportInput) (any, error) {
			ctx, err := sensitiveDiscovery(ctx, p)
			if err != nil {
				return nil, err
			}
			out, err := service.Import(ctx, actor(p), in.PreviewID, in.Items)
			return out, discoveryError(err)
		}),
	}
}
