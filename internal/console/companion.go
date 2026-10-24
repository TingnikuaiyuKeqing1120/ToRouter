package console

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"torouter/internal/companion"
	"torouter/internal/store"
)

func companionRoutes(router chi.Router, s *store.Store, hooks Hooks) {
	router.Post("/desktop/open-channel", func(w http.ResponseWriter, r *http.Request) {
		var target store.QuotaTarget
		if !decode(w, r, &target) {
			return
		}
		if _, _, ok := readStatusTarget(w, s, statusTarget{ChannelID: target.ChannelID, CredentialID: target.CredentialID}); !ok {
			return
		}
		if hooks.OpenChannelConsole == nil {
			fail(w, 503, "此运行环境无法打开渠道")
			return
		}
		if err := hooks.OpenChannelConsole(target.ChannelID, target.CredentialID); err != nil {
			fail(w, 503, "无法打开渠道")
			return
		}
		respond(w, 200, map[string]bool{"requested": true})
	})
	router.Get("/desktop/pet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if hooks.CompanionRunning == nil {
			fail(w, 503, "此运行环境无法读取桌宠状态")
			return
		}
		respond(w, 200, map[string]bool{"running": hooks.CompanionRunning()})
	})
	router.Get("/companion/preferences", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.CompanionPreferences()
		if err != nil {
			fail(w, 500, "无法读取伴侣偏好")
			return
		}
		respond(w, 200, v)
	})
	router.Put("/companion/preferences", func(w http.ResponseWriter, r *http.Request) {
		var patch store.CompanionPreferencesPatch
		if !decode(w, r, &patch) {
			return
		}
		if patch.Theme != nil && *patch.Theme != "light" && *patch.Theme != "dark" {
			fail(w, 400, "主题必须为 light 或 dark")
			return
		}
		if err := validateQuotaTargets(s, patch.QuotaTargets); err != nil {
			fail(w, 400, err.Error())
			return
		}
		v, err := s.PatchCompanionPreferences(patch)
		if err != nil {
			fail(w, 500, "无法保存伴侣偏好")
			return
		}
		respond(w, 200, v)
	})
	var mu sync.Mutex
	var cached store.UsageTotals
	var cachedAt, cachedDay time.Time
	router.Get("/companion", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if hooks.Companion == nil {
			fail(w, 503, "桌面伴侣状态暂不可用")
			return
		}
		snapshot := hooks.Companion()
		now := snapshot.ServerTime
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		mu.Lock()
		if !cachedDay.Equal(day) || now.Sub(cachedAt) >= 2*time.Second {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			totals, err := s.UsageTotalsBetween(ctx, day, day.AddDate(0, 0, 1))
			cancel()
			if err != nil {
				mu.Unlock()
				fail(w, 500, "暂时无法读取今日用量")
				return
			}
			cached, cachedAt, cachedDay = totals, now, day
		}
		today := cached
		mu.Unlock()
		respond(w, 200, struct {
			companion.Snapshot
			Today store.UsageTotals `json:"today"`
		}{snapshot, today})
	})
	handleOpen := func(w http.ResponseWriter, r *http.Request) {
		if hooks.OpenCompanion == nil {
			fail(w, 503, "此运行环境无法打开独立窗口")
			return
		}
		if err := hooks.OpenCompanion(); err != nil {
			fail(w, 503, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"requested": true})
	}
	router.Post("/companion/open", handleOpen)
	router.Post("/desktop/launch-pet", handleOpen)
	router.Post("/desktop/launch-gui", func(w http.ResponseWriter, r *http.Request) {
		if hooks.OpenConsole == nil {
			fail(w, 503, "此运行环境无法打开桌面控制台")
			return
		}
		if err := hooks.OpenConsole(); err != nil {
			fail(w, 503, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"requested": true})
	})
	router.Post("/desktop/close-pet", func(w http.ResponseWriter, r *http.Request) {
		if hooks.CloseCompanion == nil {
			fail(w, 503, "此运行环境无法关闭桌宠")
			return
		}
		if err := hooks.CloseCompanion(); err != nil {
			fail(w, 503, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"requested": true})
	})
}
