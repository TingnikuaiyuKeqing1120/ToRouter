package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"torouter/internal/store"
)

func canonicalArguments(input json.RawMessage) ([]byte, string, error) {
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil || args == nil {
		return nil, "", NewError("invalid_arguments", "Expected an argument object.")
	}
	id, _ := args["request_id"].(string)
	if strings.TrimSpace(id) == "" || len(id) > 128 {
		return nil, "", NewError("invalid_arguments", "request_id must contain 1..128 bytes.")
	}
	raw, err := json.Marshal(args)
	return raw, id, err
}

func (s *Server) invoke(ctx context.Context, p Principal, op Operation, input json.RawMessage) (any, error) {
	p.audit = safeAuditTarget(op.Name, input)
	if err := p.Require(ctx, op.Permission); err != nil {
		return nil, err
	}
	if op.ReadOnly {
		return op.Handler(ctx, p, input)
	}
	raw, id, err := canonicalArguments(input)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	permissions := []string{op.Permission}
	if p.Has(Sensitive) && op.Permission != Sensitive {
		permissions = append(permissions, Sensitive)
	}
	receipt, claimed, err := s.store.BeginManagementReceipt(p.ID, id, op.Name, hex.EncodeToString(hash[:]), permissions)
	if errors.Is(err, store.ErrManagementConflict) {
		return nil, NewError("conflict", "request_id was already used for a different operation or arguments.")
	}
	if err != nil {
		return nil, err
	}
	if !claimed {
		if err = s.authorizeReceipt(ctx, p, receipt); err != nil {
			return nil, err
		}
		return receipt, nil
	}
	// Check a second time after the durable reservation, before domain effects.
	if err = p.Require(ctx, op.Permission); err != nil {
		return s.finish(ctx, p, id, "failed", map[string]any{"error": publicError(err)})
	}
	handlerCtx, after := newAfterResponseScope(ctx)
	completed := false
	defer func() { after.finish(p, receipt.Permissions, completed) }()
	out, callErr := op.Handler(handlerCtx, p, input)
	domainStatus := operationStatus(out)
	status := "completed"
	if callErr != nil {
		// Domain errors must explicitly identify known failures; arbitrary I/O
		// failures may have happened after the upstream accepted the request.
		status = "unknown"
		var safe *Error
		if errors.As(callErr, &safe) && safe.Code != "unknown" {
			status = "failed"
		}
		// An external step may already have returned a factual result before
		// authorization was revoked. Preserve it, with a safe error alongside,
		// before finish performs the final response authorization check.
		if out != nil {
			out = map[string]any{"result": out, "error": publicError(callErr)}
		} else {
			out = map[string]any{"error": publicError(callErr)}
		}
	}
	if domainStatus != "" {
		status = domainStatus
	}
	finished, err := s.finish(ctx, p, id, status, out)
	completed = err == nil && finished.Status == "completed"
	return finished, err
}

func operationStatus(out any) string {
	status := ""
	if result, ok := out.(OperationOutcome); ok {
		status = result.OperationStatus()
	}
	if status == "" {
		var domain struct {
			Status string `json:"status"`
		}
		if raw, err := json.Marshal(out); err == nil && json.Unmarshal(raw, &domain) == nil {
			status = domain.Status
		}
	}
	switch status {
	case "partial", "failed", "unknown":
		return status
	}
	return ""
}

func (s *Server) finish(ctx context.Context, p Principal, id, status string, result any) (store.ManagementReceipt, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return store.ManagementReceipt{}, err
	}
	if err = s.store.CompleteManagementReceipt(p.ID, id, status, raw); err != nil {
		return store.ManagementReceipt{}, NewError("unknown", "Effects may have completed but the receipt could not be saved. Inspect current state; do not retry with a new ID.")
	}
	r, err := s.store.ManagementReceipt(p.ID, id)
	if err != nil {
		return r, err
	}
	if err = s.authorizeReceipt(ctx, p, r); err != nil {
		return store.ManagementReceipt{}, err
	}
	return r, nil
}

func (s *Server) authorizeReceipt(ctx context.Context, p Principal, r store.ManagementReceipt) error {
	for _, permission := range r.Permissions {
		if err := p.Require(ctx, permission); err != nil {
			return err
		}
	}
	return nil
}

// Receipt reads only this authorization's own durable receipt, after a fresh
// check. A caller must not expose arbitrary connection IDs through MCP.
func (s *Server) Receipt(ctx context.Context, p Principal, id string) (store.ManagementReceipt, error) {
	if err := p.Require(ctx, Read); err != nil {
		return store.ManagementReceipt{}, err
	}
	r, err := s.store.ManagementReceipt(p.ID, id)
	if err != nil {
		return r, err
	}
	if err = s.authorizeReceipt(ctx, p, r); err != nil {
		return store.ManagementReceipt{}, err
	}
	return r, nil
}

func (s *Server) CreatePreview(ctx context.Context, p Principal, operation string, payload any) (string, error) {
	if err := p.Require(ctx, Maintain); err != nil {
		return "", err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return s.store.CreateManagementPreview(p.ID, operation, raw)
}

func (s *Server) ConsumePreview(ctx context.Context, p Principal, operation, id string) (json.RawMessage, error) {
	if err := p.Require(ctx, Maintain); err != nil {
		return nil, err
	}
	raw, err := s.store.ConsumeManagementPreview(p.ID, operation, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, NewError("conflict", "Preview is expired, already used, or belongs to another authorization or operation. Create a new preview.")
	}
	return raw, err
}
