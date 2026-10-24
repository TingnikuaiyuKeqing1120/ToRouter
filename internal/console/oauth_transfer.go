package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"
	"torouter/internal/management"
	"torouter/internal/oauthtransfer"
	"torouter/internal/store"
)

type oauthExportInput struct {
	Refs []store.OAuthTransferRef `json:"refs"`
}
type oauthPrepareInput struct {
	TargetChannelID int64    `json:"target_channel_id,omitempty"`
	File            string   `json:"file,omitempty"`
	Files           []string `json:"files,omitempty"`
	CredentialType  string   `json:"credential_type,omitempty"`
}
type oauthTransferPreviewInput struct {
	PreviewID string `json:"preview_id"`
}
type oauthTransferCancelInput struct {
	command
	PreviewID string `json:"preview_id"`
}
type oauthTransferImportInput struct {
	command
	PreviewID string                    `json:"preview_id"`
	Items     []oauthtransfer.Selection `json:"items"`
}

func transferError(err error) error {
	switch {
	case errors.Is(err, oauthtransfer.ErrFile):
		if item := regexp.MustCompile(`第 [0-9]+ 项`).FindString(err.Error()); item != "" {
			return invalid("凭证解析失败: " + item + "格式或材料不完整，请核对所选类型。")
		}
		return invalid("凭证文件无效，请核对所选类型及文件中各项的完整材料。")
	case errors.Is(err, oauthtransfer.ErrLimit):
		return invalid("单次最多100条凭证，每文件最多4MiB、总共8MiB；请减少选择或重新导出。")
	case errors.Is(err, oauthtransfer.ErrExpired):
		return management.NewError("not_found", "预览已过期、已取消或不属于当前连接，请重新选择文件预览。")
	case errors.Is(err, oauthtransfer.ErrConflict), errors.Is(err, store.ErrEditConflict):
		return management.NewError("conflict", "目标已变化，请刷新预览并重新选择；不会覆盖现有凭证。")
	case errors.Is(err, oauthtransfer.ErrInvalid), errors.Is(err, store.ErrInvalidOAuthTransfer):
		return invalid("凭证或选择无效，请核对类型、完整材料与导入目标。")
	}
	return err
}
func oauthTransferRoutes(r chi.Router, service *oauthtransfer.Service) {
	reply := func(w http.ResponseWriter, out any, err error) {
		w.Header().Set("Cache-Control", "no-store")
		if err == nil {
			respond(w, http.StatusOK, out)
			return
		}
		var safe *management.Error
		if errors.As(transferError(err), &safe) {
			status := 400
			switch safe.Code {
			case "forbidden":
				status = 403
			case "not_found":
				status = 404
			case "conflict":
				status = 409
			}
			fail(w, status, safe.Message)
			return
		}
		fail(w, 500, "凭证迁移未完成，请核对文件和当前目标后重试。")
	}
	r.Post("/oauth-transfer/export", func(w http.ResponseWriter, r *http.Request) {
		var in oauthExportInput
		if !decodeOAuthTransfer(w, r, &in) {
			return
		}
		out, err := service.Export(r.Context(), in.Refs)
		reply(w, out, err)
	})
	r.Post("/oauth-transfer/preview", func(w http.ResponseWriter, r *http.Request) {
		var in oauthPrepareInput
		if !decodeOAuthTransfer(w, r, &in) {
			return
		}
		out, err := prepareTransfer(r.Context(), service, "console", in)
		reply(w, out, err)
	})
	r.Get("/oauth-transfer/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := service.Get(r.Context(), "console", chi.URLParam(r, "id"))
		reply(w, out, err)
	})
	r.Delete("/oauth-transfer/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := service.Cancel("console", chi.URLParam(r, "id"))
		reply(w, map[string]bool{"cancelled": err == nil}, err)
	})
	r.Post("/oauth-transfer/sessions/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Items []oauthtransfer.Selection `json:"items"`
		}
		if !decodeOAuthTransfer(w, r, &in) {
			return
		}
		out, err := service.Import(r.Context(), "console", chi.URLParam(r, "id"), in.Items)
		reply(w, out, err)
	})
}

