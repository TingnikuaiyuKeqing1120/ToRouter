package console

import (
	"errors"
	"net/http"
	"torouter/internal/router"
	"torouter/internal/store"
)

type scopePreviewBinding struct {
	ChannelID   int64    `json:"channel_id"`
	ChannelName string   `json:"channel_name"`
	ModelID     string   `json:"model_id"`
	Allowed     bool     `json:"allowed"`
	Reasons     []string `json:"reasons"`
}
type scopePreviewModel struct {
	ModelID  string                `json:"model_id"`
	Allowed  bool                  `json:"allowed"`
	Reason   string                `json:"reason"`
	Bindings []scopePreviewBinding `json:"bindings"`
}

func clientScopePreview(s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope *store.ClientKeyScope `json:"scope"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Scope == nil {
			fail(w, 400, "explicit scope is required")
			return
		}
		result, err := evaluateClientScope(s, *body.Scope)
		respondManagementTool(w, result, err)
	}
}

func evaluateClientScope(s *store.Store, input store.ClientKeyScope) (any, error) {
	scope, err := s.ValidateClientKeyScope(input)
	if err != nil {
		var config *store.ConfigurationError
		if errors.As(err, &config) {
			return nil, &ManagementToolError{Status: 400, Message: config.Error()}
		}
		return nil, err
	}
	models, err := s.ListDownstreamModels()
	if err != nil {
		return nil, errors.New("cannot read models")
	}
	channels, err := s.ListChannels()
	if err != nil {
		return nil, errors.New("cannot read channels")
	}
	tags, err := s.ListTags()
	if err != nil {
		return nil, errors.New("cannot read tags")
	}
	groups, err := s.ListTagGroups()
	if err != nil {
		return nil, errors.New("cannot read tag groups")
	}
	byID := map[int64]store.Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	groupNames := map[string]string{}
	for _, group := range groups {
		groupNames[group.ID] = group.Name
	}
	rows := []scopePreviewModel{}
	allowedModels := 0
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		row := scopePreviewModel{ModelID: m.ID, Bindings: []scopePreviewBinding{}}
		modelAllowed := scope.AllowsModel(m.ID)
		for _, b := range m.Bindings {
			c, exists := byID[b.ChannelID]
			binding := scopePreviewBinding{ChannelID: b.ChannelID, ChannelName: c.Name, ModelID: b.Model, Reasons: []string{}}
			if !modelAllowed {
				binding.Reasons = append(binding.Reasons, "公开模型不在允许范围内")
			}
			if !exists {
				binding.Reasons = append(binding.Reasons, "渠道不存在")
			} else {
				if reason := router.BindingStructureReason(c, b); reason != "" {
					binding.Reasons = append(binding.Reasons, reason)
				}
				for _, group := range scope.MismatchedTagGroups(store.ResolveModelTags(c, b.Model, tags, groups)) {
					name := groupNames[group]
					if name == "" {
						name = group
					}
					binding.Reasons = append(binding.Reasons, name+"不匹配")
				}
			}
			binding.Allowed = len(binding.Reasons) == 0
			row.Allowed = row.Allowed || binding.Allowed
			row.Bindings = append(row.Bindings, binding)
		}
		if row.Allowed {
			row.Reason = "存在匹配绑定"
			allowedModels++
		} else if !modelAllowed {
			row.Reason = "公开模型不在允许范围内"
		} else {
			row.Reason = "没有匹配的合格绑定"
		}
		rows = append(rows, row)
	}
	return map[string]any{"models": rows, "allowed_models": allowedModels, "total_models": len(rows)}, nil
}
