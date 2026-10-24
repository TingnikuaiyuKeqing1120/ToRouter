package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"sync"
	"torouter/internal/accessguard"
	"torouter/internal/auth"
	"torouter/internal/codex"
	"torouter/internal/companion"
	"torouter/internal/copilot"
	"torouter/internal/gateway/convert"
	"torouter/internal/health"
	"torouter/internal/nativeoauth"
	"torouter/internal/pluginhost"
	"torouter/internal/plugins"
	"torouter/internal/pricing"
	"torouter/internal/quota"
	route "torouter/internal/router"
	"torouter/internal/siwc"
	"torouter/internal/store"
)

const maxBody = 16 << 20

func failureCooldown(settings store.Settings) time.Duration {
	if settings.FailureCooldownSeconds < 1 {
		return 10 * time.Second
	}
	return time.Duration(settings.FailureCooldownSeconds) * time.Second
}

type Gateway struct {
	Store   *store.Store
	Breaker *health.Breaker
	Client  *http.Client
	Live    *companion.Tracker
	Creds   *CredentialManager
	// Plugins shares one lazy credential lifecycle with console and workers.
	Plugins         *plugins.Manager
	pluginErr       error
	affinity        *affinity
	QuotaCheck      func(context.Context, store.Channel, store.CredentialItem) quota.Observation
	runtimeMu       sync.Mutex
	runtime         *runtimeState
	reasoningOnce   sync.Once
	reasoningBridge *convert.ReasoningBridge
}

func New(s *store.Store) *Gateway {
	m, err := plugins.New(s)
	g := NewWithPlugins(s, m)
	g.pluginErr = err
	return g
}

// NewWithOAuth injects the token services, which is how tests point them at a
// double. The console is constructed with the same instances (see
// internal/app): one instance per track is what gives every channel exactly one
// refresh lock no matter which side of the process refreshes it.
func NewWithOAuth(s *store.Store, copilotSvc *copilot.Service, siwcSvc *siwc.Service, codexSvc *codex.Service) *Gateway {
	m, err := plugins.NewInjected(s, copilotSvc, siwcSvc, codexSvc)
	g := NewWithPlugins(s, m)
	g.pluginErr = err
	return g
}

