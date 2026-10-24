package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"torouter/internal/store"
)

type OperationReceipt struct {
	RequestID string    `json:"request_id"`
	State     string    `json:"state"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at"`
}

type OperationReport struct {
	ChannelGeneration int64                            `json:"channel_generation"`
	Supported         bool                             `json:"supported"`
	Identity          string                           `json:"identity"`
	Config            store.CredentialOperationsConfig `json:"config"`
	ResetCards        *ResetCredits                    `json:"reset_cards,omitempty"`
	ResetCardsError   string                           `json:"reset_cards_error,omitempty"`
	LastWarmup        *OperationReceipt                `json:"last_warmup,omitempty"`
	LastReset         *OperationReceipt                `json:"last_reset,omitempty"`
	TimeZone          string                           `json:"time_zone"`
}

type operationEntry struct {
	OperationReceipt
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
}

// Configuration remains on the credential. This record only contains local
// evidence, completed request IDs and durable scheduling claims.
type operationState struct {
	Cards            *ResetCredits              `json:"cards,omitempty"`
	CardsError       string                     `json:"cards_error,omitempty"`
	History          map[string]operationEntry  `json:"history,omitempty"`
	LastWarmupID     string                     `json:"last_warmup_id,omitempty"`
	LastResetID      string                     `json:"last_reset_id,omitempty"`
	DailyDate        string                     `json:"daily_date,omitempty"`
	DailyTimes       []string                   `json:"daily_times,omitempty"`
	ResetLatched     bool                       `json:"reset_latched,omitempty"`
	LastNaturalReset int64                      `json:"last_natural_reset,omitempty"`
	LastPoll         time.Time                  `json:"last_poll,omitempty"`
	ScheduleRevision credentialScheduleRevision `json:"schedule_revision"`
}

type credentialScheduleRevision struct {
	Channel    int64 `json:"channel"`
	Credential int64 `json:"credential"`
}

func scheduleRevision(c store.Channel, cid string) credentialScheduleRevision {
	r := credentialScheduleRevision{Channel: c.OperationsRevision}
	for _, cred := range c.Credentials {
		if cred.ID == cid {
			r.Credential = cred.OperationsRevision
			break
		}
	}
	return r
}

type Operations struct {
	store        *store.Store
	svc          *Service
	owner        string
	now          func() time.Time
	tickInterval time.Duration
}

func NewOperations(svc *Service) *Operations {
	return NewLocalOperations(svc.Store).WithService(svc)
}

// NewLocalOperations supports persisted reports and configuration without
// constructing an OAuth service or enabling any network operation.
func NewLocalOperations(st *store.Store) *Operations {
	return &Operations{store: st, owner: uuid.NewString(), now: time.Now, tickInterval: 30 * time.Second}
}

// WithService returns an immutable view sharing the local operation owner.
// Reports from either view therefore recognize the same in-process receipts.
func (ops *Operations) WithService(svc *Service) *Operations {
	bound := *ops
	bound.svc = svc
	return &bound
}

var errOperationsUnavailable = errors.New("Codex 网络操作不可用：服务尚未加载")

func decodeOperationState(raw json.RawMessage) (operationState, error) {
	s := operationState{History: map[string]operationEntry{}}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return s, errors.New("cannot read credential operation state")
		}
	}
	if s.History == nil {
		s.History = map[string]operationEntry{}
	}
	return s, nil
}

func operationConfig(c store.Channel, cid string) store.CredentialOperationsConfig {
	for _, cred := range c.Credentials {
		if cred.ID == cid && cred.Operations != nil {
			return *cred.Operations
		}
	}
	return store.DefaultCredentialOperationsConfig()
}

func operationEnabled(c store.Channel, cid string) bool {
	if !c.Enabled {
		return false
	}
	for _, cred := range c.Credentials {
		if cred.ID == cid {
			return cred.Enabled
		}
	}
	return false
}

