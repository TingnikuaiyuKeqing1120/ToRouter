package gateway

// ConversionDiagnostic is shared by protocol errors, request logs and management
// views. Raw upstream errors belong in the existing redacted detail channel;
// neither this constructor nor its templates copy request/error contents.
type ConversionDiagnostic struct {
	Code           string           `json:"code,omitempty"`
	Source         string           `json:"source,omitempty"`
	Phase          string           `json:"phase,omitempty"`
	Message        string           `json:"message,omitempty"`
	Facts          []string         `json:"facts,omitempty"`
	PossibleCauses []string         `json:"possible_causes,omitempty"`
	Steps          []DiagnosticStep `json:"steps,omitempty"`
	ActionsTaken   []string         `json:"actions_taken,omitempty"`
	SettingsPath   string           `json:"settings_path,omitempty"`
	RequestID      string           `json:"request_id,omitempty"`
}

type DiagnosticStep struct {
	Action   string `json:"action,omitempty"`
	Expected string `json:"expected,omitempty"`
}

// newConversionDiagnostic uses observed failure categories only. The caller
// selects the code after establishing its premise, and adds actual request ID,
// safe field categories, policy mode and actions taken. Unknown codes/phases
// are not echoed, since they could originate in an untrusted upstream error.
func newConversionDiagnostic(code, phase string) *ConversionDiagnostic {
	d := &ConversionDiagnostic{Code: code, Source: "gateway"}
	switch phase {
	case "request_conversion", "upstream_request", "upstream_response", "response_conversion", "stream", "transport", "routing":
		d.Phase = phase
	}
	switch code {
	case "reasoning_signature_bypass":
		d.Message = "本次外来工具历史没有原生 Gemini 签名，已使用该协议的旁路标记。"
		d.Facts = []string{"网关加入了 thought_signature_bypass 适配；这没有恢复原思考状态。"}
		d.Steps = []DiagnosticStep{{Action: "需要完整保留原生思考时，沿用可回传原始签名的客户端与目标协议。", Expected: "旁路标记只处理缺签名的外来轨迹，不保证与原生思考等价。"}}
	case "execution_unconfirmed":
		d.Source = "transport"
		d.Message = "上游可能已经处理了请求，但本次执行结果未能确认。网关没有自动重发，请先核对原请求。"
		d.Facts = []string{"本次失败不足以证明请求未执行。", "网关未因此自动重发或切换上游。"}
		d.Steps = []DiagnosticStep{
			{Action: "核对客户端已收到的结果、工具执行记录和上游请求记录。", Expected: "确认已发生的执行；缺少记录时不能推定没有执行。"},
			{Action: "确认重复执行的影响后，由调用方决定是否手动重试。", Expected: "避免重复工具操作与重复推理消耗。"},
		}
	case "conversion_invalid_payload":
		d.Message = "本次请求或上游响应的结构不完整，无法转换。请根据出错阶段检查必填内容与格式。"
		d.Facts = []string{"转换器发现缺失、错误或不一致的数据结构。"}
		d.Steps = []DiagnosticStep{{Action: "核对出错阶段：请求转换时检查客户端消息和工具关系；响应转换时检查目标原生响应。", Expected: "结构修正后才能继续判断兼容性；不能据此认定目标模型不支持请求。"}}
	case "reasoning_not_representable":
		d.Message = "目标协议无法完整承载这份思考状态。可使用上游原生协议，或在系统设置 → 协议转换调整处理策略。"
		d.Facts = []string{"网关已取得思考材料，但当前转换路径没有可用的完整回传方式。"}
		d.Steps = []DiagnosticStep{
			{Action: "使用目标上游的原生协议，并让客户端回传原始思考附件。", Expected: "绕开当前转换的承载限制；仍由上游判断原材料是否有效。"},
			{Action: "若接受丢弃旧思考，在系统设置 → 协议转换选择可用优先，再发起新请求。", Expected: "可表达的正文、工具调用和结果仍会保留；不会补回缺失历史。"},
		}
		d.SettingsPath = "#/settings?section=conversion"
	case "cache_hint_removed":
		d.Message = "目标协议不使用原缓存提示，网关已移除该提示。正文与历史保持，缓存命中由目标上游决定。"
		d.Facts = []string{"跨协议转换移除了 cache_control 提示。"}
		d.Steps = []DiagnosticStep{{Action: "需要保留原缓存断点时，使用相同上游的原生协议。", Expected: "原生路径会保留缓存控制；是否命中仍以上游报告为准。"}}
	case "conversion_unsupported":
		d.Message = "网关无法完整转换本次请求或响应中的内容。请查看受影响的字段，或使用目标原生协议。"
		d.Facts = []string{"本次路径遇到网关尚未实现的转换。"}
		d.Steps = []DiagnosticStep{
			{Action: "查看同一请求记录中的入站、出站格式和受影响字段。", Expected: "确认具体的本地转换边界；这不证明上游不支持该功能。"},
			{Action: "若客户端支持，改用目标原生协议；或由客户端移除不需要的功能后发起新请求。", Expected: "原生请求成功只验证该路径，不能证明原转换请求已修复。"},
		}
	case "reasoning_state_unavailable":
		d.Message = "本次续接所需的思考附件无法取得或唯一关联。请检查客户端回传的附件与调用关系。"
		d.Facts = []string{"网关没有可安全用于本次续接的必要思考附件。"}
		d.PossibleCauses = []string{"客户端可能未完整回传原始附件。", "本地暂存记录可能缺失或无法唯一匹配；尚未确认具体原因。"}
		d.Steps = []DiagnosticStep{
			{Action: "检查客户端是否回传原始附件及对应调用 ID，并核对原成功路径。", Expected: "取得并唯一关联原始附件后，才可判断续接是否恢复。"},
			{Action: "无法取得原附件时，由客户端保留可见内容并重新建立会话。", Expected: "切换思考处理模式也无法找回原工具 ID 或缺失历史；网关不会猜测关联。"},
		}
	case "context_unavailable":
		d.Message = "当前路径无法重建续接所需的上下文。请返回原路径，或让客户端提交完整历史。"
		d.Facts = []string{"本次请求依赖网关无法重建的历史上下文。"}
		d.Steps = []DiagnosticStep{
			{Action: "核对会话引用、压缩上下文或缓存引用，并尝试回到原账号与端点。", Expected: "原路径能继续时，支持问题与该上下文的可解析范围有关。"},
			{Action: "让客户端提交完整历史；如需新会话，先由客户端保存可见摘要。", Expected: "完整历史可用于重建请求；摘要迁移仍有信息损失，不能视为恢复原上下文。"},
		}
	case "tool_context_mismatch":
		d.Message = "本次完整历史中的工具调用与结果无法正确对应。请检查调用 ID 和工具返回。"
		d.Facts = []string{"已检查的完整历史存在工具调用与结果对应关系不满足的情况。"}
		d.Steps = []DiagnosticStep{
			{Action: "检查客户端完整历史中的调用 ID、结果 ID 与并行工具返回。", Expected: "每项工具结果能关联其原调用；仅有服务端历史引用时应先取得该历史。"},
			{Action: "需要重试前先核对工具执行记录。", Expected: "确认已有工具执行结果，避免把历史修复变成重复执行。"},
		}
	case "reasoning_state_rejected":
		d.Source = "upstream"
		d.Message = "上游拒绝了本次请求中的思考状态。可查看请求详情，或在系统设置 → 协议转换调整处理策略。"
		d.Facts = []string{"上游明确返回了思考材料或签名的兼容拒绝。"}
		d.PossibleCauses = []string{"这份思考状态可能与当前目标路径不兼容。", "客户端可能未完整回传原附件；当前证据不足以确认。"}
		d.Steps = []DiagnosticStep{
			{Action: "核对本次目标与上次成功请求的模型、账号及端点。", Expected: "切回后恢复，支持状态与目标路径兼容性有关，但不能证明密钥过期。"},
			{Action: "若接受丢弃旧思考，在系统设置 → 协议转换选择可用优先后发起新请求。", Expected: "只在安全条件满足时针对思考材料尝试一次；正文、工具和唯一历史仍须保留。"},
		}
		d.SettingsPath = "#/settings?section=conversion"
	case "upstream_rejected":
		d.Source = "upstream"
		d.Message = "上游拒绝了本次请求，当前证据不足以确认具体原因。请查看错误摘要与尝试记录。"
		d.Facts = []string{"已收到上游拒绝响应。"}
		d.PossibleCauses = []string{"原因尚不明确。"}
		d.Steps = []DiagnosticStep{
			{Action: "核对同一请求的状态码、错误摘要与实际目标；需要完整原因时查阅上游请求记录。", Expected: "区分参数、权限、额度及其他问题；单独状态码不能确定原因，网关不保存完整上游错误正文。"},
			{Action: "由调用方用同目标的最小原生请求检查基础接入，再逐步加入历史和工具。", Expected: "原生成功而转换失败，才支持继续检查转换路径；单次结果不能确定唯一原因。"},
		}
	case "stream_interrupted":
		d.Source = "transport"
		d.Message = "响应流未确认完整结束，执行结果可能未知。请先检查已收到的内容和工具执行记录，再决定是否重试。"
		d.Facts = []string{"未确认收到完整成功终态。"}
		d.Steps = []DiagnosticStep{
			{Action: "核对客户端已收到的答案、工具调用、工具执行记录及上游记录。", Expected: "确认已发生的输出与执行；缺少记录时仍不能推定没有执行。"},
			{Action: "确认重复执行的影响后，由调用方决定是否发起新请求。", Expected: "中断不会被当成成功，也不构成自动重放的依据。"},
		}
	case "reasoning_state_dropped":
		d.Message = "本次请求或响应已移除思考材料，可能影响推理连续性。具体移除范围见请求记录。"
		d.Facts = []string{"网关实际移除了思考材料。"}
		d.Steps = []DiagnosticStep{
			{Action: "查看实际移除的材料类别与该请求后续尝试结果。", Expected: "明确本次降级范围；调用成功不证明原思考状态已恢复。"},
			{Action: "若希望后续请求保留全部思考材料，在系统设置 → 协议转换选择稳定优先。", Expected: "设置只影响后续请求，不会恢复本次已移除的状态。"},
		}
		d.SettingsPath = "#/settings?section=conversion"
	default:
		d.Code = "conversion_failed"
		d.Message = "网关未能完成本次协议转换。请查看同一请求的诊断与受影响字段。"
		d.Facts = []string{"本次转换未能完成。"}
		d.Steps = []DiagnosticStep{{Action: "查看同一请求的转换阶段、格式与受影响字段。", Expected: "依据具体限制选择目标原生协议或调整客户端请求。"}}
	}
	return d
}
