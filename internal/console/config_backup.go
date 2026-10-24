package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"torouter/internal/configbackup"
	"torouter/internal/management"
	"torouter/internal/plugins"
	"torouter/internal/store"
)

type configExportInput struct {
	Password string `json:"password"`
}

type configPreviewInput struct {
	File     string `json:"file"`
	Password string `json:"password"`
}

type configRestoreInput struct {
	File     string `json:"file"`
	Password string `json:"password"`
	Confirm  bool   `json:"confirm"`
}

func decodeConfigBackup(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, configbackup.MaxFileBytes))
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func configBackupError(err error) error {
	switch {
	case errors.Is(err, configbackup.ErrPassword):
		return invalid("请输入12至256个字符的备份口令；不会保存口令。")
	case errors.Is(err, configbackup.ErrFile):
		return invalid("备份文件或口令无效；请核对文件与口令。")
	case errors.Is(err, configbackup.ErrLimit):
		return invalid("备份文件超出大小限制（最大 12MB）。")
	case errors.Is(err, configbackup.ErrVersion):
		return invalid("不支持的备份文件版本。")
	}
	return err
}

func configBackupRoutes(r chi.Router, s *store.Store, hooks Hooks, manager *plugins.Manager) {
	reply := func(w http.ResponseWriter, out any, err error) {
		w.Header().Set("Cache-Control", "no-store")
		if err == nil {
			respond(w, http.StatusOK, out)
			return
		}
		var safe *management.Error
		if errors.As(configBackupError(err), &safe) {
			fail(w, 400, safe.Message)
			return
		}
		fail(w, 400, err.Error())
	}

	r.Post("/config-backup/export", func(w http.ResponseWriter, r *http.Request) {
		var in configExportInput
		if !decodeConfigBackup(w, r, &in) {
			return
		}
		exported, err := s.ExportFullConfig()
		if err != nil {
			reply(w, nil, err)
			return
		}
		encrypted, err := configbackup.Encrypt(r.Context(), exported, in.Password)
		if err != nil {
			reply(w, nil, err)
			return
		}
		filename := fmt.Sprintf("torouter-backup-%s.json", time.Now().Format("20060102-150405"))
		reply(w, map[string]any{
			"file":     encrypted,
			"filename": filename,
		}, nil)
	})

	r.Post("/config-backup/preview", func(w http.ResponseWriter, r *http.Request) {
		var in configPreviewInput
		if !decodeConfigBackup(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.File) == "" {
			fail(w, 400, "备份文件内容不能为空")
			return
		}
		payload, err := configbackup.Decrypt(r.Context(), []byte(in.File), in.Password)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if err := payload.Validate(); err != nil {
			fail(w, 400, "备份内容校验失败: "+err.Error())
			return
		}
		reply(w, map[string]any{
			"summary": payload.Summary(),
		}, nil)
	})

	r.Post("/config-backup/restore", func(w http.ResponseWriter, r *http.Request) {
		var in configRestoreInput
		if !decodeConfigBackup(w, r, &in) {
			return
		}
		if !in.Confirm {
			fail(w, 400, "必须显式确认恢复操作")
			return
		}
		if strings.TrimSpace(in.File) == "" {
			fail(w, 400, "备份文件内容不能为空")
			return
		}
		payload, err := configbackup.Decrypt(r.Context(), []byte(in.File), in.Password)
		if err != nil {
			reply(w, nil, err)
			return
		}
		if err := payload.Validate(); err != nil {
			fail(w, 400, "备份内容校验失败: "+err.Error())
			return
		}
		if err := s.RestoreFullConfig(payload); err != nil {
			reply(w, nil, err)
			return
		}
		if manager != nil {
			if err := manager.Reload(); err != nil {
				fail(w, 500, "插件重载失败: "+err.Error())
				return
			}
		}
		reply(w, map[string]any{
			"ok":               true,
			"restart_required": true,
			"snapshot":         s.LastPreRestoreSnapshot(),
		}, nil)
	})

	r.Get("/config-backup/rollback", func(w http.ResponseWriter, r *http.Request) {
		target := s.LastPreRestoreSnapshot()
		reply(w, map[string]any{
			"available": target != "",
			"snapshot":  target,
		}, nil)
	})

	r.Post("/config-backup/rollback", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Snapshot string `json:"snapshot"`
			Confirm  bool   `json:"confirm"`
		}
		if !decodeConfigBackup(w, r, &in) {
			return
		}
		if !in.Confirm {
			fail(w, 400, "必须显式确认回退操作")
			return
		}
		target := strings.TrimSpace(in.Snapshot)
		if target == "" {
			target = s.LastPreRestoreSnapshot()
		}
		if target == "" {
			fail(w, 400, "未找到可用的回退快照")
			return
		}
		if err := s.RestoreSnapshot(target); err != nil {
			fail(w, 500, "快照回退失败: "+err.Error())
			return
		}
		if manager != nil {
			if err := manager.Reload(); err != nil {
				fail(w, 500, "插件重载失败: "+err.Error())
				return
			}
		}
		reply(w, map[string]any{
			"ok":                true,
			"restart_required":  true,
			"restored_snapshot": target,
		}, nil)
	})
}