func (ops *Operations) target(id int64, cid, identity string, write bool) (store.Channel, error) {
	c, err := ops.store.Channel(id)
	if err != nil {
		return c, err
	}
	current, err := store.CredentialOperationsIdentity(c, cid)
	if err != nil {
		return c, err
	}
	if identity != "" && current != identity {
		return c, store.ErrCredentialIdentityChanged
	}
	if write && identity == "" {
		return c, store.ErrCredentialIdentityChanged
	}
	if write && !operationEnabled(c, cid) {
		return c, fmt.Errorf("%w: 渠道或凭证已停用", store.ErrInvalidCredentialOperation)
	}
	return c, nil
}

func (ops *Operations) update(c store.Channel, cid string, update func(*operationState) error) (operationState, error) {
	return ops.mutate(c, cid, false, update)
}

func (ops *Operations) mutate(c store.Channel, cid string, claim bool, update func(*operationState) error) (operationState, error) {
	write := ops.store.UpdateCredentialOperationState
	if claim {
		write = ops.store.ClaimCredentialOperationState
	}
	raw, err := write(c, cid, func(raw json.RawMessage) (json.RawMessage, error) {
		s, err := decodeOperationState(raw)
		if err != nil {
			return nil, err
		}
		if err := update(&s); err != nil {
			return nil, err
		}
		return json.Marshal(s)
	})
	if err != nil {
		return operationState{}, err
	}
	return decodeOperationState(raw)
}

func (ops *Operations) receipt(s operationState, id string) *OperationReceipt {
	entry, ok := s.History[id]
	if !ok {
		return nil
	}
	r := entry.OperationReceipt
	if r.State == "running" && entry.Owner != ops.owner {
		r.State, r.Detail = "unknown", "上次进程未记录最终结果，请先核对再解除阻塞"
	}
	return &r
}

func (ops *Operations) report(c store.Channel, cid string, s operationState) (OperationReport, error) {
	identity, err := store.CredentialOperationsIdentity(c, cid)
	if err != nil {
		return OperationReport{}, err
	}
	zone := ops.now().Location().String()
	return OperationReport{ChannelGeneration: c.Generation, Supported: true, Identity: identity, Config: operationConfig(c, cid), ResetCards: s.Cards, ResetCardsError: s.CardsError, LastWarmup: ops.receipt(s, s.LastWarmupID), LastReset: ops.receipt(s, s.LastResetID), TimeZone: zone}, nil
}

// Report is strictly local and never starts a quota/card request.
func (ops *Operations) Report(id int64, cid string) (OperationReport, error) {
	c, err := ops.target(id, cid, "", false)
	if err != nil {
		return OperationReport{}, err
	}
	raw, err := ops.store.LoadCredentialOperationState(id, cid)
	if err != nil {
		return OperationReport{}, err
	}
	s, err := decodeOperationState(raw)
	if err != nil {
		return OperationReport{}, err
	}
	return ops.report(c, cid, s)
}

func (ops *Operations) PatchConfig(id int64, cid, identity string, patch store.CredentialOperationsPatch) (OperationReport, error) {
	if patch.Warmup != nil && patch.Warmup.Enabled {
		if _, err := ops.target(id, cid, identity, false); err != nil {
			return OperationReport{}, err
		}
		usage, err := CachedUsageFromStore(ops.store, id, cid)
		if err != nil || !usage.OK || usage.Usage == nil || generalWindows(*usage.Usage)[18000] == nil {
			return OperationReport{}, fmt.Errorf("%w: 未确认5小时通用窗口，请先刷新额度；此自动功能仅适用于有5小时窗口的凭证", store.ErrInvalidCredentialOperation)
		}
	}
	if err := ops.store.PatchCredentialOperationsConfig(id, cid, identity, patch); err != nil {
		return OperationReport{}, err
	}
	return ops.Report(id, cid)
}

