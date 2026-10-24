package console

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"torouter/internal/store"
)

func clientKeyRoutes(r chi.Router, s *store.Store) {
	r.Get("/client-keys", func(w http.ResponseWriter, r *http.Request) {
		keys, err := s.ListClientKeys()
		if err != nil {
			fail(w, 500, "cannot read client keys")
			return
		}
		respond(w, 200, keys)
	})
	r.Post("/client-keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string          `json:"name"`
			Scope json.RawMessage `json:"scope"`
		}
		if !decode(w, r, &body) {
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" || len(body.Name) > 100 {
			fail(w, 400, "密钥名称为空或过长，请填写较短的名称。")
			return
		}
		scope := store.DefaultClientKeyScope()
		if len(body.Scope) > 0 {
			var supplied *store.ClientKeyScope
			dec := json.NewDecoder(bytes.NewReader(body.Scope))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&supplied); err != nil || supplied == nil {
				fail(w, 400, "invalid scope")
				return
			}
			scope = *supplied
		}
		scope, err := s.ValidateClientKeyScope(scope)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		key, token, err := s.CreateClientKeyWithScope(body.Name, scope)
		if err != nil {
			fail(w, 500, "cannot create client key")
			return
		}
		respond(w, 201, map[string]any{"client_key": key, "key": token})
	})
	r.Put("/client-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "invalid client key id")
			return
		}
		var body struct {
			Name  string                `json:"name"`
			Scope *store.ClientKeyScope `json:"scope"`
		}
		if !decode(w, r, &body) {
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" || len(body.Name) > 100 {
			fail(w, 400, "密钥名称为空或过长，请填写较短的名称。")
			return
		}
		if body.Scope == nil {
			fail(w, 400, "请明确设置访问范围。")
			return
		}
		scope, err := s.ValidateClientKeyScope(*body.Scope)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		key, err := s.UpdateClientKey(id, body.Name, scope)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fail(w, 404, "client key not found")
			} else {
				fail(w, 500, "cannot update client key")
			}
			return
		}
		respond(w, 200, key)
	})
	r.Post("/client-keys/preview", clientScopePreview(s))
	r.Delete("/client-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "invalid client key id")
			return
		}
		if err := s.DeleteClientKey(id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fail(w, 404, "client key not found")
			} else {
				fail(w, 500, "cannot revoke client key")
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	r.Get("/client-keys/{id}/key", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "invalid client key id")
			return
		}
		token, err := s.RevealClientKey(id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				fail(w, 404, "client key not found or legacy key not copyable")
			} else {
				fail(w, 500, "cannot reveal client key")
			}
			return
		}
		respond(w, 200, map[string]string{"key": token})
	})
}
