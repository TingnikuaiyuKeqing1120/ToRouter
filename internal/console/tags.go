package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

type tagGroupWithTags struct {
	store.TagGroup
	Tags []store.Tag `json:"tags"`
}

func tagRoutes(r chi.Router, s *store.Store) {
	r.Put("/tag-groups/reorder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Order []string `json:"order"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Order == nil {
			fail(w, 400, "order is required")
			return
		}
		if err := s.ReorderTagGroups(body.Order); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"reordered": true})
	})
	r.Put("/tags/reorder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GroupID string   `json:"group_id"`
			Order   []string `json:"order"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.GroupID == "" || body.Order == nil {
			fail(w, 400, "group_id and order are required")
			return
		}
		if err := s.ReorderTags(body.GroupID, body.Order); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"reordered": true})
	})
	// GET /tag-groups 获取所有标签组及下属标签
	r.Get("/tag-groups", func(w http.ResponseWriter, r *http.Request) {
		groups, err := s.ListTagGroups()
		if err != nil {
			fail(w, 500, "cannot read tag groups")
			return
		}
		tags, err := s.ListTags()
		if err != nil {
			fail(w, 500, "cannot read tags")
			return
		}

		tagsByGroup := make(map[string][]store.Tag)
		for _, t := range tags {
			tagsByGroup[t.GroupID] = append(tagsByGroup[t.GroupID], t)
		}

		out := make([]tagGroupWithTags, 0, len(groups))
		for _, g := range groups {
			groupTags := tagsByGroup[g.ID]
			if groupTags == nil {
				groupTags = []store.Tag{}
			}
			out = append(out, tagGroupWithTags{
				TagGroup: g,
				Tags:     groupTags,
			})
		}

		respond(w, 200, out)
	})

	// POST /tag-groups 创建自定义标签组
	r.Post("/tag-groups", func(w http.ResponseWriter, r *http.Request) {
		var g store.TagGroup
		if !decode(w, r, &g) {
			return
		}
		g.Type = store.TagGroupTypeUser
		if err := s.SaveTagGroup(g); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 201, g)
	})

	// PUT /tag-groups/{id} 更新标签组信息
	r.Put("/tag-groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		var g store.TagGroup
		if !decode(w, r, &g) {
			return
		}
		g.ID = id
		if err := s.SaveTagGroup(g); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, g)
	})

	// DELETE /tag-groups/{id} 删除自定义标签组
	r.Delete("/tag-groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		if err := s.DeleteTagGroup(id); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"deleted": true})
	})

	// GET /tags 获取所有标签列表
	r.Get("/tags", func(w http.ResponseWriter, r *http.Request) {
		tags, err := s.ListTags()
		if err != nil {
			fail(w, 500, "cannot read tags")
			return
		}
		respond(w, 200, tags)
	})

	// POST /tags 创建新标签
	r.Post("/tags", func(w http.ResponseWriter, r *http.Request) {
		var t store.Tag
		if !decode(w, r, &t) {
			return
		}
		if err := s.SaveTag(t); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 201, t)
	})

	// PUT /tags/{id} 更新标签
	r.Put("/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		var t store.Tag
		if !decode(w, r, &t) {
			return
		}
		t.ID = id
		if err := s.SaveTag(t); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, t)
	})

	// GET /tags/references 检查标签引用情况
	r.Get("/tags/references", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			fail(w, 400, "id parameter required")
			return
		}
		refs, err := s.GetTagReferences(id)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, refs)
	})

	// DELETE /tags/{id} 删除标签（支持 ?force=true 级联解绑）
	r.Delete("/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := decodedPathID(w, r)
		if !ok {
			return
		}
		force := r.URL.Query().Get("force") == "true"

		err := s.DeleteTag(id, force)
		var inUse *store.TagInUseError
		if errors.As(err, &inUse) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "标签正在被使用", "needs_confirm": true, "references": inUse.References,
			})
			return
		}
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"deleted": true})
	})

	// POST /tags/reset-builtin 重置内置标签与组为默认预设
	r.Post("/tags/reset-builtin", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ResetBuiltinTags(); err != nil {
			fail(w, 500, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"reset": true})
	})
}