func NewWithPlugins(s *store.Store, manager *plugins.Manager) *Gateway {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 16
	g := &Gateway{Store: s, Breaker: health.New(), Creds: NewCredentialManager(), Client: &http.Client{Transport: transport, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Plugins: manager, affinity: newAffinity()}
	if manager == nil {
		g.pluginErr = errors.New("plugin registry unavailable")
	} else {
		manager.OnChange(func(credentialType string, _ bool) { g.invalidatePluginQuota(credentialType) })
	}
	return g
}

var defaultCredentialManager = NewCredentialManager()

func (g *Gateway) credentials() *CredentialManager {
	if g.Creds != nil {
		return g.Creds
	}
	return defaultCredentialManager
}

type requestInfo struct {
	protocol, model   string
	format            string // one of the store.Format* names
	stream, responses bool
	body              []byte
}

func parse(r *http.Request) (requestInfo, error) {
	in := requestInfo{}
	switch {
	case r.URL.Path == "/v1/chat/completions":
		in.protocol, in.format = "openai", store.FormatOpenAIChatCompletions
	case r.URL.Path == "/v1/responses":
		in.protocol, in.format, in.responses = "openai", store.FormatOpenAIResponses, true
	case r.URL.Path == "/v1/messages":
		in.protocol, in.format = "anthropic", store.FormatAnthropicMessages
	case strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
		in.protocol, in.format = "gemini", store.FormatGeminiGenerateContent
		suffix := strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
		model, method, ok := strings.Cut(suffix, ":")
		if !ok || (method != "generateContent" && method != "streamGenerateContent") || model == "" || strings.ContainsAny(model, "/\\") {
			return in, errors.New("unsupported Gemini model endpoint")
		}
		in.model = model
		in.stream = method == "streamGenerateContent"
	default:
		return in, errors.New("unsupported endpoint")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return in, err
	}
	if len(b) > maxBody {
		return in, errors.New("request exceeds 16 MiB")
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(b, &envelope); err != nil || envelope == nil {
		return in, errors.New("request must be a JSON object")
	}
	if in.protocol != "gemini" {
		if err = json.Unmarshal(envelope["model"], &in.model); err != nil || in.model == "" {
			return in, errors.New("model is required")
		}
		if raw, ok := envelope["stream"]; ok {
			if err = json.Unmarshal(raw, &in.stream); err != nil {
				return in, errors.New("stream must be boolean")
			}
		}
	}
	in.body = b
	return in, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientKey, authenticated := auth.ClientFromContextOK(r.Context())
	if !authenticated {
		writeError(w, "openai", 401, "client API key required")
		return
	}
	if g.Store != nil && !g.Store.VerifyClientKey(clientKey) {
		writeError(w, "openai", 401, "client API key revoked or configuration changed")
		return
	}
	if g.pluginErr != nil {
		writeError(w, "openai", 503, "cannot load plugin preferences; check plugin configuration")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		g.models(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models/specs" {
		g.ClientModelSpecs(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, "openai", 405, "method not allowed")
		return
	}
	started := time.Now()
	requestID := uuid.NewString()
	w.Header().Set(requestIDHeader, requestID)
	in, err := parse(r)
	auditWriter := &notSentWriter{ResponseWriter: w}
	w = auditWriter
	sent := false
	defer func() {
		if !sent {
			g.recordNotSent(r, in, started, auditWriter)
		}
	}()
	if err != nil {
		status := 400
		if strings.Contains(err.Error(), "exceeds") {
			status = 413
		}
		writeError(w, in.protocol, status, err.Error())
		return
	}
	if g.Store != nil && !g.Store.VerifyClientKey(clientKey) {
		writeError(w, in.protocol, 401, "client API key revoked or configuration changed")
		return
	}
	live := g.Live.Begin(in.model, in.stream, started)
	observed := &companionWriter{ResponseWriter: w, request: live}
	if live != nil {
		w = observed
		defer observed.finish(r)
	}
	settings, err := g.Store.GetSettings()
	if err != nil {
		writeError(w, in.protocol, 500, "cannot read settings")
		return
	}
	channels, err := g.reconcileRuntime()
	if err != nil {
		writeError(w, in.protocol, 500, "cannot read channels")
		return
	}
	if g.Store != nil && !g.Store.VerifyClientKey(clientKey) {
		writeError(w, in.protocol, 401, "client API key revoked or configuration changed")
		return
	}
	models, err := g.Store.ListDownstreamModels()
	if err != nil {
		writeError(w, in.protocol, 500, "cannot read models")
		return
	}
	if g.Store != nil && !g.Store.VerifyClientKey(clientKey) {
		writeError(w, in.protocol, 401, "client API key revoked or configuration changed")
		return
	}
	scope, err := g.scopeSnapshot(clientKey.Scope)
	if err != nil {
		writeError(w, in.protocol, 500, "cannot read access scope")
		return
	}
	if !scope.scope.AllowsModel(in.model) {
		writeError(w, in.protocol, 403, "client access scope denies this model")
		return
	}
	allowedModels := scope.filterModels(models, channels)
	// Permission is independent of temporary routing eligibility. A permitted
	// disabled/cooling binding remains a 503; an excluded binding is a 403.
	for _, m := range models {
		if m.ID != in.model || len(m.Bindings) == 0 {
			continue
		}
		for _, allowed := range allowedModels {
			if allowed.ID == m.ID && len(allowed.Bindings) == 0 {
				writeError(w, in.protocol, 403, "client access scope denies all upstream bindings")
				return
			}
		}
	}
	// Authorization narrows where this key may send requests, but cannot prove
	// which domain produced opaque native state. Preserve the configured domain
	// ambiguity independently of the allowed subset for the unpinned check below.
	configuredDomains := distinctDomains(route.Candidates(channels, models, in.model, in.format))
	configuredCandidates := route.Candidates(channels, allowedModels, in.model, in.format)
	candidates := g.enabledPluginCandidates(configuredCandidates)
	catalogItems, err := g.Store.ListCatalogItems("", "")
	if err != nil {
		writeError(w, in.protocol, 500, "cannot read catalog")
		return
	}
	// Resolve both independent whole tables before any upstream starts. Catalog
	// refreshes during a request affect future requests only.
	frozenPrices := map[string][2]store.PriceResolution{}
	for _, candidate := range candidates {
		actual, official := pricing.ResolveRequestPrices(settings.Prices, catalogItems, candidate.Channel, scope.tags, scope.groups, candidate.Model, in.model)
		frozenPrices[fmt.Sprintf("%d/%s", candidate.Channel.ID, candidate.Model)] = [2]store.PriceResolution{actual, official}
	}
	// Keep a successful path stable. Only external history/compaction requires
	// its origin; ordinary or opaque reasoning may travel to another allowed
	// target, which decides whether it accepts the supplied material.
	state := detectState(in.protocol, in.responses, in.body)
	reasons := []string{}
	// Two independent pin keys: the client's own session identifier, and a
	// fingerprint of the oldest state carrier for clients that send neither.
	// Both are namespaced by client key, downstream model and inbound format:
	// the key describes the conversation the way the client speaks it, so a
	// request that arrives in another format is another key and follows no pin
	// from this one — the endpoint world it would continue belongs to that
	// format, not to this one. A request can carry both, so both are looked up:
	// two domains for one conversation is a conflict, not a coin toss.
	session, sessionKind := sessionKey(r, in.format, in.responses, in.body, clientKey.ID, in.model)
	fingerprint := carrierFingerprint(clientKey.ID, in.model, in.format, state.oldest)
	var pinned domain
	pinKind := ""
	pinnedStrict := false
	if session != "" {
		if d, ok := g.affinity.lookup(session); ok {
			pinned, pinKind = d, sessionKind
		}
	}
	if fingerprint != "" {
		if d, ok := g.affinity.lookup(fingerprint); ok {
			switch {
			case pinned.channel == 0:
				pinned, pinKind = d, "carrier_fingerprint"
			case d != pinned && state.requiresOrigin():
				// Both keys name this conversation — the same namespaces
				// produced them — and they point at two different continuation
				// domains, so one of the pins is stale and which one is not
				// visible from here. Following either would be a guess, and the
				// carrier of the wrong guess is dropped by the upstream rather
				// than answered: refuse, and let the client sort its session
				// out. Neither pin resolves by waiting — the conversation has
				// two live domains until the client does — so the recovery
				// carries no retry_later.
				reasons = append(reasons, "affinity_conflict")
				writeErrorEx(w, in.protocol, 409,
					"this request's session identifier and its state carrier resolve to different continuation domains; to-router will not guess which domain the conversation continues on",
					&recovery{Code: "state_affinity_conflict", Actions: []string{"handoff", "start_fresh_session"},
						Detail: "the session id is pinned on one continuation domain while the carrier it sent was produced on another; hand off to a new session or start a fresh one"})
				return
			}
		}
	}
	if pinned.channel != 0 {
		if state.requiresOrigin() {
			for _, c := range channels {
				if c.ID == pinned.channel && !scope.allows(c, pinned.model) {
					writeError(w, in.protocol, 403, "client access scope denies the state continuation domain")
					return
				}
			}
		}
	}
	if len(candidates) == 0 {
		if len(configuredCandidates) > 0 {
			writeErrorEx(w, in.protocol, 503, disabledPluginReason, disabledPluginRecovery())
			return
		}
		// No declared endpoint can serve this request in the current scope.
		writeErrorEx(w, in.protocol, 503, "no enabled channel supports this model and endpoint",
			&recovery{Code: "no_usable_endpoint", Actions: []string{"enable_channel", "declare_endpoint"},
				Detail: "no enabled channel bound to this model declares an endpoint that can serve " + in.format})
		return
	}
	if pinned.channel != 0 {
		if state.requiresOrigin() {
			for _, c := range channels {
				if c.ID == pinned.channel && !g.pluginEnabled(c) {
					writeErrorEx(w, in.protocol, 503, disabledPluginReason, disabledPluginRecovery())
					return
				}
			}
		}
		// The pin outranks the endpoint preference. The conversation is already
		// being served on one endpoint world of the pinned channel, and that
		// world is what keeps its state continuable, so the pinned channel's
		// endpoint is re-resolved against the pinned format before the domain is
		// restricted or reordered: a preference the owner changed afterwards
		// must not evict the session from an endpoint that is still declared.
		// The pinned endpoint is there whenever the generation matches — the
		// declaration set is what the generation counts — and when it is not,
		// the state_domain_changed path below still applies. Overriding the
		// endpoint here rather than changing the router keeps PickEndpoint's
		// meaning for unpinned routing, where the preference is the operator's
		// current intent for a fresh request.
		for i := range candidates {
			if candidates[i].Channel.ID != pinned.channel {
				continue
			}
			if ep, ok := route.PickModelEndpoint(candidates[i].Channel, candidates[i].Model, pinned.format); ok {
				candidates[i].Endpoint = ep
			}
		}
		switch {
		case !state.requiresOrigin():
			// Soft: a request without state is portable, so the pinned domain
			// leads and the remaining candidates follow instead of being
			// dropped. When the pinned domain fails — its retries spent, its
			// breaker open — the next candidate still gets its turn, and the
			// success path re-pins the session onto the domain that answered,
			// so the conversation moves as a whole instead of splitting.
			candidates = pinFirst(candidates, pinned)
			slog.Info("session affinity applied", "model", in.model, "channel_id", pinned.channel, "upstream_model", pinned.model, "endpoint_format", pinned.format, "pin_kind", pinKind, "strength", "soft")
		default:
			// Hard: the request carries the state this pin's domain produced, so
			// that domain is the only one that can continue it. The candidates
			// collapse to it and nothing else is tried, whatever it costs; a
			// failure from here on is that domain being unreachable, which the
			// final error names (see pinnedStrict below).
			if kept := restrictToDomain(candidates, pinned); len(kept) > 0 {
				candidates = kept
				pinnedStrict = true
				slog.Info("session affinity applied", "model", in.model, "channel_id", pinned.channel, "upstream_model", pinned.model, "endpoint_format", pinned.format, "pin_kind", pinKind, "strength", "hard")
			} else if reconfiguredChannel(candidates, pinned) {
				// The channel is still here; what it serves is not. A rotated
				// credential, a moved base URL or a changed endpoint set keeps the
				// id and bumps the generation, and reverting the edit bumps it
				// again — the configuration that produced this state cannot be
				// waited back, so retry_later would poll a 503 forever.
				reasons = append(reasons, "affinity_domain_changed")
				writeErrorEx(w, in.protocol, 503,
					"the channel that owns this conversation's state was reconfigured; this state belongs to its previous configuration; hand off to a new session or start a fresh one",
					&recovery{Code: "state_domain_changed", Actions: []string{"handoff", "start_fresh_session"},
						Detail: "the channel kept its id but its continuation domain moved on; reverting the configuration bumps the generation again, so the old domain never comes back"})
				return
			} else {
				reasons = append(reasons, "affinity_unavailable")
				writeErrorEx(w, in.protocol, 503,
					"the channel that owns this conversation's state is unavailable; retry when it recovers, or hand off to a new session",
					&recovery{Code: "state_affinity_unavailable", Actions: []string{"retry_later", "handoff", "start_fresh_session"},
						Detail: "pinned continuation domain is not among the enabled candidates for this model"})
				return
			}
		}
	} else if state.requiresOrigin() && configuredDomains > 1 {
		// No pin and the request carries state: sending it anywhere would be a
		// guess. Refuse and say how to fix it.
		reasons = append(reasons, "affinity_unknown")
		writeErrorEx(w, in.protocol, 409,
			"this request carries upstream-native state but its origin channel is unknown; to-router will not guess which channel to send it to",
			&recovery{Code: "state_affinity_unknown", Actions: []string{"use_single_binding", "handoff", "start_fresh_session"},
				Detail: "bind this downstream model to one channel for stateful sessions, or start a fresh session without history"})
		return
	}
	pathScope := session
	if pathScope == "" {
		pathScope = fingerprint
	}
	if pathScope == "" {
		pathScope = fmt.Sprintf("client:%d/%s/%s", clientKey.ID, in.model, in.format)
	}
	previous, hasPrevious := g.previousPath(pathScope)
	if state.requiresOrigin() {
		// A server-side history reference belongs to an account, not merely a
		// channel. Do not round-robin it into another credential in the pool.
		originCredential := pinned.credential
		if originCredential == "" && hasPrevious && (session != "" || fingerprint != "") {
			originCredential = previous.CredentialID
		}
		for i := range candidates {
			a := &candidates[i]
			if originCredential == "" && len(a.Channel.Credentials) > 1 {
				writeErrorEx(w, in.protocol, 409, "the history reference's original credential is unknown; use the original account or submit full history", diagnosticRecovery(nil, newConversionDiagnostic("context_unavailable", "routing")))
				return
			}
			if originCredential != "" {
				var original []store.CredentialItem
				for _, cred := range a.Channel.Credentials {
					if cred.ID == originCredential {
						original = append(original, cred)
					}
				}
				a.Channel.Credentials = original
			}
		}
	}
	if hasPrevious && !pinnedStrict {
		candidates = preferPath(candidates, previous)
	}
	if !route.FallbackEnabled(models, in.model) && len(candidates) > 1 {
		candidates = candidates[:1]
		reasons = append(reasons, "fallback_disabled")
	}
	if state.present() {
		reasons = append(reasons, "request_stateful")
		slog.Info("request carries provider-native state", "model", in.model, "kinds", strings.Join(state.kinds, ","), "pinned", pinKind != "")
	}
	if state.requiresOrigin() {
		reasons = append(reasons, "request_origin_bound")
	}
	limit := settings.MaxAttempts
	if limit < 1 {
		limit = 3
	}
	retries := settings.RetriesPerChannel
	if retries < 1 {
		retries = store.DefaultRetriesPerChannel
	}
	attempts := 0
	lastStatus := 503
	lastError := "all candidate channels are cooling down"
	var attemptsList []store.AttemptRecord
	var attemptLease health.Lease
	var actualCallStart time.Time
	addAttempt := func(c store.Channel, cred store.CredentialItem, model string, status int, duration int64, errReason, action string) {
		g.Breaker.Observe(attemptLease, health.LastRequest{At: actualCallStart, Status: status, DurationMS: time.Since(actualCallStart).Milliseconds(), Error: safeRuntimeReason(errReason)})
		attemptsList = append(attemptsList, store.AttemptRecord{
			AttemptIndex:      len(attemptsList) + 1,
			ChannelID:         c.ID,
			ChannelName:       c.Name,
			CredentialID:      cred.ID,
			CredentialMaskKey: cred.Masked(),
			UpstreamModel:     model,
			StatusCode:        status,
			DurationMS:        duration,
			ErrorReason:       errReason,
			ActionTaken:       action,
		})
		auditWriter.attempts = attemptsList
	}
	var configurationAdvice string
	var lastDiagnostic *ConversionDiagnostic
	reasoningRetried := false
candidates:
	for _, candidate := range candidates {
		if attempts >= limit || r.Context().Err() != nil {
			break
		}
		c := candidate.Channel
		model := candidate.Model
		configurationAdvice = ""
		lastDiagnostic = nil
		if !g.pluginEnabled(c) {
			lastStatus, lastError = 503, disabledPluginReason
			continue
		}
		cfg := c.ModelConfigs[model]
		var defaults *store.ModelDefaults
		if settings.RequestDefaultsEnabled {
			defaults = &settings.RequestDefaults
		}
		opts := g.conversionOptions(settings, clientKey.ID, in.model, candidate, "")
		body, adapted, err := candidateBody(in, candidate.Endpoint.Format, model, cfg, defaults, c.CredentialType == store.CredentialChatGPTOAuth, opts)
		if err != nil {
			lastStatus = 400
			lastError = "unsupported cross-protocol request: " + err.Error()
			configurationAdvice = clientConfigurationAdvice(in, err.Error(), adapted)
			lastDiagnostic = conversionFailure(err, "request_conversion")
			continue
		}
		// The SIWC profile is enforced per candidate, because it is the
		// channel that decides it: a request this candidate cannot serve as
		// asked is one another channel may serve exactly.
		buffered := false
		if c.CredentialType == store.CredentialChatGPTOAuth {
			rewritten, stripped, adaptedArgs, profileErr := ProfileRequestBody(body)
			if profileErr != nil {
				if errors.Is(profileErr, ErrServerStateUnsupported) {
					// Server-side state on a profile that keeps none. Sending
					// it would answer a different question than the one asked,
					// so this candidate is refused and the request moves on;
					// if every candidate is a ChatGPT channel, the client gets
					// the recovery block below.
					reasons = append(reasons, "siwc_profile_stateless")
					lastStatus = 400
					lastError = "this request carries server-side conversation state, which the chatgpt_oauth profile does not support; send the full conversation in input instead"
					continue
				}
				lastStatus = 400
				lastError = "unsupported chatgpt_oauth request: " + profileErr.Error()
				continue
			}
			body = rewritten
			adapted = append(adapted, strippedDegradations(stripped)...)
			adapted = append(adapted, adaptedDegradations(adaptedArgs)...)
			if !in.stream {
				buffered = true
			}
		}
		attemptStart := time.Now()
		usage := store.Usage{RequestID: requestID, Estimated: true, Time: attemptStart, ChannelID: c.ID, ChannelName: c.Name, Model: in.model, UpstreamModel: model, Inbound: in.format, Outbound: candidate.Endpoint.Format, ClientKeyID: clientKey.ID, ClientKeyName: clientKey.Name, Stateful: state.present(), Degraded: mergeDegradations("", adapted)}
		selectedPrices := frozenPrices[fmt.Sprintf("%d/%s", c.ID, model)]
		usage.ActualPricing = &store.UsagePricingBasis{Resolution: selectedPrices[0]}
		usage.OfficialPricing = &store.UsagePricingBasis{Resolution: selectedPrices[1]}
		if len(adapted) > 0 {
			slog.Warn("lossy adaptation applied to request", "channel", c.Name, "kinds", usage.Degraded, "model", in.model)
		}
		// Same-channel retries come before any cross-channel failover: retrying
		// the channel that already owns the state cannot lose it, while moving
		// to another channel can. The breaker is consulted per attempt, so a
		// channel that has gone cold stops the retries instead of absorbing
		// them all.
		//
		// Whether a failure may be resent at all is graded first, by whether
		// the upstream may already have processed the request (see
		// classifyTransportError). A transient failure never reached the
		// upstream, so repeating it is free; an ambiguous one may have been
		// executed there, and neither a retry here nor a failover to another
		// channel can take that back — so neither happens, and the client
		// decides.
		activeCred, selected := g.pickCredential(r.Context(), candidate, previous, hasPrevious, "")
		if !selected {
			lastError = g.unavailableReason(candidate)
			continue
		}
		acquireSkips := 0
		var reasoningRetryBody []byte
		var reasoningRetryKey health.Key
		var reasoningRemoved []string
		for try := 1; ; try++ {
			key := combination(candidate, activeCred.ID)
			lease, acquired := g.Breaker.Acquire(key)
			lease = lease.WithCooldown(failureCooldown(settings))
			if !acquired {
				acquireSkips++
				if acquireSkips >= len(c.Credentials) {
					break
				}
				if next, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, activeCred.ID); ok {
					activeCred = next
					try--
					continue
				}
				break
			}
			attemptLease = lease
			if try == 1 {
				attempts++
			}
			live.Attempt(c.ID, c.Name)
			observed.code = ""
			lastDiagnostic = nil
			// The previous retry has already been recorded. Its error must not
			// turn this attempt's successful result into an incomplete response.
			usage.Error = ""
			usage.Diagnostic = nil
			usage.Degraded = mergeDegradations("", adapted)
			outgoingBody := body
			if key == reasoningRetryKey && len(reasoningRetryBody) > 0 {
				outgoingBody = reasoningRetryBody
				usage.Degraded = mergeDegradations(usage.Degraded, reasoningRemoved)
				d := newConversionDiagnostic("reasoning_state_dropped", "upstream_request")
				d.ActionsTaken = []string{"上游明确拒绝思考材料后，已在同一目标上移除相应思考并重试一次。"}
				setUsageDiagnostic(&usage, d)
			}
			opts = g.conversionOptions(settings, clientKey.ID, in.model, candidate, activeCred.ID)
			effectiveChannel := c
			effectiveChannel.APIKey = activeCred.APIKey
			effectiveChannel.Credentials = []store.CredentialItem{activeCred}
			if activeCred.OAuth != nil {
				effectiveChannel.OAuth = activeCred.OAuth
			}
			callStart := time.Now()
			actualCallStart = callStart
			resp, reqErr := g.call(r, in, candidate.Endpoint, effectiveChannel, model, outgoingBody)
			callDuration := time.Since(callStart).Milliseconds()
			if reqErr == nil {
				sent = true
				g.quotaBypass(c, activeCred.ID, true)
			}
			cancelled := func() bool {
				if r.Context().Err() == nil {
					return false
				}
				g.Breaker.Release(lease)
				usage.Status = 499
				usage.Error = "client cancelled"
				addAttempt(c, activeCred, model, 499, time.Since(callStart).Milliseconds(), usage.Error, store.ActionTakenClientCancelled)
				usage.Attempts = attemptsList
				if sent {
					g.record(usage, settings, attemptStart)
				}
				return true
			}
			if cancelled() {
				if resp != nil && resp.body != nil {
					resp.body.Close()
				}
				return
			}
			earlyStreamRejection := false
			if reqErr == nil && resp.err == nil && resp.status >= 200 && resp.status < 300 && (in.stream || buffered) && strings.Contains(strings.ToLower(resp.header.Get("Content-Type")), "text/event-stream") {
				replay, rejection, probeErr := probeReasoningRejection(resp.body, candidate.Endpoint.Format)
				resp.body = replay
				if probeErr == nil && len(rejection) > 0 {
					// This narrow case is a validation error in the first SSE
					// event, before any output. Other 2xx streams remain committed.
					resp.body.Close()
					resp.body = io.NopCloser(bytes.NewReader(rejection))
					resp.status = http.StatusBadRequest
					earlyStreamRejection = true
				}
			}
			if reqErr == nil && (resp.status == 429 || resp.status == 503) {
				if d, ok := parseRetryAfter(resp.header.Get("Retry-After")); ok {
					lease = lease.WithRetryAfter(d)
					attemptLease = lease
				}
			}
			if reqErr != nil {
				g.Breaker.Release(lease)
				if r.Context().Err() != nil {
					usage.Status = 499
					usage.Error = "client cancelled"
					addAttempt(c, activeCred, model, 499, callDuration, "client cancelled", store.ActionTakenClientCancelled)
					usage.Attempts = attemptsList

					return
				}
				if errors.Is(reqErr, plugins.ErrDisabled) {
					lastStatus, lastError = 503, disabledPluginReason
					observed.code = "connection_plugin_disabled"
					// This was rejected locally before the outgoing request; it
					// is neither a failed upstream nor a new health observation.
					break
				}
				if errors.Is(reqErr, pluginhost.ErrNotInstalled) {
					lastStatus, lastError = 503, unavailablePluginReason
					observed.code = "connection_plugin_unavailable"
					break
				}
				if errors.Is(reqErr, nativeoauth.ErrUnsupportedRequest) {
					lastStatus = 400
					lastError = reqErr.Error()
					lastDiagnostic = newConversionDiagnostic("conversion_unsupported", "upstream_request")
					observed.code = "conversion_unsupported"
					break
				}
				if try < retries {
					if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, activeCred.ID); ok {
						addAttempt(c, activeCred, model, 502, callDuration, reqErr.Error(), store.ActionTakenCoolingSwitched)
						activeCred = nextCred
						live.Retrying()
						continue
					}
				}
				lastStatus = 502
				// The error belongs to the candidate declaration: an unusable
				// URL, a base outside the OAuth host allowlist, or a credential
				// that could not be refreshed. It names the cause, so it travels
				// as-is instead of a generic label.
				lastError = reqErr.Error()
				observed.code = "configuration_error"
				addAttempt(c, activeCred, model, 502, callDuration, reqErr.Error(), store.ActionTakenExhaustedFallback)
				live.Retrying()
				break
			}
			if resp.err != nil {
				if r.Context().Err() != nil {
					g.Breaker.Release(lease)
					usage.Status = 499
					usage.Error = "client cancelled"
					addAttempt(c, activeCred, model, 499, callDuration, "client cancelled", store.ActionTakenClientCancelled)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					return
				}
				g.failRequest(r.Context(), lease)
				if classifyTransportError(resp.err) == errAmbiguous {
					// The connection was up when the exchange broke down, so the
					// upstream may have executed the request already. Resending it
					// here — or on another channel — would repeat the work, the
					// cost and any side effect, so both are refused: the attempt
					// ends here, and the client gets the recovery block with the
					// ways forward.
					usage.Status = 502
					usage.Error = "upstream connection failed or timed out; the request may have been processed"
					addAttempt(c, activeCred, model, 502, callDuration, usage.Error, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					lastStatus = 502
					lastError = "the upstream may have already processed this request; to-router did not retry it to avoid duplicate execution"
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				}
				usage.Status = 502
				usage.Error = "upstream connection failed or timed out"
				lastStatus, lastError = 502, usage.Error
				live.Retrying()
				if try < retries {
					if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, activeCred.ID); ok {
						addAttempt(c, activeCred, model, 502, callDuration, resp.err.Error(), store.ActionTakenCoolingSwitched)
						usage.Attempts = attemptsList
						g.record(usage, settings, attemptStart)
						activeCred = nextCred
						continue
					}
					if !quotaUnavailable(g.quotaObservation(c, activeCred)) && waitFor(r.Context(), g.Breaker.Remaining(key)) {
						if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, ""); ok {
							addAttempt(c, activeCred, model, 502, callDuration, resp.err.Error(), store.ActionTakenRetry)
							usage.Attempts = attemptsList
							g.record(usage, settings, attemptStart)
							activeCred = nextCred
							continue
						}
					}
					if r.Context().Err() != nil {
						usage.Status = 499
						usage.Error = "client cancelled"
						addAttempt(c, activeCred, model, 499, callDuration, "client cancelled", store.ActionTakenClientCancelled)
						usage.Attempts = attemptsList
						g.record(usage, settings, attemptStart)
						return
					}
				}
				addAttempt(c, activeCred, model, 502, callDuration, resp.err.Error(), store.ActionTakenExhaustedFallback)
				usage.Attempts = attemptsList
				g.record(usage, settings, attemptStart)
				break
			}
			if resp.status < 200 || resp.status >= 300 {
				errorBody, _ := io.ReadAll(io.LimitReader(resp.body, 64<<10))
				if cancelled() {
					resp.body.Close()
					return
				}
				configurationAdvice = ""
				if resp.status == 400 || resp.status == 422 {
					configurationAdvice = clientConfigurationAdvice(in, string(errorBody), adapted)
				}
				resp.body.Close()
				lastStatus = resp.status
				lastError = fmt.Sprintf("upstream returned HTTP %d", resp.status)
				usage.Status = lastStatus
				usage.Error = lastError
				lastDiagnostic = newConversionDiagnostic("upstream_rejected", "upstream_response")
				lastDiagnostic.Facts = append(lastDiagnostic.Facts, fmt.Sprintf("本次上游返回 HTTP %d。", resp.status))
				if isReasoningCompatibilityRejection(resp.status, errorBody) {
					lastDiagnostic = newConversionDiagnostic("reasoning_state_rejected", "upstream_response")
					if earlyStreamRejection {
						lastDiagnostic.Facts = append(lastDiagnostic.Facts, "HTTP 200 流的首个事件是思考兼容拒绝；尚未向客户端输出内容。")
					}
					if settings.ReasoningPolicy == store.ReasoningAvailability && !reasoningRetried && try < retries {
						stripped, removed, stripErr := stripReasoning(outgoingBody, candidate.Endpoint.Format)
						if stripErr == nil && len(removed) > 0 {
							reasoningRetried = true
							reasoningRetryBody, reasoningRetryKey, reasoningRemoved = stripped, key, removed
							lastDiagnostic.ActionsTaken = []string{"已确认执行前思考兼容拒绝；将在同一目标上移除相应思考，重试一次。"}
							setUsageDiagnostic(&usage, lastDiagnostic)
							g.Breaker.Release(lease)
							addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenRetry)
							usage.Attempts = attemptsList
							g.record(usage, settings, attemptStart)
							live.Retrying()
							continue
						}
						if stripErr != nil {
							lastDiagnostic.ActionsTaken = []string{"移除思考后无法构成完整请求，未重试。"}
						}
					} else if reasoningRetried {
						lastDiagnostic.ActionsTaken = []string{"本次逻辑请求已经尝试过一次思考剥离，不再重复。"}
					} else if settings.ReasoningPolicy == store.ReasoningAvailability && try >= retries {
						lastDiagnostic.ActionsTaken = []string{"当前渠道重试次数已用尽，未增加额外请求。"}
					}
				}
				setUsageDiagnostic(&usage, lastDiagnostic)
				// One status, two independent verdicts: the replay axis decides
				// whether the request may be sent again, the health axis what the
				// breaker counts. A 507 and a 501 agree on the first and differ
				// on the second, which is why the two are read apart.
				grade := gradeStatus(resp.status)
				if resp.status == http.StatusRequestTimeout {
					lastDiagnostic = newConversionDiagnostic("execution_unconfirmed", "upstream_response")
					lastDiagnostic.Facts = append(lastDiagnostic.Facts, "上游返回 HTTP 408。")
					setUsageDiagnostic(&usage, lastDiagnostic)
					// A 408 is the upstream saying it stopped waiting for the
					// request — which, from a proxy that has already run it, can
					// happen after the fact. Grade it like the ambiguous
					// transport failures: no retry on this channel, no failover
					// to another. The upstream's own status travels, because it
					// did answer, and the recovery block says why we stopped.
					usage.Error = "upstream returned HTTP 408; the request may have been processed"
					addAttempt(c, activeCred, model, resp.status, callDuration, usage.Error, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					g.gradeHealth(lease, grade.health)
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				}
				switch grade.replay {
				case replayAllowed:
					g.gradeHealth(lease, grade.health)
					live.Retrying()
					if try < retries {
						if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, activeCred.ID); ok {
							addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenCoolingSwitched)
							usage.Attempts = attemptsList
							g.record(usage, settings, attemptStart)
							activeCred = nextCred
							continue
						}
						if !quotaUnavailable(g.quotaObservation(c, activeCred)) && waitFor(r.Context(), g.Breaker.Remaining(key)) {
							if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, ""); ok {
								addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenRetry)
								usage.Attempts = attemptsList
								g.record(usage, settings, attemptStart)
								activeCred = nextCred
								continue
							}
						}
						if r.Context().Err() != nil {
							usage.Status = 499
							usage.Error = "client cancelled"
							addAttempt(c, activeCred, model, 499, callDuration, "client cancelled", store.ActionTakenClientCancelled)
							usage.Attempts = attemptsList
							g.record(usage, settings, attemptStart)
							return
						}
					}
					addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
				case replayAmbiguous:
					lastDiagnostic = newConversionDiagnostic("execution_unconfirmed", "upstream_response")
					lastDiagnostic.Facts = append(lastDiagnostic.Facts, fmt.Sprintf("上游返回 HTTP %d。", resp.status))
					setUsageDiagnostic(&usage, lastDiagnostic)
					// A 5xx that does not prove the request was not run. A gateway
					// upstream reports a request it already executed as one of its
					// own timeouts, and 500/502/504 is where those land, so this
					// is graded exactly like the ambiguous transport failures:
					// spent once, never resent, never failed over. The breaker
					// still counts it — a channel answering 5xx is what the
					// gateway-level health signal is for.
					usage.Status = 502
					usage.Error = fmt.Sprintf("upstream returned HTTP %d; the request may have been processed", resp.status)
					addAttempt(c, activeCred, model, resp.status, callDuration, usage.Error, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					g.gradeHealth(lease, grade.health)
					lastStatus = 502
					lastError = fmt.Sprintf("the upstream returned HTTP %d and may have already processed this request; to-router did not retry it, here or on another channel, to avoid duplicate execution", resp.status)
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				default:
					g.gradeHealth(lease, grade.health)
					if (resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden) && try < retries {
						if nextCred, ok := g.pickCredential(r.Context(), candidate, health.Key{}, false, activeCred.ID); ok {
							addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenCoolingSwitched)
							usage.Attempts = attemptsList
							g.record(usage, settings, attemptStart)
							activeCred = nextCred
							live.Retrying()
							continue
						}
					}
					// A deterministic answer: the status is about the request,
					// about what this upstream supports, or about a resource it
					// has run out of, so another attempt, here or on another
					// channel, reproduces it exactly. The health axis decides
					// what the breaker hears: a capability the channel lacks is
					// released without a failure — a missing capability is not an
					// unhealthy channel — while a resource it has exhausted
					// counts. For the 5xx of this kind the upstream's own status
					// carries, with the reason spelled out.
					addAttempt(c, activeCred, model, resp.status, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					if resp.status >= 500 {
						if grade.health == healthFailure {
							lastError = fmt.Sprintf("upstream returned HTTP %d; the failure is the channel running out of a resource rather than anything about this request, so another attempt, here or on another channel, would meet the same exhaustion", resp.status)
						} else {
							lastError = fmt.Sprintf("upstream returned HTTP %d; the failure is about the request or about what this upstream supports, not about its health, so another attempt, here or on another channel, would fail the same way", resp.status)
						}
					}
					if lastStatus < 400 {
						lastStatus = 502
					}
					if configurationAdvice != "" {
						lastError += "; " + configurationAdvice
					}
					writeErrorEx(w, in.protocol, lastStatus, lastError, diagnosticRecovery(recoveryFor(reasons), lastDiagnostic))
					return
				}
				// Retryable and the same-channel budget is spent: the next
				// candidate gets its turn.
				break
			}
			usage.Status = resp.status
			writeDegradationHeader(w, usage)
			if buffered {
				// The profile streams whatever the client asked for, so a
				// non-streaming caller gets the completed Response object
				// reassembled from the stream. The upstream already ran the
				// request: a failure here is committed, exactly like a truncated
				// body on any other channel.
				result, bufferErr := bufferChatGPT(resp.body, resp.header.Get("Content-Type"))
				resp.body.Close()
				if cancelled() {
					return
				}
				if bufferErr != nil {
					g.failRequest(r.Context(), lease)
					lastStatus = 502
					lastError = "upstream stream could not be assembled into a response; the upstream already processed this request, so to-router did not retry it"
					usage.Status = 502
					usage.Error = bufferErr.Error()
					addAttempt(c, activeCred, model, 502, callDuration, bufferErr.Error(), store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				}
				if inTokens, outTokens, crTokens, cwTokens, hasUsage := convert.ParseUsage(result, candidate.Endpoint.Format); hasUsage {
					usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens = inTokens, outTokens, crTokens, cwTokens
					usage.Estimated = false
				} else {
					// The assembled object carried no usage: the row must say the
					// numbers are missing rather than report zeros as measured.
					usage.Estimated = true
				}
				if !json.Valid(result) {
					g.failRequest(r.Context(), lease)
					lastStatus = 502
					lastError = "upstream returned invalid JSON; the upstream already processed this request, so to-router did not retry it"
					usage.Status = 502
					usage.Error = lastError
					addAttempt(c, activeCred, model, 502, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				}
				originalResult := result
				healthy := g.gradeResponsesResult(originalResult, lease, &usage)
				if candidate.Endpoint.Format != in.format {
					var dropped []string
					result, dropped, err = convert.ResponseWithOptions(result, candidate.Endpoint.Format, in.format, in.model, opts)
					if err != nil {
						g.Breaker.Release(lease)
						usage.Status, lastStatus = 502, 502
						lastError = "upstream response could not be converted; the request was not replayed"
						usage.Error = lastError
						lastDiagnostic = conversionFailure(err, "response_conversion")
						setUsageDiagnostic(&usage, lastDiagnostic)
						addAttempt(c, activeCred, model, usage.Status, callDuration, lastError, store.ActionTakenExhaustedFallback)
						usage.Attempts = attemptsList
						g.record(usage, settings, attemptStart)
						reasons = append(reasons, "conversion_refused")
						break candidates
					}
					usage.Degraded = mergeDegradations(usage.Degraded, dropped)
					writeDegradationHeader(w, usage)
				}
				if healthy {
					if g.Breaker.Success(lease) {
						g.rememberPaths(pathScope, session, fingerprint, key)
						g.pin(session, fingerprint, domain{channel: c.ID, model: model, format: candidate.Endpoint.Format, generation: c.Generation, credential: activeCred.ID})
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if output, reported := convert.ReportedOutputTokens(originalResult, candidate.Endpoint.Format); reported {
					live.Output(output)
				}
				w.WriteHeader(200)
				_, writeErr := w.Write(result)
				if writeErr != nil {
					usage.Status = 499
					usage.Error = "client disconnected"
				}
				usage.TTFTMS = time.Since(attemptStart).Milliseconds()
				addAttempt(c, activeCred, model, usage.Status, callDuration, usage.Error, store.ActionTakenSuccess)
				usage.Attempts = attemptsList
				g.record(usage, settings, attemptStart)
				observed.jsonResult(usage, candidate.Endpoint.Format, originalResult)
				return
			}
			if in.stream {
				if !strings.Contains(strings.ToLower(resp.header.Get("Content-Type")), "text/event-stream") {
					resp.body.Close()
					// The upstream answered 2xx: the request was accepted and run
					// there, so it is committed. A body that is not SSE is the
					// channel's line, not a reason to spend the inference again —
					// the breaker counts the failure, and neither a resend nor a
					// failover may repeat the work.
					g.failRequest(r.Context(), lease)
					lastStatus = 502
					lastError = "upstream did not return an SSE stream; the upstream already processed this request, so to-router did not retry it"
					usage.Status = 502
					usage.Error = lastError
					addAttempt(c, activeCred, model, 502, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					reasons = append(reasons, "upstream_may_have_processed")
					break candidates
				}
				observer := newObserver(resp.body, candidate.Endpoint.Format, &usage)
				if live != nil {
					observer.onOutput = live.Output
					observer.onTextUnits = live.TextUnits
				}
				fw := &flushWriter{ResponseWriter: w, start: started, usage: &usage}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				var streamErr error
				var dropped []string
				if candidate.Endpoint.Format == in.format {
					_, streamErr = io.Copy(fw, observer)
				} else {
					dropped, streamErr = convert.StreamWithOptions(observer, fw, candidate.Endpoint.Format, in.format, in.model, opts)
				}
				resp.body.Close()
				if len(dropped) > 0 {
					usage.Degraded = mergeDegradations(usage.Degraded, dropped)
					slog.Warn("stripped reasoning from streamed response", "channel", c.Name, "kinds", strings.Join(dropped, ","))
				}
				if streamErr == nil && !observer.complete {
					streamErr = errors.New("upstream stream ended without a completion event")
				}
				nativeFailure := false
				if streamErr == nil {
					switch observer.responseTerminal {
					case "response.failed":
						streamErr = errors.New("upstream response failed")
						nativeFailure = true
					case "error":
						streamErr = errors.New("upstream stream error")
						nativeFailure = true
					}
				}
				if streamErr != nil {
					diagnostic := newConversionDiagnostic("stream_interrupted", "stream")
					if errors.Is(streamErr, convert.ErrUnsupported) || errors.Is(streamErr, convert.ErrReasoningState) || errors.Is(streamErr, convert.ErrContinuationState) {
						diagnostic = conversionFailure(streamErr, "response_conversion")
					}
					diagnostic.RequestID = requestID
					diagnostic.ActionsTaken = []string{"响应已经开始处理，未重发请求，也未切换上游。"}
					setUsageDiagnostic(&usage, diagnostic)
					usage.Status = 502
					usage.Error = "stream interrupted"
					if nativeFailure {
						usage.Error = streamErr.Error()
					}
					action := store.ActionTakenExhaustedFallback
					status := 502
					if r.Context().Err() != nil {
						usage.Status = 499
						usage.Error = "client cancelled"
						status = 499
						action = store.ActionTakenClientCancelled
						g.Breaker.Release(lease)
					} else if streamFailureCounts(streamErr) {
						g.failRequest(r.Context(), lease)
					} else {
						g.Breaker.Release(lease)
					}
					continuation := errors.Is(streamErr, convert.ErrContinuationState)
					if continuation {
						usage.Error = "stream carries reasoning the client protocol cannot preserve"
					}
					if !fw.wrote {
						if continuation {
							// Nothing reached the client yet, so this refusal is
							// the whole answer: the usage row must carry the
							// status the client is about to see.
							usage.Status = 400
							status = 400
							writeErrorEx(w, in.protocol, 400, "upstream stream carries state that could not be preserved", diagnosticRecovery(nil, diagnostic))
						} else {
							writeErrorEx(w, in.protocol, 502, "upstream stream could not be converted", diagnosticRecovery(nil, diagnostic))
						}
					} else if r.Context().Err() == nil && !nativeFailure {
						writeStreamDiagnostic(fw, in.format, diagnostic)
					}
					addAttempt(c, activeCred, model, status, callDuration, usage.Error, action)
				} else if observer.responseTerminal == "response.incomplete" {
					// The typed terminal arrived intact, but generation stopped at
					// a token limit or content filter. Preserve its native 200 and
					// usage without calling the channel broken or the turn complete.
					usage.Error = "upstream response incomplete: " + observer.incompleteReason
					g.Breaker.Release(lease)
					addAttempt(c, activeCred, model, 200, callDuration, usage.Error, store.ActionTakenSuccess)
				} else {
					if g.Breaker.Success(lease) {
						g.rememberPaths(pathScope, session, fingerprint, key)
						g.pin(session, fingerprint, domain{channel: c.ID, model: model, format: candidate.Endpoint.Format, generation: c.Generation, credential: activeCred.ID})
					}
					addAttempt(c, activeCred, model, usage.Status, callDuration, usage.Error, store.ActionTakenSuccess)
				}
				usage.Estimated = !observer.hasUsage
				usage.Attempts = attemptsList
				g.record(usage, settings, attemptStart)
				observed.result(usage)
				return
			}
			// Everything below runs on an upstream 2xx: the request was
			// accepted and run, so it is committed. A failure from here on — a
			// truncated or oversized body, an unparseable or unconvertible
			// answer — may not be resent, here or on another channel: that
			// would repeat the inference, the cost and any side effect. The
			// breaker still grades the failure for what it says about the
			// channel (a broken wire versus a capability it lacks), but the
			// attempt ends and the client decides.
			result, readErr := io.ReadAll(io.LimitReader(resp.body, maxBody+1))
			resp.body.Close()
			if cancelled() {
				return
			}
			if readErr != nil || len(result) > maxBody {
				// The body broke mid-response or overflowed the cap: that is
				// the channel's wire, so the breaker counts a failure.
				g.failRequest(r.Context(), lease)
				lastStatus = 502
				lastError = "upstream response incomplete or exceeds 16 MiB; the upstream already processed this request, so to-router did not retry it"
				usage.Status = 502
				usage.Error = lastError
				addAttempt(c, activeCred, model, 502, callDuration, lastError, store.ActionTakenExhaustedFallback)
				usage.Attempts = attemptsList
				g.record(usage, settings, attemptStart)
				reasons = append(reasons, "upstream_may_have_processed")
				break candidates
			}
			// The endpoint's own format, not its family: a Responses body
			// reports input_tokens/output_tokens, which the Chat Completions
			// schema would read as no usage at all (T024).
			inTokens, outTokens, crTokens, cwTokens, hasUsage := convert.ParseUsage(result, candidate.Endpoint.Format)
			usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens = inTokens, outTokens, crTokens, cwTokens
			usage.Estimated = !hasUsage
			if output, reported := convert.ReportedOutputTokens(result, candidate.Endpoint.Format); reported {
				live.Output(output)
			}
			var dropped []string
			originalResult := result
			responseHealthy := true
			if candidate.Endpoint.Format == store.FormatOpenAIResponses && json.Valid(result) {
				responseHealthy = g.gradeResponsesResult(result, lease, &usage)
			}
			if candidate.Endpoint.Format != in.format {
				result, dropped, err = convert.ResponseWithOptions(result, candidate.Endpoint.Format, in.format, in.model, opts)
				if errors.Is(err, convert.ErrContinuationState) {
					lastDiagnostic = conversionFailure(err, "response_conversion")
					setUsageDiagnostic(&usage, lastDiagnostic)
					// The upstream returned a tool call together with reasoning
					// the client's format cannot hold. Stripping it would hand
					// back a tool call that can never be validly continued.
					// This is deterministic, not transient: asking the same
					// channel again — or any other channel — only spends money,
					// so neither happens and the request ends here.
					lastStatus = 400
					lastError = "upstream response carries reasoning that this protocol cannot preserve; no request was replayed"
					usage.Status = lastStatus
					usage.Error = lastError
					addAttempt(c, activeCred, model, lastStatus, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					g.Breaker.Release(lease)
					reasons = append(reasons, "conversion_refused")
					break candidates
				}
				if err != nil {
					lastDiagnostic = conversionFailure(err, "response_conversion")
					setUsageDiagnostic(&usage, lastDiagnostic)
					// Converting the answer is a capability question, not a
					// channel health one, so the breaker is released without a
					// failure: a channel that answered correctly must not cool
					// down over a feature this client's format cannot hold.
					// Nothing is resent either way — the request is committed
					// and the outcome deterministic.
					g.Breaker.Release(lease)
					lastStatus = 502
					lastError = "upstream response uses unsupported cross-protocol features; the upstream already processed this request, so to-router did not retry it"
					usage.Status = 502
					usage.Error = lastError
					addAttempt(c, activeCred, model, 502, callDuration, lastError, store.ActionTakenExhaustedFallback)
					usage.Attempts = attemptsList
					g.record(usage, settings, attemptStart)
					reasons = append(reasons, "conversion_refused")
					break candidates
				}
			} else if !json.Valid(result) {
				// A 2xx that is not even JSON is the channel's wire, not a
				// conversion question, so the breaker counts it.
				g.failRequest(r.Context(), lease)
				lastStatus = 502
				lastError = "upstream returned invalid JSON; the upstream already processed this request, so to-router did not retry it"
				usage.Status = 502
				usage.Error = lastError
				addAttempt(c, activeCred, model, 502, callDuration, lastError, store.ActionTakenExhaustedFallback)
				usage.Attempts = attemptsList
				g.record(usage, settings, attemptStart)
				reasons = append(reasons, "upstream_may_have_processed")
				break candidates
			}
			if len(dropped) > 0 {
				usage.Degraded = mergeDegradations(usage.Degraded, dropped)
				writeDegradationHeader(w, usage)
				slog.Warn("stripped reasoning from response", "channel", c.Name, "kinds", strings.Join(dropped, ","), "model", in.model)
			}
			if responseHealthy {
				if g.Breaker.Success(lease) {
					g.rememberPaths(pathScope, session, fingerprint, key)
					g.pin(session, fingerprint, domain{channel: c.ID, model: model, format: candidate.Endpoint.Format, generation: c.Generation, credential: activeCred.ID})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.status)
			_, writeErr := w.Write(result)
			if writeErr != nil {
				usage.Status = 499
				usage.Error = "client disconnected"
			}
			usage.TTFTMS = time.Since(attemptStart).Milliseconds()
			addAttempt(c, activeCred, model, usage.Status, callDuration, usage.Error, store.ActionTakenSuccess)
			usage.Attempts = attemptsList
			g.record(usage, settings, attemptStart)
			observed.jsonResult(usage, candidate.Endpoint.Format, originalResult)
			return
		}
	}
	if r.Context().Err() == nil {
		if lastError == disabledPluginReason {
			writeErrorEx(w, in.protocol, 503, lastError, disabledPluginRecovery())
			return
		}
		if lastError == unavailablePluginReason {
			writeErrorEx(w, in.protocol, 503, lastError, &recovery{Code: "connection_plugin_unavailable", Actions: []string{"install_compatible_runtime"}, Detail: "open #/plugins?category=connect to install or repair the compatibility layer; saved credentials are retained"})
			return
		}
		if attempts == 0 && lastStatus == 503 {
			observed.code = "all_channels_cooling"
		}
		if pinnedStrict && !gradedFailure(reasons) {
			// The candidates were restricted to the pinned domain, so every way
			// this loop can end without a success is that domain being
			// unreachable: the channel's breaker is open, its retries were
			// spent, or its wire is failing. The status that would travel
			// otherwise is the upstream's own (a 429, or a 502 the gateway
			// synthesized for a broken wire), and none of them tells the client
			// why the conversation cannot continue where it lives. The
			// pinned-unavailable recovery does, and its retry_later stays honest
			// for a channel that may recover. A failure that names itself —
			// the request may already have been processed, or the conversion
			// refuses it — outranks this and keeps its own status and recovery.
			lastStatus = 503
			lastError = "the channel that owns this conversation's state is unavailable; retry when it recovers, or hand off to a new session"
			reasons = append(reasons, "affinity_unavailable")
		}
		if configurationAdvice != "" {
			lastError += "; " + configurationAdvice
		}
		writeErrorEx(w, in.protocol, lastStatus, lastError, diagnosticRecovery(recoveryFor(reasons), lastDiagnostic))
	}
}

// gradedFailure reports whether the reasons already name the failure itself —
// the two grades that end an attempt because resending is unsafe or pointless —
// rather than the routing that led to it. Routing advice must not override
// those: a request the upstream may already have run is not one the client can
// simply retry later on the pinned channel.
func gradedFailure(reasons []string) bool {
	joined := strings.Join(reasons, ",")
	return strings.Contains(joined, "upstream_may_have_processed") || strings.Contains(joined, "conversion_refused")
}

// upstreamResponse is one finished upstream attempt.
type upstreamResponse struct {
	status int
	header http.Header
	body   io.ReadCloser
	err    error
}

// oauthEndpointAllowed reports whether an OAuth credential may be sent to this
// endpoint: the host must be on the credential type's allowlist and the scheme
// must be https. Both halves are checked again at runtime precisely because a
// database row can be edited behind the console's back; the diagnostic escape
// hatch (store.OAuthHostAllowed) suspends both, because a local double is plain
// HTTP on loopback by definition.
func oauthEndpointAllowed(credentialType, baseURL string) error {
	if !store.OAuthHostAllowed(credentialType, baseURL) {
		return fmt.Errorf("endpoint %s is not on the %s host allowlist; to-router will not send the OAuth credential there", baseURL, credentialType)
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && !store.EndpointRelaxedForDiagnostics(baseURL)) {
		return fmt.Errorf("endpoint %s is not https; to-router will not send the %s credential over a cleartext connection", baseURL, credentialType)
	}
	return nil
}

// call performs a single upstream request. Body bytes are re-read per attempt,
// so the same candidate can be retried without re-converting. A failure to
// build the request is the returned error: it belongs to the candidate
// declaration, not the wire. A failure on the wire rides in
// upstreamResponse.err, where the idempotency grading decides whether it may
// be resent.
func (g *Gateway) call(r *http.Request, in requestInfo, ep store.Endpoint, c store.Channel, model string, body []byte) (*upstreamResponse, error) {
	// An OAuth credential is resolved before the request is built, because it
	// decides where the request may go and with what header. A channel the
	// console never configured this way skips it entirely.
	credential := ""
	var runtime plugins.Runtime
	if c.CredentialType != "" && c.CredentialType != store.CredentialAPIKey {
		if err := oauthEndpointAllowed(c.CredentialType, ep.BaseURL); err != nil {
			return nil, err
		}
		if (c.CredentialType == store.CredentialChatGPTOAuth || c.CredentialType == store.CredentialCodexOAuth) && ep.Format != store.FormatOpenAIResponses {
			return nil, fmt.Errorf("the %s profile serves only the responses endpoint, not %s", c.CredentialType, ep.Format)
		}
		if g.Plugins == nil {
			return nil, plugins.ErrDisabled
		}
		resolved, ctx, err := g.Plugins.Resolve(r.Context(), c.CredentialType)
		if err != nil {
			return nil, err
		}
		runtime = resolved
		r = r.WithContext(ctx)
		token, err := runtime.Bearer(ctx, c)
		if err != nil {
			return nil, err
		}
		credential = token
	}
	req, err := upstreamRequest(r, in, ep, c, credential, model, body)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	if executor, ok := runtime.(plugins.Executor); ok {
		resp, err = executor.Execute(g.Client, req, c)
	} else {
		resp, err = accessguard.Do(g.Client, req)
	}
	if err != nil {
		if accessguard.Denied(err) || errors.Is(err, nativeoauth.ErrUnsupportedRequest) || errors.Is(err, pluginhost.ErrNotInstalled) {
			return nil, err
		}
		return &upstreamResponse{err: err}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized && c.CredentialType == store.CredentialCodexOAuth && len(c.Credentials) == 1 {
		if service, ok := runtime.(interface {
			RejectAccessOnly(store.Channel, string, string) error
		}); ok {
			// Only the exact rejected access-only bearer is marked. A rotation,
			// move or new consent racing this response makes the feedback stale.
			_ = service.RejectAccessOnly(c, c.Credentials[0].ID, credential)
		}
	}
	return &upstreamResponse{status: resp.StatusCode, header: resp.Header, body: resp.Body}, nil
}

// waitFor waits d and reports false when the client went away, in which case
// the attempt must stop.
func waitFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// parseRetryAfter honors upstream Retry-After without a local ceiling.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil {
		const maxSeconds = uint64((1<<63 - 1) / int64(time.Second))
		if secs > maxSeconds {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		return max(0, time.Until(when)), true
	}
	return 0, false
}

// errClass grades a failed attempt by whether the upstream may already have
// processed the request, which is what decides whether sending it again is
// safe.
type errClass int

const (
	// errTransient: the request never reached the upstream — DNS failure,
	// connection refused, dial timeout — so repeating it cannot duplicate
	// anything.
	errTransient errClass = iota
	// errAmbiguous: the connection was established and the request may have
	// been fully processed — read timeout, connection reset, write error — so
	// resending it here or on another channel could duplicate the work, the
	// cost and any side effect the upstream performed.
	errAmbiguous
)

// classifyTransportError grades a transport failure by whether the upstream may
// already have processed the request. Everything the HTTP client reports is
// wrapped in *url.Error, so it is the network-layer error underneath that
// decides, and errors.As/Is reach it without unwrapping by hand.
func classifyTransportError(err error) errClass {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errTransient
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "read", "write":
			// The connection was established when it broke down: the request
			// reached the upstream and may have been executed there.
			return errAmbiguous
		case "dial":
			// The connection never came up, so not one request byte was sent.
			// A dial timeout lands here too, and for the same reason: with no
			// connection to the upstream, nothing could have been processed.
			return errTransient
		}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return errTransient
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return errAmbiguous
	}
	// A deadline that expired on an established exchange, whether reported as
	// the raw sentinel or through the request context http.Client.Timeout sets.
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return errAmbiguous
	}
	// http.Client.Timeout surfaces as a *url.Error whose inner error only
	// answers Timeout(), with no sentinel to match.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errAmbiguous
	}
	// Unrecognized: the safe default is not to resend a request the upstream
	// may have processed.
	return errAmbiguous
}

