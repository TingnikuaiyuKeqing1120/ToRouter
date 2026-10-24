package console

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/go-chi/chi/v5"
	"torouter/internal/buildinfo"
	"torouter/internal/management"
	"torouter/internal/store"
	"torouter/internal/updater"
)

type UpdateAccepted struct {
	Requested bool   `json:"requested"`
	Port      int    `json:"port"`
	Version   string `json:"version"`
}

type appUpdateInput struct {
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	OpenMode string `json:"open_mode,omitempty"`
}

func updateStatus(h Hooks) updater.Status {
	if h.UpdateStatus == nil {
		return updater.Status{CurrentVersion: buildinfo.Version, State: "disabled"}
	}
	return h.UpdateStatus()
}

func appUpdateRoutes(r chi.Router, h Hooks) {
	r.Get("/app-updates", func(w http.ResponseWriter, r *http.Request) { respond(w, http.StatusOK, updateStatus(h)) })
	r.Post("/app-updates/check", func(w http.ResponseWriter, r *http.Request) {
		if !emptyControlBody(w, r) {
			return
		}
		if h.CheckUpdate == nil {
			respond(w, http.StatusOK, updateStatus(h))
			return
		}
		status, err := h.CheckUpdate(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, updater.ErrorMessage(err))
			return
		}
		respond(w, http.StatusOK, status)
	})
	r.Post("/app-updates/install", func(w http.ResponseWriter, r *http.Request) {
		var in appUpdateInput
		if !decode(w, r, &in) {
			return
		}
		if in.Version == "" || !validUpdateHash(in.SHA256) || (in.OpenMode != "" && !store.ValidLaunchMode(in.OpenMode)) {
			fail(w, 400, "请选择有效更新版本和界面")
			return
		}
		if h.PrepareUpdate == nil {
			fail(w, 503, "当前版本未提供在线更新")
			return
		}
		out, activate, err := h.PrepareUpdate(r.Context(), in.Version, in.SHA256, in.OpenMode)
		if err != nil {
			fail(w, 409, updater.ErrorMessage(err))
			return
		}
		respond(w, http.StatusAccepted, out)
		if activate != nil {
			activate()
		}
	})
}

type installAppUpdateInput struct {
	maintenanceInput
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func validUpdateHash(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32
}

func (t *managementTools) appUpdateOperations() []management.Operation {
	return []management.Operation{
		typedOperation[emptyInput]("get_app_updates", "app_updates.read", management.Read, "读取当前程序版本、已缓存的更新与最近安装结果，不联网检查。", true, func(_ context.Context, _ management.Principal, _ emptyInput) (any, error) {
			return updateStatus(t.hooks), nil
		}),
		typedOperation[command]("check_app_update", "app_updates.check", management.Manage, "向当前发行版内置更新源检查新版本；不下载或安装，不接受任意地址。", false, func(ctx context.Context, p management.Principal, _ command) (any, error) {
			if err := p.Require(ctx, management.Manage); err != nil {
				return nil, err
			}
			if t.hooks.CheckUpdate == nil {
				return updateStatus(t.hooks), nil
			}
			out, err := t.hooks.CheckUpdate(ctx)
			if err != nil {
				return nil, management.NewError("update_check_failed", updater.ErrorMessage(err))
			}
			return out, nil
		}),
		typedOperation[installAppUpdateInput]("install_app_update", "app_updates.install", management.Maintain, "预览并安装明确的已签名程序版本；会断开管理连接，之后读取版本与安装结果，勿盲目重试。", false, func(ctx context.Context, p management.Principal, in installAppUpdateInput) (any, error) {
			status := updateStatus(t.hooks)
			if t.hooks.PrepareUpdate == nil || !status.Enabled {
				return nil, invalid("当前版本未提供在线更新")
			}
			if status.Available == nil || in.Version == "" || !validUpdateHash(in.SHA256) || status.Available.Version != in.Version || status.Available.SHA256 != in.SHA256 {
				return nil, invalid("请先检查并选择当前可用的更新版本")
			}
			ready, result, err := t.prepareMaintenance(ctx, p, "install_app_update", in.PreviewID, map[string]string{"version": in.Version, "sha256": in.SHA256}, status.Available, map[string]any{"version": in.Version, "notes": status.Available.Notes, "disconnects": true, "preserves_data": true})
			if err != nil || !ready {
				return result, err
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			out, activate, err := t.hooks.PrepareUpdate(ctx, in.Version, in.SHA256, "")
			if err != nil {
				return nil, management.NewError("update_failed", updater.ErrorMessage(err))
			}
			if err = p.Require(ctx, management.Maintain); err != nil {
				return nil, err
			}
			if activate != nil {
				err = management.AfterResponse(ctx, activate)
			}
			return out, err
		}),
	}
}