func (ops *Operations) RefreshCards(ctx context.Context, id int64, cid, identity string) (OperationReport, error) {
	if ops.svc == nil {
		return OperationReport{}, errOperationsUnavailable
	}
	c, err := ops.target(id, cid, identity, false)
	if err != nil {
		return OperationReport{}, err
	}
	if identity == "" {
		return OperationReport{}, store.ErrCredentialIdentityChanged
	}
	cards, readErr := ops.svc.ResetCreditsForCredential(ctx, c, cid)
	_, err = ops.update(c, cid, func(s *operationState) error {
		if readErr != nil {
			s.CardsError = "重置卡读取失败，请稍后手动刷新"
		} else {
			s.Cards, s.CardsError = &cards, ""
		}
		return nil
	})
	if err != nil {
		return OperationReport{}, err
	}
	return ops.Report(id, cid)
}

func (ops *Operations) Warmup(ctx context.Context, id int64, cid, identity, requestID string) (OperationReport, error) {
	return ops.execute(ctx, id, cid, identity, requestID, "warmup", nil)
}

func (ops *Operations) ConsumeReset(ctx context.Context, id int64, cid, identity, requestID string) (OperationReport, error) {
	return ops.execute(ctx, id, cid, identity, requestID, "reset", nil)
}

// scheduleClaim runs in the same persistent transaction as the action receipt.
type scheduleClaim struct {
	config   store.CredentialOperationsConfig
	revision credentialScheduleRevision
	apply    func(*operationState) error
}