// streamFailureCounts reports whether a failed streamed attempt counts against
// the channel's breaker. The upstream already ran the request, so nothing is
// resent either way; what the failure says about the channel is what differs. A
// capability the converter lacks, or a continuation state it refuses to break,
// is deterministic and belongs to the request, so a channel that answered
// correctly is released without a failure — the same grading the non-streaming
// committed branch gives those converter errors. A truncated, malformed or
// interrupted stream is the line itself and counts.
func streamFailureCounts(err error) bool {
	return !errors.Is(err, convert.ErrContinuationState) && !errors.Is(err, convert.ErrReasoningState) && !errors.Is(err, convert.ErrUnsupported)
}

// recovery explains why the gateway stopped early and what the caller can do
// about it. It rides along with the upstream's own status code so client
// backoff logic keeps working.
type recovery struct {
	Code       string                `json:"code"`
	Actions    []string              `json:"actions"`
	Detail     string                `json:"detail,omitempty"`
	Diagnostic *ConversionDiagnostic `json:"-"`
}

// recoveryFor explains why the gateway stopped short and what the caller can do
// about it. Reasons are independent of each other: a request can be stateful and
// have fallback turned off at the same time, and the advice must not claim that
// enabling fallback would fix a state problem. Two reasons outrank the rest:
// "upstream_may_have_processed" and "conversion_refused" describe the failure
// itself, while the others only describe the routing that led to it. A plain
// upstream error gets no recovery at all: "request_stateful" says only that the
// request happens to carry state, never that anything is wrong with its
// affinity, and state_affinity_conflict advice there would name a conflict
// nothing observed — a request the upstream merely refused is one the client
// reads as the upstream's own answer, not as a routing problem.
func recoveryFor(reasons []string) *recovery {
	if len(reasons) == 0 {
		return nil
	}
	joined := strings.Join(reasons, ",")
	stateful := strings.Contains(joined, "request_origin_bound")
	switch {
	case strings.Contains(joined, "upstream_may_have_processed"):
		return &recovery{Code: "upstream_may_have_processed", Actions: []string{"handoff", "start_fresh_session", "manual_retry_if_acceptable"}, Detail: "the upstream may have already processed this request; resending it, here or on another channel, could duplicate execution, so to-router left the decision to you; confirm for yourself whether the upstream already executed it before resending, and accept the duplicate-execution risk before a manual retry"}
	case strings.Contains(joined, "conversion_refused"):
		return &recovery{Code: "conversion_refused", Actions: []string{"handoff", "start_fresh_session", "use_different_format"}, Detail: "this response could not be converted completely; the upstream already processed the request, so the gateway did not replay it"}
	case strings.Contains(joined, "affinity_unavailable"):
		return &recovery{Code: "state_affinity_unavailable", Actions: []string{"retry_later", "handoff", "start_fresh_session"}, Detail: "the channel that owns this conversation's state is unavailable"}
	case strings.Contains(joined, "affinity_unknown"):
		return &recovery{Code: "state_affinity_unknown", Actions: []string{"use_single_binding", "handoff", "start_fresh_session"}, Detail: "state origin unknown; to-router does not guess which channel to send it to"}
	case strings.Contains(joined, "siwc_profile_stateless"):
		return siwcStatelessRecovery()
	case strings.Contains(joined, "fallback_disabled") && stateful:
		return &recovery{Code: "fallback_disabled", Actions: []string{"retry_later", "handoff"}, Detail: "fallback is off for this model and the request carries upstream-native state; enabling fallback would not help because another channel cannot validate that state"}
	case strings.Contains(joined, "fallback_disabled"):
		return &recovery{Code: "fallback_disabled", Actions: []string{"retry_later", "enable_fallback"}, Detail: "this downstream model has fallback turned off; only the first binding was tried"}
	}
	return nil
}

