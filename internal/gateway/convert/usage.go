package convert

// Usage extraction is intentionally tolerant of unrelated envelope fields,
// unlike conversion. It validates every token count it actually consumes.
//
// protocol is a wire family or a full endpoint format name; the formats fold
// onto the families here, except for Responses — the only family member with a
// usage schema of its own (T024).
func extractUsage(root object, protocol string, p *parser, strict bool) (irUsage, bool) {
	switch protocol {
	case Responses:
		protocol = OpenAIResponses
	case OpenAIChatCompletions:
		protocol = OpenAI
	case AnthropicMessages:
		protocol = Anthropic
	case GeminiGenerateContent:
		protocol = Gemini
	}
	var m object
	var raw any
	switch protocol {
	case OpenAI:
		raw = root["usage"]
	case OpenAIResponses:
		raw = root["usage"]
		if raw == nil {
			// A streaming response.completed event carries the whole response
			// object, so the usage nests one level below the event.
			if resp, ok := root["response"].(map[string]any); ok {
				raw = resp["usage"]
			}
		}
	case Anthropic:
		raw = root["usage"]
		if raw == nil {
			if msg, ok := root["message"].(map[string]any); ok {
				raw = msg["usage"]
			}
		}
	case Gemini:
		raw = root["usageMetadata"]
	}
	if raw == nil {
		return irUsage{}, false
	}
	var ok bool
	m, ok = raw.(map[string]any)
	if !ok {
		p.fail(malformed("usage must be object"))
		return irUsage{}, false
	}
	if strict {
		switch protocol {
		case OpenAI:
			p.obj(m, "prompt_tokens completion_tokens total_tokens prompt_tokens_details completion_tokens_details cached_tokens prompt_cache_hit_tokens prompt_cache_miss_tokens")
		case OpenAIResponses:
			p.obj(m, "input_tokens output_tokens total_tokens input_tokens_details output_tokens_details")
		case Anthropic:
			p.obj(m, "input_tokens output_tokens cache_read_input_tokens cache_creation_input_tokens")
		case Gemini:
			p.obj(m, "promptTokenCount candidatesTokenCount totalTokenCount cachedContentTokenCount thoughtsTokenCount")
		}
	}
	u := irUsage{}
	getFrom := func(source object, key string, dst *int64, present *bool) {
		if v, ok := source[key]; ok {
			n := p.integer(v)
			// Provider aliases describe the same counter, not extra tokens.
			if *present && *dst != n {
				p.fail(malformed("conflicting token counts for %s", key))
			}
			*dst = n
			*present = true
		}
	}
	get := func(key string, dst *int64, present *bool) { getFrom(m, key, dst, present) }
	switch protocol {
	case OpenAI:
		get("prompt_tokens", &u.Input, &u.HasInput)
		get("completion_tokens", &u.Output, &u.HasOutput)
		get("cached_tokens", &u.CacheRead, &u.HasCacheRead)           // Step
		get("prompt_cache_hit_tokens", &u.CacheRead, &u.HasCacheRead) // DeepSeek
		if v, ok := m["prompt_tokens_details"]; ok && v != nil {
			d, ok := v.(map[string]any)
			if !ok {
				p.fail(malformed("prompt details must be object"))
			} else {
				getFrom(d, "cached_tokens", &u.CacheRead, &u.HasCacheRead)
				getFrom(d, "cache_write_tokens", &u.CacheWrite, &u.HasCacheWrite)          // Kimi
				getFrom(d, "cache_creation_input_tokens", &u.CacheWrite, &u.HasCacheWrite) // Qwen
				// Qwen's creation metadata repeats the same 5m write counter,
				// not additional tokens (official Chat reference, 2026-10-11).
				if v, ok := d["cache_creation"]; ok && v != nil {
					creation, ok := v.(map[string]any)
					if !ok {
						p.fail(malformed("cache creation details must be object"))
					} else {
						if strict {
							p.obj(creation, "ephemeral_5m_input_tokens")
						}
						getFrom(creation, "ephemeral_5m_input_tokens", &u.CacheWrite, &u.HasCacheWrite)
					}
				}
				if strict {
					if v, ok := d["cache_type"]; ok && p.str(v) != "ephemeral" {
						p.fail(unsupportedf("cache type"))
					}
					for k, v := range d {
						if k != "cached_tokens" && k != "cache_write_tokens" && k != "cache_creation_input_tokens" && k != "cache_creation" && k != "cache_type" && p.integer(v) != 0 {
							p.fail(unsupportedf("prompt detail %s", k))
						}
					}
				}
			}
		}
		if v, ok := m["prompt_cache_miss_tokens"]; ok {
			// DeepSeek misses remain ordinary input; automatic caching does
			// not report a separately billed write counter.
			n := p.integer(v)
			if u.HasInput && u.HasCacheRead && (u.CacheRead > u.Input || n != u.Input-u.CacheRead) {
				p.fail(malformed("cache hit and miss tokens do not match input"))
			}
		}
		if strict {
			if d, ok := m["completion_tokens_details"].(map[string]any); ok {
				for k, v := range d {
					if p.integer(v) != 0 && k != "reasoning_tokens" {
						p.fail(unsupportedf("completion detail %s", k))
					}
				}
			}
		}
	// Responses input_tokens counts the whole prompt, including read and
	// explicit write tokens, like Chat prompt_tokens (provider contracts
	// verified 2026-10-11). Reasoning is a subset of output_tokens.
	case OpenAIResponses:
		get("input_tokens", &u.Input, &u.HasInput)
		get("output_tokens", &u.Output, &u.HasOutput)
		if v, ok := m["input_tokens_details"]; ok && v != nil {
			d, ok := v.(map[string]any)
			if !ok {
				p.fail(malformed("input token details must be object"))
			} else {
				if v, ok := d["cached_tokens"]; ok {
					u.CacheRead = p.integer(v)
					u.HasCacheRead = true
				}
				if v, ok := d["cache_write_tokens"]; ok {
					u.CacheWrite = p.integer(v)
					u.HasCacheWrite = true
				}
				if strict {
					for k, v := range d {
						if k != "cached_tokens" && k != "cache_write_tokens" && p.integer(v) != 0 {
							p.fail(unsupportedf("input detail %s", k))
						}
					}
				}
			}
		}
		if strict {
			if d, ok := m["output_tokens_details"].(map[string]any); ok {
				for k, v := range d {
					if p.integer(v) != 0 && k != "reasoning_tokens" {
						p.fail(unsupportedf("output detail %s", k))
					}
				}
			}
		}
	case Anthropic:
		get("input_tokens", &u.Input, &u.HasInput)
		get("output_tokens", &u.Output, &u.HasOutput)
		get("cache_read_input_tokens", &u.CacheRead, &u.HasCacheRead)
		get("cache_creation_input_tokens", &u.CacheWrite, &u.HasCacheWrite)
	case Gemini:
		get("promptTokenCount", &u.Input, &u.HasInput)
		get("candidatesTokenCount", &u.Output, &u.HasOutput)
		get("cachedContentTokenCount", &u.CacheRead, &u.HasCacheRead)
		if v, ok := m["thoughtsTokenCount"]; ok {
			n := p.integer(v)
			{
				u.Output += n
				u.HasOutput = true
			}
		}
	}
	if protocol != Anthropic && u.HasInput {
		if u.CacheRead > u.Input || u.CacheWrite > u.Input-u.CacheRead {
			p.fail(malformed("cached tokens exceed input"))
		} else {
			u.Input -= u.CacheRead + u.CacheWrite
		}
	}
	return u, u.HasInput || u.HasOutput || u.HasCacheRead || u.HasCacheWrite
}
func parseUsage(b []byte, protocol string) (int64, int64, int64, int64, bool) {
	p := &parser{}
	root := decodeRoot(b, p)
	u, ok := extractUsage(root, protocol, p, false)
	if p.err != nil {
		return 0, 0, 0, 0, false
	}
	return u.Input, u.Output, u.CacheRead, u.CacheWrite, ok
}
func parseOpenAIUsage(b []byte) (int64, int64, int64, int64, bool) { return parseUsage(b, OpenAI) }

