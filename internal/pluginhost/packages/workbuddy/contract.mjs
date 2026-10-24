// WorkBuddy's reviewed boundary: reject unsupported semantics, never repair
// prompts, rename tools, merge schemas or lower tool constraints into text.
const object = v => v !== null && typeof v === 'object' && !Array.isArray(v);
let redactor = () => 'WorkBuddy request failed';
export function setSanitizer(fn) { redactor = fn; }
export function sanitize(value) { return redactor(String(value ?? 'WorkBuddy request failed')); }
function schema(s, root = false, depth = 0, booleanOK = false) {
  if (booleanOK && typeof s === 'boolean') return;
  if (!object(s) || depth > 64) throw new Error('WorkBuddy cannot preserve this tool schema');
  if (root && (s.type !== 'object' || ['anyOf', 'oneOf', 'allOf', '$ref'].some(k => k in s))) throw new Error('WorkBuddy requires an object tool schema root');
  const types = Array.isArray(s.type) ? s.type : s.type === undefined ? [] : [s.type];
  if (types.some(t => !['string', 'number', 'integer', 'boolean', 'object', 'array', 'null'].includes(t))) throw new Error('unsupported tool schema type');
  if ('enum' in s && (!Array.isArray(s.enum) || !s.enum.length || types.filter(t => t !== 'null').length !== 1)) throw new Error('WorkBuddy requires a typed nonempty tool enum');
  if ('required' in s && (!Array.isArray(s.required) || s.required.some(k => typeof k !== 'string'))) throw new Error('invalid tool required fields');
  if (Array.isArray(s.items) || typeof s.pattern === 'string' && /(?<!\\)\\0(?![0-9])/.test(s.pattern)) throw new Error('WorkBuddy cannot preserve tuple items or NUL patterns');
  for (const k of ['items', 'additionalItems', 'unevaluatedItems', 'contains', 'not', 'if', 'then', 'else', 'propertyNames', 'additionalProperties', 'unevaluatedProperties']) {
    if (k in s) schema(s[k], false, depth + 1, ['additionalProperties', 'unevaluatedProperties'].includes(k));
  }
  for (const k of ['anyOf', 'oneOf', 'allOf', 'prefixItems']) if (Array.isArray(s[k])) for (const sub of s[k]) schema(sub, false, depth + 1);
  for (const k of ['properties', 'patternProperties', '$defs', 'definitions', 'dependentSchemas']) if (object(s[k])) for (const sub of Object.values(s[k])) schema(sub, false, depth + 1);
}
export function validate(chat) {
  if (!object(chat) || typeof chat.model !== 'string' || !chat.model.trim() || !Array.isArray(chat.messages) || !chat.messages.length) throw new Error('model and messages are required');
  if (chat.stream !== true) throw new Error('WorkBuddy supports streamed chat completions only');
  if (chat.max_completion_tokens != null) throw new Error('WorkBuddy cannot preserve max_completion_tokens; use max_tokens');
  if (chat.tool_choice != null && !['auto', 'none'].includes(chat.tool_choice)) throw new Error('WorkBuddy cannot enforce the selected tool_choice');
  for (const m of chat.messages) {
    if (!object(m) || !['system', 'user', 'assistant', 'tool'].includes(m.role)) throw new Error('WorkBuddy cannot preserve this message role');
  }
  if (chat.tools != null && !Array.isArray(chat.tools)) throw new Error('tools must be an array');
  for (const t of chat.tools ?? []) {
    if (t?.type !== 'function' || !object(t.function) || !/^[A-Za-z0-9_-]+$/.test(t.function.name)) throw new Error('WorkBuddy cannot preserve this tool name or type');
    if (t.function.strict != null && t.function.strict !== false) throw new Error('WorkBuddy cannot enforce strict tool schemas');
    if (t.function.parameters !== undefined) schema(t.function.parameters, true);
  }
}