// outboundBody builds the body for one candidate endpoint, given as an API
// format. The same format passes through — an upstream model alias aside,
// which is an explicit rewrite, not a conversion — and a different format is
// converted. Deciding by format rather than by wire family is what keeps a Chat
// Completions request away from /responses: the two share the "openai" family
// but nothing else. Conversion here is safe by construction, because
// Candidates never offers a pair router.CanConvert refuses.
func outboundBody(in requestInfo, to string, model string, options ...convert.Options) ([]byte, []string, error) {
	var opts convert.Options
	if len(options) > 0 {
		opts = options[0]
	}
	if in.format != to {
		return convert.RequestWithOptions(in.body, in.format, to, model, opts)
	}
	body, dropped, err := convert.PrepareRequestWithOptions(in.body, in.format, opts)
	if err != nil {
		return nil, nil, err
	}
	if in.protocol == "gemini" || model == in.model {
		return body, dropped, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, err
	}
	obj["model"], _ = json.Marshal(model)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, err
	}
	return out, dropped, nil
}

// upstreamRequest builds one upstream request. credential carries an OAuth
// bearer token when the channel authenticates with one and is empty otherwise;
// that is the only difference between the two worlds. A failure to build the
// request belongs to the candidate declaration, not the wire.
func upstreamRequest(original *http.Request, in requestInfo, ep store.Endpoint, c store.Channel, credential string, model string, body []byte) (*http.Request, error) {
	base, err := url.Parse(ep.BaseURL)
	if err != nil {
		return nil, err
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, errors.New("unsupported scheme")
	}
	path := strings.TrimRight(base.Path, "/")
	if c.CredentialType == store.CredentialCopilotOAuth {
		// Copilot serves the two documented facades directly off the API base:
		// /chat/completions for the OpenAI shape and /v1/messages for the
		// Anthropic one — the OpenAI side takes no /v1 prefix here
		// (docs/protocol-refs/oauth-github-copilot.md, "对 Claude Code 的形态").
		switch ep.Format {
		case store.FormatOpenAIChatCompletions:
			path += "/chat/completions"
		case store.FormatAnthropicMessages:
			path += "/v1/messages"
		default:
			return nil, errors.New("copilot_oauth channel declares an unsupported endpoint format")
		}
	} else if c.CredentialType == store.CredentialCodexOAuth {
		// The backend is Responses-shaped at {base}/responses with no /v1
		// prefix (protocol ref, 调用面).
		if ep.Format != store.FormatOpenAIResponses {
			return nil, errors.New("the codex backend serves only the responses endpoint")
		}
		path += "/responses"
	} else {
		switch ep.Format {
		case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses:
			if !strings.HasSuffix(path, "/v1") {
				path += "/v1"
			}
			if ep.Format == store.FormatOpenAIResponses {
				path += "/responses"
			} else {
				path += "/chat/completions"
			}
		case store.FormatAnthropicMessages:
			if !strings.HasSuffix(path, "/v1") {
				path += "/v1"
			}
			path += "/messages"
		case store.FormatGeminiGenerateContent:
			if strings.ContainsAny(model, "/\\?#") {
				return nil, errors.New("invalid Gemini model")
			}
			if !strings.HasSuffix(path, "/v1beta") {
				path += "/v1beta"
			}
			path += "/models/" + model
			if in.stream {
				path += ":streamGenerateContent"
				base.RawQuery = "alt=sse"
			} else {
				path += ":generateContent"
			}
		default:
			return nil, errors.New("invalid endpoint format")
		}
	}
	base.Path = path
	base.RawPath = ""
	req, err := http.NewRequestWithContext(original.Context(), http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The SIWC profile answers with SSE whatever the client asked for, so its
	// Accept is SSE even for a non-streaming caller.
	if in.stream || c.CredentialType == store.CredentialChatGPTOAuth {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if credential != "" {
		// The credential type, not the endpoint format, decides the header set:
		// Copilot wants its client disguise, the first-party ChatGPT API wants
		// only the bearer, and the Codex backend wants the CLI's account and
		// originator pair.
		switch c.CredentialType {
		case store.CredentialCopilotOAuth:
			copilot.ApplyRequestHeaders(req.Header, credential)
		case store.CredentialChatGPTOAuth:
			siwc.ApplyRequestHeaders(req.Header, credential)
		case store.CredentialCodexOAuth:
			codex.ApplyRequestHeaders(req.Header, credential, c.OAuth.Account, codexSessionID(original, in))
		case store.CredentialClaudeOAuth:
			req.Header.Set("Authorization", "Bearer "+credential)
			if in.protocol == "anthropic" {
				if beta := strings.Join(original.Header.Values("Anthropic-Beta"), ","); beta != "" {
					req.Header.Set("Anthropic-Beta", beta)
				}
			}
			for _, name := range []string{"X-Claude-Code-Session-Id", "Session-Id"} {
				if value := original.Header.Get(name); value != "" {
					req.Header.Set(name, value)
				}
			}
		case store.CredentialDevinOAuth:
			req.Header.Set("Authorization", "Bearer "+credential)
			if session := original.Header.Get("X-Session-Id"); session != "" {
				req.Header.Set("X-Session-Id", session)
			}
		case store.CredentialZenFree:
			req.Header.Set("Authorization", "Bearer "+credential)
			for _, name := range []string{"X-Opencode-Session", "X-Opencode-Session-Id", "X-Session-Id", "Conversation-Id"} {
				if value := original.Header.Get(name); value != "" {
					req.Header.Set(name, value)
				}
			}
			if in.protocol == "anthropic" {
				if beta := original.Header.Get("anthropic-beta"); beta != "" {
					req.Header.Set("anthropic-beta", beta)
				}
			}
		default:
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		return req, nil
	}
	switch ep.Format {
	case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses:
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	case store.FormatAnthropicMessages:
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		version := original.Header.Get("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		req.Header.Set("anthropic-version", version)
		if in.protocol == "anthropic" {
			if beta := original.Header.Get("anthropic-beta"); beta != "" {
				req.Header.Set("anthropic-beta", beta)
			}
		}
	case store.FormatGeminiGenerateContent:
		req.Header.Set("x-goog-api-key", c.APIKey)
	}
	return req, nil
}

// statusGrade is the two-axis verdict on an upstream HTTP status. The axes are
// orthogonal: whether the request may be sent again, and what the answer says
// about the channel. Mixing them into one class was what graded a 507 as a
// capability signal — "do not retry" accidentally meaning "do not count either".
type statusGrade struct {
	replay replayDecision
	health healthImpact
}

// replayDecision grades whether a failed attempt may be sent again, which is
// the only thing that decides retries and failover. A POST inference request is
// the case the grading exists for: nothing in a non-2xx answer proves the model
// was never run, and a 5xx from a gateway upstream is where its own "already
// executed" timeouts land. The axis has a second producer — the transport
// grader, errTransient and errAmbiguous — whose failures count against the
// breaker either way, because the request never arrived or the wire broke
// mid-exchange.
type replayDecision int

const (
	// replayAllowed: to-router's policy choice to automatically repeat 429
	// and 503. This is an availability-oriented policy, NOT a proof that
	// execution never began — RFC 9110 forbids a proxy from retrying
	// non-idempotent requests on weaker evidence than this, and the residual
	// duplicate-execution risk is accepted deliberately (see DEBTS T018).
	// Retry-After on these statuses supplies the combination cooldown.
	replayAllowed replayDecision = iota
	// replayAmbiguous: the answer does not prove the request was not run — the
	// 500/502/504 family, 408 included. It is graded like the ambiguous
	// transport failures: one attempt, no resend, no failover, the client
	// decides. 408 keeps its own branch in ServeHTTP, because the upstream's own
	// status travels where a 5xx's does not.
	replayAmbiguous
	// replayDeterministic: the answer is about the request, about what the
	// upstream supports, or about a resource it has run out of — the 4xx
	// family, the 501/505 capability signals, the resource-exhaustion 5xx and
	// every other 5xx the grading does not name. Another attempt, here or
	// elsewhere, reproduces it exactly.
	replayDeterministic
)

// healthImpact grades what a failed attempt says about the channel, which is
// the only thing the breaker counts. Independent of replay: a capability the
// channel lacks is not the channel being unhealthy, so a channel that answered
// correctly stays selectable, while a throttled, ambiguous or exhausted one is
// what the gateway-level health signal is for.
type healthImpact int

const (
	healthFailure healthImpact = iota
	healthNeutral
)

// statusBandwidthLimitExceeded is the non-standard 509 an upstream out of
// bandwidth answers with (LiteSpeed-family servers; semantic inference, not
// verified against a live upstream — worst case it cools a channel once).
// net/http names no constant for it.
const statusBandwidthLimitExceeded = 509

// gradeStatus grades an HTTP status an upstream answer can carry along both
// axes. The pairings are the point: 429/503 are safe to repeat and count,
// 500/502/504/408 are neither repeatable nor harmless. Most 4xx and capability
// 5xx answer about the request; 401/402/403 instead mark an unavailable
// credential or exhausted balance. Those and resource-exhaustion 5xx count
// against health without granting additional automatic replay permission.
func gradeStatus(status int) statusGrade {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return statusGrade{replayAllowed, healthFailure}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusRequestTimeout:
		return statusGrade{replayAmbiguous, healthFailure}
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusInsufficientStorage, statusBandwidthLimitExceeded:
		// Credentials, balance, storage and bandwidth make this combination
		// unavailable. In particular a model-call 402 is balance exhaustion,
		// not a parameter error; it does not grant a new automatic resend.
		return statusGrade{replayDeterministic, healthFailure}
	}
	return statusGrade{replayDeterministic, healthNeutral}
}

// gradeHealth records what one failed attempt says about the channel's health:
// a failure counts against its breaker, a neutral outcome only clears a probe.
// Keeping the breaker half of the grade behind one call is what lets the replay
// half change without the health half following it.
func (g *Gateway) gradeHealth(lease health.Lease, impact healthImpact) {
	if impact == healthFailure {
		g.fail(lease)
		return
	}
	g.Breaker.Release(lease)
}

// A native JSON result and a Response assembled from the SIWC stream carry the
// same generation status. The intact result remains on the wire; this verdict
// only controls health, accounting and whether a completed turn learns a pin.
func (g *Gateway) gradeResponsesResult(result []byte, lease health.Lease, usage *store.Usage) bool {
	var shape struct {
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(result, &shape)
	switch shape.Status {
	case "incomplete":
		usage.Error = "upstream response incomplete: " + incompleteReason(result)
		g.Breaker.Release(lease)
		return false
	case "cancelled":
		usage.Error = "upstream response cancelled"
		g.Breaker.Release(lease)
		return false
	case "queued", "in_progress":
		// A background request was accepted but has not completed inference.
		g.Breaker.Release(lease)
		return false
	}
	if shape.Status == "failed" || (len(shape.Error) > 0 && string(shape.Error) != "null") {
		usage.Status = 502
		usage.Error = "upstream response failed"
		g.fail(lease)
		return false
	}
	return true
}

// familyOf maps an API format onto the wire-format family the converter works
// with: Responses and Chat Completions are both OpenAI on the wire, but they are
// different endpoints with different state semantics. Family values pass
// through so a caller holding either representation gets a usable answer.
func familyOf(format string) string {
	switch format {
	case "openai", "anthropic", "gemini":
		return format
	case store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses:
		return "openai"
	case store.FormatAnthropicMessages:
		return "anthropic"
	case store.FormatGeminiGenerateContent:
		return "gemini"
	}
	return ""
}

// pin remembers that this session and this conversation belong to the channel
// that just served them, so later turns follow instead of re-rolling the dice.
func (g *Gateway) pin(session, fingerprint string, d domain) {
	if session != "" {
		g.affinity.record(session, d)
	}
	if fingerprint != "" {
		g.affinity.record(fingerprint, d)
	}
}

// pinFirst reorders the candidates so the pinned continuation domain is tried
// first, keeping the relative order routing chose inside each group. A soft pin
// is a preference rather than a restriction: a request without state is
// portable, so the pinned domain leads and the rest follow. When that domain is
// not among the candidates at all — the endpoint world it names is gone, or the
// channel left — nothing is reordered, and the request routes as if unpinned.
func pinFirst(candidates []route.Attempt, d domain) []route.Attempt {
	out := make([]route.Attempt, 0, len(candidates))
	rest := []route.Attempt{}
	for _, a := range candidates {
		if a.Channel.ID == d.channel && a.Model == d.model && a.Endpoint.Format == d.format && a.Channel.Generation == d.generation {
			out = append(out, a)
			continue
		}
		rest = append(rest, a)
	}
	return append(out, rest...)
}

// restrictToDomain keeps only the candidates inside the pinned continuation
// domain. Every dimension of the domain has to match: a channel that kept only
// its Messages endpoint after a Chat Completions pin is not inside that pin's
// domain, even though it is the same channel, and neither is the same endpoint
// of a channel whose configuration generation moved on — the state the pin
// carries was produced by the configuration the pin was recorded against. An
// empty result means that domain is not available at all.
func restrictToDomain(candidates []route.Attempt, d domain) []route.Attempt {
	out := []route.Attempt{}
	for _, a := range candidates {
		if a.Channel.ID == d.channel && a.Model == d.model && a.Endpoint.Format == d.format && a.Channel.Generation == d.generation {
			out = append(out, a)
		}
	}
	return out
}

// reconfiguredChannel reports whether the pinned domain is gone because the
// channel that owns it was reconfigured rather than because the channel left:
// the id is still among the candidates, but the continuation domain it now
// serves is not the one that produced the state. The distinction decides the
// advice. A reconfigured channel never comes back — reverting the edit bumps the
// generation again — so telling the client to retry when it recovers would poll
// a 503 forever, while a channel that merely went away may return unchanged.
func reconfiguredChannel(candidates []route.Attempt, d domain) bool {
	for _, a := range candidates {
		if a.Channel.ID != d.channel {
			continue
		}
		if a.Model != d.model || a.Endpoint.Format != d.format || a.Channel.Generation != d.generation {
			return true
		}
	}
	return false
}

// distinctDomains counts the continuation domains among the candidates. Two
// bindings on the same channel and upstream model are one domain: routing
// between them cannot lose state. The endpoint format is part of the domain for
// the same reason the channel is.
func distinctDomains(candidates []route.Attempt) int {
	seen := map[string]bool{}
	for _, a := range candidates {
		seen[fmt.Sprintf("%d/%s/%s", a.Channel.ID, a.Endpoint.Format, a.Model)] = true
	}
	return len(seen)
}
func (g *Gateway) record(u store.Usage, s store.Settings, start time.Time) {
	completeUsageDiagnostic(&u)
	u.DurationMS = time.Since(start).Milliseconds()
	var actual, official store.PriceResolution
	if u.ActualPricing != nil && u.OfficialPricing != nil {
		actual, official = u.ActualPricing.Resolution, u.OfficialPricing.Resolution
	} else {
		items, err := g.Store.ListCatalogItems("", "")
		if err != nil {
			slog.Error("pricing catalog could not be read", "error", err)
		}
		actual, official = pricing.ResolveUsagePrices(s.Prices, items, u.ChannelID, u.UpstreamModel, u.Model)
	}
	a, o := pricing.Compute(actual, u, "request", ""), pricing.Compute(official, u, "request", "")
	u.ActualCost, u.OfficialValue = a.Amount, o.Amount
	u.ActualPricing, u.OfficialPricing = &a.Basis, &o.Basis
	u.Currency, u.OfficialCurrency = "", ""
	if a.Amount != nil {
		u.Currency = a.Basis.Currency
	}
	if o.Amount != nil {
		u.OfficialCurrency = o.Basis.Currency
	}
	if err := g.Store.AddUsage(u); err != nil {
		slog.Error("usage could not be saved", "error", err)
	}
}
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	models, err := g.Store.ListDownstreamModels()
	if err != nil {
		writeError(w, "openai", 500, "cannot read models")
		return
	}
	data := []map[string]any{}
	key, ok := auth.ClientFromContextOK(r.Context())
	if !ok {
		writeError(w, "openai", 401, "client API key required")
		return
	}
	scope, err := g.scopeSnapshot(key.Scope)
	if err != nil {
		writeError(w, "openai", 500, "cannot read access scope")
		return
	}
	channels, err := g.Store.ListChannels()
	if err != nil {
		writeError(w, "openai", 500, "cannot read channels")
		return
	}
	qualified := []store.DownstreamModel{}
	for _, model := range scope.filterModels(models, channels) {
		for _, format := range specFormats {
			if len(g.enabledPluginCandidates(route.Candidates(channels, []store.DownstreamModel{model}, model.ID, format))) > 0 {
				qualified = append(qualified, model)
				break
			}
		}
	}
	for _, id := range route.DownstreamEnabled(qualified) {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "to-router"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// writeError emits a protocol-shaped error. recovery, when present, tells the
// caller why the gateway stopped early and what to do about it; the status
// code stays the upstream's so client backoff logic keeps working.
func writeError(w http.ResponseWriter, protocol string, status int, message string) {
	writeErrorEx(w, protocol, status, message, nil)
}

func writeErrorEx(w http.ResponseWriter, protocol string, status int, message string, rec *recovery) {
	rec, message = exposeDiagnostic(w, rec, message)
	if observed, ok := w.(interface{ observeError(int, *recovery) }); ok {
		observed.observeError(status, rec)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var body any
	switch protocol {
	case "anthropic":
		e := map[string]any{"type": "api_error", "message": message}
		if rec != nil {
			e["recovery"] = rec
			if rec.Diagnostic != nil {
				e["diagnostic"] = rec.Diagnostic
			}
		}
		body = map[string]any{"type": "error", "error": e}
	case "gemini":
		e := map[string]any{"code": status, "status": "UNKNOWN", "message": message}
		if rec != nil {
			e["recovery"] = rec
			if rec.Diagnostic != nil {
				e["diagnostic"] = rec.Diagnostic
			}
		}
		body = map[string]any{"error": e}
	default:
		e := map[string]any{"type": "gateway_error", "message": message, "code": status}
		if rec != nil {
			e["recovery"] = rec
			if rec.Diagnostic != nil {
				e["diagnostic"] = rec.Diagnostic
			}
		}
		body = map[string]any{"error": e}
	}
	json.NewEncoder(w).Encode(body)
}
func streamError(w io.Writer, protocol string) {
	switch protocol {
	case "anthropic":
		fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Upstream stream interrupted\"}}\n\n")
	default:
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"Upstream stream interrupted\",\"type\":\"gateway_error\"}}\n\n")
	}
}

type flushWriter struct {
	http.ResponseWriter
	start time.Time
	usage *store.Usage
	wrote bool
}

func (w *flushWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.usage.TTFTMS = time.Since(w.start).Milliseconds()
		w.wrote = true
	}
	n, e := w.ResponseWriter.Write(b)
	w.Flush()
	return n, e
}
func (w *flushWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }
