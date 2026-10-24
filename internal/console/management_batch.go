package console

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"torouter/internal/management"
	"torouter/internal/store"
)

type batchCredentialTarget struct {
	credentialRef
	ExpectedIdentity string `json:"expected_identity,omitempty"`
}
type batchCredentialInput struct {
	command
	Targets         []batchCredentialTarget `json:"targets"`
	Atomic          bool                    `json:"atomic,omitempty"`
	ContinueOnError bool                    `json:"continue_on_error,omitempty"`
}
type batchAutomationInput struct {
	batchCredentialInput
	Patch store.CredentialOperationsPatch `json:"patch"`
}

// These are typed bulk views of existing domain operations, not an arbitrary
// dispatcher. Provider calls cannot be rolled back, so partial results are explicit.
func (t *managementTools) batchOperations() []management.Operation {
	ops := []management.Operation{}
	for _, pair := range [][2]string{{"batch_refresh_credential_quota", "refresh_credential_quota"}, {"batch_warmup_credentials", "warmup_credential"}} {
		name, target := pair[0], pair[1]
		op := typedOperation[batchCredentialInput](name, "credential.batch."+target, management.Manage, "对明确凭证逐项执行；不支持atomic，默认遇错停止，结果保留成功/失败/未知/未发送项。预热会消耗额度。", false, func(ctx context.Context, p management.Principal, in batchCredentialInput) (any, error) {
			return t.batchCredentials(ctx, p, target, in, nil)
		})
		op.External = true
		ops = append(ops, op)
	}
	ops = append(ops, typedOperation[batchAutomationInput]("batch_configure_credential_automation", "credential.batch.automation", management.Manage, "对明确凭证逐项保存同一自动配置分支，保留其他分支；不支持atomic。", false, func(ctx context.Context, p management.Principal, in batchAutomationInput) (any, error) {
		return t.batchCredentials(ctx, p, "configure_credential_automation", in.batchCredentialInput, &in.Patch)
	}))
	return ops
}

func (t *managementTools) batchCredentials(ctx context.Context, p management.Principal, operation string, in batchCredentialInput, patch *store.CredentialOperationsPatch) (any, error) {
	if in.Atomic {
		return nil, management.NewError("unsupported", "这类操作逐项执行，不能原子回滚；请明确atomic=false。")
	}
	if len(in.Targets) < 1 || len(in.Targets) > 100 {
		return nil, invalid("targets须为1..100个明确凭证")
	}
	seen := map[credentialRef]bool{}
	for _, target := range in.Targets {
		if target.ChannelID <= 0 || target.CredentialID == "" || seen[target.credentialRef] {
			return nil, invalid("凭证目标无效或重复")
		}
		seen[target.credentialRef] = true
		if operation != "refresh_credential_quota" && target.ExpectedIdentity == "" {
			return nil, invalid("请先读取各凭证expected_identity")
		}
	}
	var handler func(context.Context, management.Principal, json.RawMessage) (any, error)
	for _, op := range t.registry {
		if op.Name == operation {
			handler = op.Handler
		}
	}
	if handler == nil {
		return nil, management.NewError("unsupported", "目标操作不可用")
	}
	items := []map[string]any{}
	completed, unknown, failed := 0, 0, 0
	stopped := false
	for i, target := range in.Targets {
		entry := map[string]any{"target": target.credentialRef}
		if stopped {
			entry["status"] = "not_sent"
			items = append(items, entry)
			continue
		}
		id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%d/%s/%s/%d", p.ID, operation, in.RequestID, i))).String()
		args := map[string]any{"request_id": id, "channel_id": target.ChannelID, "credential_id": target.CredentialID}
		if operation != "refresh_credential_quota" {
			args["expected_identity"] = target.ExpectedIdentity
		}
		if patch != nil {
			args["patch"] = patch
		}
		raw, _ := json.Marshal(args)
		value, err := handler(ctx, p, raw)
		status := "completed"
		if err != nil {
			status = "unknown"
			if _, ok := err.(*management.Error); ok {
				status = "failed"
			}
			entry["error"] = "操作未完成，请查询该凭证状态和操作回执。"
		} else {
			entry["result"] = value
			if domain, ok := asObject(value)["status"].(string); ok && (domain == "failed" || domain == "unknown" || domain == "partial") {
				status = domain
			}
		}
		entry["request_id"] = id
		entry["status"] = status
		switch status {
		case "completed":
			completed++
		case "unknown":
			unknown++
		default:
			failed++
		}
		if status != "completed" && !in.ContinueOnError {
			stopped = true
		}
		items = append(items, entry)
	}
	status := "completed"
	if completed < len(in.Targets) {
		if completed > 0 {
			status = "partial"
		} else if unknown > 0 {
			status = "unknown"
		} else {
			status = "failed"
		}
	}
	return map[string]any{"status": status, "atomic": false, "items": items, "completed": completed, "failed": failed, "unknown": unknown}, nil
}
