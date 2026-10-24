package router

import (
	"slices"
	"sort"
	"torouter/internal/store"
)

// BindingStructureReason describes static routing eligibility, independently of
// cooldown, quota and credential health. An empty reason means eligible.
func BindingStructureReason(c store.Channel, b store.Binding) string {
	if !b.Enabled {
		return "绑定未启用"
	}
	if !c.Enabled {
		return "渠道未启用"
	}
	if !slices.Contains(c.Models, b.Model) && !slices.Contains(c.AvailableModels, b.Model) {
		return "上游模型不存在"
	}
	for _, format := range []string{store.FormatOpenAIChatCompletions, store.FormatOpenAIResponses, store.FormatAnthropicMessages, store.FormatGeminiGenerateContent} {
		if _, ok := PickModelEndpoint(c, b.Model, format); ok {
			return ""
		}
	}
	return "没有可用端点"
}

// Attempt is one ordered routing candidate: a channel plus the concrete
// upstream model and the endpoint that will serve it.
type Attempt struct {
	Channel  store.Channel
	Endpoint store.Endpoint
	Model    string
}

// convertibleFormats lists the formats that convert pairwise, in the fixed
// portable order used when a channel declares several endpoints and none
// matches the inbound format exactly. The endpoint array's own order is display
// order and carries no routing meaning. Responses follows the existing three
// formats to preserve their relative preference.
var convertibleFormats = []string{
	store.FormatOpenAIChatCompletions,
	store.FormatAnthropicMessages,
	store.FormatGeminiGenerateContent,
	store.FormatOpenAIResponses,
}

// CanConvert reports whether a request in format `from` can be served by an
// endpoint in format `to`. All four known formats convert pairwise, while an
// exact known format needs no conversion. Specific content and continuation
// limitations are checked by the gateway/converter for the selected path.
func CanConvert(from, to string) bool {
	return isConvertible(from) && isConvertible(to)
}

// isConvertible reports whether a format is one of the four that convert
// pairwise. An unknown format is not.
func isConvertible(format string) bool {
	for _, f := range convertibleFormats {
		if f == format {
			return true
		}
	}
	return false
}

// PickEndpoint chooses which of a channel's endpoints serves an inbound
// format, in three steps:
//
//  1. an endpoint whose format matches the inbound format exactly;
//  2. the endpoint the channel explicitly prefers (PreferredEndpoint), when it
//     can serve this format;
//  3. the fixed portable order in convertibleFormats.
//
// A channel with no usable endpoint yields none, so the binding produces no
// candidate at all instead of failing on the wire.
//
// The steps are a routing preference, not a continuation identity: a caller
// holding a pin resolves the pinned format instead, because the endpoint that
// already serves the conversation outranks what the channel prefers now.
func PickEndpoint(c store.Channel, format string) (store.Endpoint, bool) {
	if !isConvertible(format) {
		return store.Endpoint{}, false
	}
	if ep, ok := c.EndpointFor(format); ok {
		return ep, true
	}
	if c.PreferredEndpoint != "" {
		if ep, ok := c.EndpointFor(c.PreferredEndpoint); ok && CanConvert(format, ep.Format) {
			return ep, true
		}
	}
	for _, f := range convertibleFormats {
		if ep, ok := c.EndpointFor(f); ok {
			return ep, true
		}
	}
	return store.Endpoint{}, false
}

// PickModelEndpoint applies a provider's exact catalogue before the ordinary
// native/preferred/portable preference. It never guesses a newly listed model's
// protocol and does not do hidden conversion inside a provider driver.
func PickModelEndpoint(c store.Channel, model, format string) (store.Endpoint, bool) {
	allowed := c.ModelOutboundFormats(model)
	endpoints := make([]store.Endpoint, 0, len(allowed))
	for _, ep := range c.Endpoints {
		if slices.Contains(allowed, ep.Format) {
			endpoints = append(endpoints, ep)
		}
	}
	c.Endpoints = endpoints
	return PickEndpoint(c, format)
}

// Candidates resolves a downstream model into ordered attempts for one API
// format. Bindings are grouped by priority (ascending); ties retain their
// saved list order. Disabled bindings, disabled channels and
// disabled downstream models are excluded. A binding whose channel declares no
// endpoint that can serve the format produces no attempt: the format matrix,
// not the wire, decides whether a channel is a candidate at all.
func Candidates(channels []store.Channel, models []store.DownstreamModel, model, format string) []Attempt {
	byID := map[int64]store.Channel{}
	for _, c := range channels {
		byID[c.ID] = c
	}
	groups := map[int][]Attempt{}
	priorities := []int{}
	for _, m := range models {
		if m.ID != model || !m.Enabled {
			continue
		}
		for _, b := range m.Bindings {
			if !b.Enabled {
				continue
			}
			c, ok := byID[b.ChannelID]
			if !ok || BindingStructureReason(c, b) != "" {
				continue
			}
			ep, ok := PickModelEndpoint(c, b.Model, format)
			if !ok {
				continue
			}
			if _, seen := groups[b.Priority]; !seen {
				priorities = append(priorities, b.Priority)
			}
			groups[b.Priority] = append(groups[b.Priority], Attempt{Channel: c, Endpoint: ep, Model: b.Model})
		}
		break
	}
	sort.Ints(priorities)
	out := []Attempt{}
	for _, p := range priorities {
		out = append(out, groups[p]...)
	}
	return out
}

// DownstreamEnabled lists public model IDs for /v1/models.
func DownstreamEnabled(models []store.DownstreamModel) []string {
	out := []string{}
	for _, m := range models {
		if m.Enabled {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// FallbackEnabled reports whether a downstream model allows trying further
// bindings after the first one fails. Unknown models default to enabled so a
// missing row never silently disables failover.
func FallbackEnabled(models []store.DownstreamModel, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return m.Fallback
		}
	}
	return true
}