func (ops *Operations) execute(ctx context.Context, id int64, cid, identity, requestID, kind string, scheduled *scheduleClaim) (OperationReport, error) {
	if ops.svc == nil {
		return OperationReport{}, errOperationsUnavailable
	}
	if !validRedeemRequestID(requestID) {
		return OperationReport{}, fmt.Errorf("%w: 操作请求ID必须是UUIDv4", store.ErrInvalidCredentialOperation)
	}
	requestID = uuid.MustParse(requestID).String()
	c, err := ops.target(id, cid, identity, false)
	if err != nil {
		return OperationReport{}, err
	}
	if identity == "" {
		return OperationReport{}, store.ErrCredentialIdentityChanged
	}
	raw, err := ops.store.LoadCredentialOperationState(id, cid)
	if err != nil {
		return OperationReport{}, err
	}
	prior, err := decodeOperationState(raw)
	if err != nil {
		return OperationReport{}, err
	}
	if old, ok := prior.History[requestID]; ok {
		if old.Kind != kind {
			return OperationReport{}, fmt.Errorf("%w: 请求ID已被另一类操作使用", store.ErrInvalidCredentialOperation)
		}
		report, err := ops.report(c, cid, prior)
		if kind == "warmup" {
			report.LastWarmup = ops.receipt(prior, requestID)
		} else {
			report.LastReset = ops.receipt(prior, requestID)
		}
		return report, err
	}
	if !operationEnabled(c, cid) {
		return OperationReport{}, fmt.Errorf("%w: 渠道或凭证已停用", store.ErrInvalidCredentialOperation)
	}
	cfg := operationConfig(c, cid)
	if scheduled != nil && (scheduleRevision(c, cid) != scheduled.revision || !sameOperationConfig(cfg, scheduled.config)) {
		return OperationReport{}, store.ErrCredentialIdentityChanged
	}
	claimed := false
	var original *OperationReceipt
	_, err = ops.mutate(c, cid, true, func(s *operationState) error {
		if old, ok := s.History[requestID]; ok {
			if old.Kind != kind {
				return fmt.Errorf("%w: 请求ID已被另一类操作使用", store.ErrInvalidCredentialOperation)
			}
			original = ops.receipt(*s, requestID)
			return nil
		}
		for _, entry := range s.History {
			if entry.State == "running" || entry.State == "unknown" {
				return store.ErrCredentialActionBusy
			}
		}
		if scheduled != nil {
			if err := scheduled.apply(s); err != nil {
				return err
			}
		}
		entry := operationEntry{OperationReceipt: OperationReceipt{RequestID: requestID, State: "running", Detail: "请求处理中", At: ops.now().UTC()}, Kind: kind, Owner: ops.owner}
		s.History[requestID] = entry
		if kind == "warmup" {
			s.LastWarmupID = requestID
		} else {
			s.LastResetID = requestID
			s.ResetLatched = true
		}
		claimed = true
		return nil
	})
	if err != nil {
		return OperationReport{}, err
	}
	if !claimed {
		report, err := ops.Report(id, cid)
		if kind == "warmup" {
			report.LastWarmup = original
		} else {
			report.LastReset = original
		}
		return report, err
	}
	// A config edit can happen while the receipt is being persisted. Automatic
	// actions must still belong to the exact currently enabled configuration.
	live, actionErr := ops.target(id, cid, identity, true)
	if actionErr == nil && scheduled != nil && (scheduleRevision(live, cid) != scheduleRevision(c, cid) || !sameOperationConfig(cfg, operationConfig(live, cid))) {
		actionErr = actionError("自动配置已改变，本次操作已取消", false)
	}
	if actionErr == nil {
		var automatic *store.CredentialOperationsConfig
		if scheduled != nil {
			automatic = &cfg
		}
		if kind == "warmup" {
			actionErr = ops.svc.warmupCredential(ctx, live, cid, cfg.Warmup.Model, automatic)
		} else {
			actionErr = ops.svc.consumeResetCredit(ctx, live, cid, requestID, automatic)
		}
	}
	state, detail := "succeeded", "预热已完成"
	if kind == "reset" {
		detail = "上游已接受使用一张重置卡，由上游选卡"
	}
	if actionErr != nil {
		state, detail = "failed", "操作未完成，请检查当前凭证与配置"
		var uncertain *CredentialActionError
		if errors.As(actionErr, &uncertain) && uncertain.Unknown {
			state, detail = "unknown", "请求结果未知；未重发，请核对后解除阻塞"
		}
	}
	_, err = ops.update(c, cid, func(s *operationState) error {
		entry, ok := s.History[requestID]
		if !ok || entry.Owner != ops.owner || entry.State != "running" {
			return store.ErrCredentialActionBusy
		}
		entry.State, entry.Detail, entry.At = state, detail, ops.now().UTC()
		s.History[requestID] = entry
		return nil
	})
	if err != nil {
		return OperationReport{}, err
	}
	if kind == "reset" && actionErr == nil {
		// Reconciliation is a read, never a reason to replay the accepted POST.
		usage, usageErr := ops.svc.RefreshUsage(ctx, id, cid, true)
		cardsReport, cardsErr := ops.RefreshCards(ctx, id, cid, identity)
		if usageErr != nil || !usage.OK || usage.Detail != "" || cardsErr != nil || cardsReport.ResetCardsError != "" {
			_, _ = ops.update(c, cid, func(s *operationState) error {
				entry := s.History[requestID]
				entry.Detail = detail + "；刷新额度或重置卡失败，请手动核对"
				s.History[requestID] = entry
				return nil
			})
		}
	}
	return ops.Report(id, cid)
}

func sameOperationConfig(a, b store.CredentialOperationsConfig) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (ops *Operations) Acknowledge(id int64, cid, identity, kind string) (OperationReport, error) {
	if kind != "warmup" && kind != "reset" {
		return OperationReport{}, fmt.Errorf("%w: 未知操作类型", store.ErrInvalidCredentialOperation)
	}
	c, err := ops.target(id, cid, identity, false)
	if err != nil {
		return OperationReport{}, err
	}
	if identity == "" {
		return OperationReport{}, store.ErrCredentialIdentityChanged
	}
	_, err = ops.update(c, cid, func(s *operationState) error {
		requestID := s.LastResetID
		if kind == "warmup" {
			requestID = s.LastWarmupID
		}
		r := ops.receipt(*s, requestID)
		if r == nil || r.State != "unknown" {
			return fmt.Errorf("%w: 只有结果未知的操作可以确认", store.ErrInvalidCredentialOperation)
		}
		entry := s.History[requestID]
		entry.State, entry.Detail, entry.At = "acknowledged", "已人工核对，解除结果未知阻塞", ops.now().UTC()
		s.History[requestID] = entry
		return nil
	})
	if err != nil {
		return OperationReport{}, err
	}
	return ops.Report(id, cid)
}