func (t *managementTools) transferOperations() []management.Operation {
	service := t.services.transfer
	return []management.Operation{
		// Sensitive reads and supplied materials never enter durable write receipts.
		typedOperation[oauthExportInput]("export_oauth_credentials", "oauth_transfer.export", management.Manage,
			"将明确选择的凭证导出为对应工具原生文件；另需sensitive。单凭证一个JSON，多个为包含独立原生文件的ZIP；无文件口令。最多100条，不含路由或自动规则。返回artifact只交付给用户保存，不打印材料。", true,
			func(ctx context.Context, p management.Principal, in oauthExportInput) (any, error) {
				ctx, err := sensitiveDiscovery(ctx, p)
				if err != nil {
					return nil, err
				}
				out, err := service.Export(ctx, in.Refs)
				if err != nil {
					return nil, transferError(err)
				}
				data := []byte(out.File)
				if out.Encoding == "base64" {
					data, err = base64.StdEncoding.DecodeString(out.File)
					if err != nil {
						return nil, transferError(oauthtransfer.ErrFile)
					}
				}
				artifact, err := t.server.PutArtifact(ctx, p, out.Filename, out.MIMEType, data)
				if err != nil {
					return nil, err
				}
				return map[string]any{"artifact": artifact, "count": out.Count}, nil
			}),
		typedOperation[oauthPrepareInput]("preview_oauth_import", "oauth_transfer.prepare", management.Manage,
			"离线预览用户提供的原生凭证JSON，另需sensitive。credential_type指定当前插件类型；file或files二选一，每文件4MiB、总8MiB、最多100条。只生成当前连接十分钟脱敏预览，已有材料跳过，不联网。", true,
			func(ctx context.Context, p management.Principal, in oauthPrepareInput) (any, error) {
				ctx, err := sensitiveDiscovery(ctx, p)
				if err != nil {
					return nil, err
				}
				out, err := prepareTransfer(ctx, service, actor(p), in)
				return out, transferError(err)
			}),
		typedOperation[oauthTransferPreviewInput]("get_oauth_import_preview", "oauth_transfer.preview", management.Manage,
			"读取本连接的未过期凭证导入预览；另需sensitive。核对最新目标与version，完整令牌和账号身份不返回。", true,
			func(ctx context.Context, p management.Principal, in oauthTransferPreviewInput) (any, error) {
				ctx, err := sensitiveDiscovery(ctx, p)
				if err != nil {
					return nil, err
				}
				out, err := service.Get(ctx, actor(p), in.PreviewID)
				return out, transferError(err)
			}),
		typedOperation[oauthTransferImportInput]("import_oauth_credentials", "oauth_transfer.import", management.Manage,
			"提交已预览中明确选中的凭证，另需sensitive；只接受candidate_id，不再传原文件。预览时未指定目标则新建，已指定目标仅追加会话绑定渠道；不能在确认时换目标。重复跳过不覆盖，按组返回回执。", false,
			func(ctx context.Context, p management.Principal, in oauthTransferImportInput) (any, error) {
				ctx, err := sensitiveDiscovery(ctx, p)
				if err != nil {
					return nil, err
				}
				out, err := service.Import(ctx, actor(p), in.PreviewID, in.Items)
				return out, transferError(err)
			}),
		typedOperation[oauthTransferCancelInput]("cancel_oauth_import_preview", "oauth_transfer.cancel", management.Manage,
			"取消当前连接的凭证预览并释放临时材料，不删除文件或已经导入的凭证。", false,
			func(ctx context.Context, p management.Principal, in oauthTransferCancelInput) (any, error) {
				if err := p.Require(ctx, management.Manage); err != nil {
					return nil, err
				}
				err := service.Cancel(actor(p), in.PreviewID)
				return map[string]bool{"cancelled": err == nil}, transferError(err)
			}),
	}
}

func prepareTransfer(ctx context.Context, s *oauthtransfer.Service, owner string, in oauthPrepareInput) (oauthtransfer.Preview, error) {
	if len(in.Files) > 0 {
		if in.File != "" {
			return oauthtransfer.Preview{}, oauthtransfer.ErrInvalid
		}
		return s.PrepareFiles(ctx, owner, in.Files, in.CredentialType, in.TargetChannelID)
	}
	return s.Prepare(ctx, owner, in.File, in.CredentialType, in.TargetChannelID)
}

func decodeOAuthTransfer(w http.ResponseWriter, r *http.Request, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*oauthtransfer.MaxTotalBytes+(1<<20)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil {
		fail(w, 400, "凭证请求格式无效或过大，请核对后重试。")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		fail(w, 400, "请求必须包含一个JSON对象。")
		return false
	}
	return true
}
