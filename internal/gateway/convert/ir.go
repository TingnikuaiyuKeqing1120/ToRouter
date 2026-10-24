package convert

const (
	RoleSystem      = "system"
	RoleUser        = "user"
	RoleAssistant   = "assistant"
	RoleTool        = "tool"
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
	TCNone          = "none"
	TCAuto          = "auto"
	TCRequired      = "required"
	TCTool          = "tool"
)

type irToolCall struct{ ID, Name, Arguments string }
type irToolResult struct {
	ID, Name, Content string
	// Structured distinguishes serialized native JSON objects from text.
	// JSON-looking text must never be parsed just to guess its provenance.
	Structured bool
}
type irPart struct {
	// BridgePosition stores an attachment's original part position without
	// retaining the surrounding conversation. It is consumed on restoration.
	BridgePosition *int `json:",omitempty"`
	// Reasoning retains its native wire payload and provenance. It never enters
	// ordinary assistant text or masquerades as another provider's signature.
	Reasoning  *irReasoning
	Signature  string
	Text       string
	Tool       *irToolCall
	ToolResult *irToolResult
	// Thought marks a part that only carries reasoning (a Gemini thought part
	// or an Anthropic thinking block), separate from ordinary answer text.
	// Tool parts that merely carry a signature are not marked.
	Thought bool
}
type irReasoning struct {
	Format string `json:"format"`
	Value  object `json:"value"`
}
type irMessage struct {
	Role  string
	Parts []irPart
}
type irTool struct {
	Name, Description string
	Schema            map[string]any
}
type irToolChoice struct{ Mode, Name string }
type irRequest struct {
	System            string
	Messages          []irMessage
	Tools             []irTool
	ToolChoice        *irToolChoice
	Temperature, TopP *float64
	MaxTokens         *int64
	Stop              []string
	Stream            bool
}

// Input excludes both cache reads and writes. Presence flags allow stream
// usage deltas to update output without clearing previously received input.
type irUsage struct {
	Input, Output, CacheRead, CacheWrite             int64
	HasInput, HasOutput, HasCacheRead, HasCacheWrite bool
}
type irResponse struct {
	ID                   string
	Parts                []irPart
	Finish, StopSequence string
	Usage                irUsage
	HasUsage             bool
	// Dropped names reasoning carriers that were stripped because the target
	// format cannot express them and drop was explicitly authorized.
	Dropped []string
}
