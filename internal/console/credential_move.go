package console

import (
	"context"
	"errors"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
	"torouter/internal/management"
	"torouter/internal/store"
)

type credentialMoveInput struct {
	command
	ChannelID                int64    `json:"channel_id"`
	TargetChannelID          int64    `json:"target_channel_id"`
	CredentialIDs            []string `json:"credential_ids"`
	ExpectedGeneration       int64    `json:"expected_generation"`
	ExpectedTargetGeneration int64    `json:"expected_target_generation"`
}

func credentialMoveRoutes(r chi.Router, s *store.Store) {
	r.Post("/channels/{id}/credentials/move", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		var in credentialMoveInput
		if !decode(w, r, &in) {
			return
		}
		source, target, err := s.MoveCredentials(id, in.TargetChannelID, in.CredentialIDs, in.ExpectedGeneration, in.ExpectedTargetGeneration, nil)
		if err != nil {
			if errors.Is(err, store.ErrEditConflict) {
				fail(w, 409, "渠道成员已变化，请刷新后重试")
			} else if errors.Is(err, store.ErrNotFound) {
				fail(w, 404, "渠道或成员不存在")
			} else if errors.Is(err, store.ErrInvalidCredentialOperation) {
				fail(w, 400, err.Error())
			} else {
				fail(w, 500, "移动失败，请刷新后重试")
			}
			return
		}
		respond(w, 200, map[string]any{"source": publicChannel(source), "target": publicChannel(target)})
	})
}
func (t *managementTools) credentialMoveOperations() []management.Operation {
	return []management.Operation{typedOperation[credentialMoveInput]("move_credentials", "credential.move", management.Manage, "原子移动同对象同地区渠道的完整凭证，保留ID和设置；要求两个渠道的当前generation。", false, func(ctx context.Context, p management.Principal, in credentialMoveInput) (any, error) {
		source, target, err := t.store.MoveCredentials(in.ChannelID, in.TargetChannelID, in.CredentialIDs, in.ExpectedGeneration, in.ExpectedTargetGeneration, func() error { return p.Require(ctx, management.Manage) })
		if err != nil {
			return nil, err
		}
		return map[string]any{"source": safeManagedChannel(source), "target": safeManagedChannel(target)}, nil
	})}
}