// ReportedOutputTokens distinguishes a reported zero from an absent output
// count. A usage envelope containing only input tokens is not an output sample.
func ReportedOutputTokens(b []byte, protocol string) (int64, bool) {
	p := &parser{}
	u, _ := extractUsage(decodeRoot(b, p), protocol, p, false)
	return u.Output, p.err == nil && u.HasOutput
}
func parseResponsesUsage(b []byte) (int64, int64, int64, int64, bool) {
	return parseUsage(b, OpenAIResponses)
}
func parseAnthropicUsage(b []byte) (int64, int64, int64, int64, bool) {
	return parseUsage(b, Anthropic)
}
func parseGeminiUsage(b []byte) (int64, int64, int64, int64, bool) { return parseUsage(b, Gemini) }
func mergeUsage(dst *irUsage, u irUsage) {
	if u.HasInput {
		dst.Input = u.Input
		dst.HasInput = true
	}
	if u.HasOutput {
		dst.Output = u.Output
		dst.HasOutput = true
	}
	if u.HasCacheRead {
		dst.CacheRead = u.CacheRead
		dst.HasCacheRead = true
	}
	if u.HasCacheWrite {
		dst.CacheWrite = u.CacheWrite
		dst.HasCacheWrite = true
	}
}
func usageOut(u irUsage, to string) object {
	switch to {
	case Responses:
		return object{"input_tokens": u.Input + u.CacheRead + u.CacheWrite, "output_tokens": u.Output, "total_tokens": u.Input + u.CacheRead + u.CacheWrite + u.Output, "input_tokens_details": object{"cached_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite}}
	case OpenAI:
		details := object{"cached_tokens": u.CacheRead}
		if u.HasCacheWrite || u.CacheWrite > 0 {
			details["cache_write_tokens"] = u.CacheWrite
		}
		return object{"prompt_tokens": u.Input + u.CacheRead + u.CacheWrite, "completion_tokens": u.Output, "total_tokens": u.Input + u.CacheRead + u.CacheWrite + u.Output, "prompt_tokens_details": details}
	case Anthropic:
		return object{"input_tokens": u.Input, "output_tokens": u.Output, "cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite}
	case Gemini:
		return object{"promptTokenCount": u.Input + u.CacheRead, "candidatesTokenCount": u.Output, "totalTokenCount": u.Input + u.CacheRead + u.Output, "cachedContentTokenCount": u.CacheRead}
	}
	return object{}
}
