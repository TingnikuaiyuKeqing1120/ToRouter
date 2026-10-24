import { useState } from 'react';
import { Copy, ExternalLink } from 'lucide-react';
import type { UsageLog } from '../types';
import { Button } from './ui';

const sources: Record<string, string> = { gateway: '网关', local: '网关', upstream: '上游', transport: '传输' };
const phases: Record<string, string> = {
  request_conversion: '请求转换', request: '请求处理', conversion: '协议转换',
  upstream_request: '上游调用', upstream: '上游调用', upstream_response: '上游响应',
  response_conversion: '响应转换', response: '响应处理', stream: '流式响应',
  streaming: '流式响应', transport: '网络传输', routing: '路由选择',
};

export function RequestDiagnostic({ log }: { log: UsageLog }) {
  const diagnostic = log.diagnostic;
  const [copyState, setCopyState] = useState<'idle' | 'busy' | 'success' | 'error'>('idle');
  if (!diagnostic) return null;
  // Only local console routes are actionable. Never turn an arbitrary upstream URL into a settings link.
  const settingsPath = diagnostic.settings_path && /^#\/settings(?:\?|$)/.test(diagnostic.settings_path)
    ? diagnostic.settings_path : diagnostic.settings_path && /^\/settings(?:\?|$)/.test(diagnostic.settings_path)
      ? `#${diagnostic.settings_path}` : undefined;
  async function copy() {
    if (!diagnostic || copyState === 'busy') return;
    setCopyState('busy');
    try {
      // Explicit whitelist: request bodies, raw errors, credentials and opaque reasoning never enter this summary.
      await navigator.clipboard.writeText(JSON.stringify({
        request: { id: log.id, request_id: log.request_id || diagnostic.request_id, time: log.time, status: log.status, not_sent: log.not_sent, channel_id: log.channel_id, model: log.model, upstream_model: log.upstream_model, inbound: log.inbound, outbound: log.outbound },
        diagnostic: {
          code: diagnostic.code, source: diagnostic.source, phase: diagnostic.phase, message: diagnostic.message,
          facts: diagnostic.facts, possible_causes: diagnostic.possible_causes,
          steps: diagnostic.steps?.map(step => ({ action: step.action, expected: step.expected })),
          actions_taken: diagnostic.actions_taken, settings_path: diagnostic.settings_path, request_id: diagnostic.request_id,
        },
      }, null, 2));
      setCopyState('success');
    } catch { setCopyState('error'); }
  }
  const lists = [
    ['已确认', diagnostic.facts], ['可能原因', diagnostic.possible_causes], ['网关处理', diagnostic.actions_taken],
  ] as const;
  return <section aria-label="请求诊断" className="logs-diagnostic">
    <div className="logs-diagnostic-heading"><h3>请求诊断</h3><span>{sources[diagnostic.source] || '来源未明'} · {phases[diagnostic.phase] || '阶段未明'}</span></div>
    <p className="logs-diagnostic-message">{diagnostic.message || '原因尚不明确，请结合已记录的证据排查。'}</p>
    {lists.map(([title, items]) => !!items?.length && <div key={title} className="logs-diagnostic-block"><h4>{title}</h4><ul>{items.map((item, index) => <li key={index}>{item}</li>)}</ul></div>)}
    {!!diagnostic.steps?.length && <div className="logs-diagnostic-block"><h4>建议与验证</h4><ol>{diagnostic.steps.map((step, index) => <li key={index}>{step.action}{step.expected && <p className="logs-diagnostic-expected">结果判断：{step.expected}</p>}</li>)}</ol></div>}
    {(diagnostic.code || diagnostic.request_id) && <p className="logs-diagnostic-reference">{diagnostic.code && <span>诊断代码：{diagnostic.code}</span>}{diagnostic.request_id && <span>关联请求：{diagnostic.request_id}</span>}</p>}
    <div className="logs-diagnostic-actions">
      {settingsPath && <a className="btn btn-secondary" href={settingsPath}><ExternalLink size={15} />打开协议转换设置</a>}
      <Button variant="secondary" disabled={copyState === 'busy'} onClick={() => void copy()}><Copy size={15} />{copyState === 'busy' ? '正在复制…' : '复制排查信息'}</Button>
      {copyState === 'success' && <span role="status">排查信息已复制</span>}
      {copyState === 'error' && <span role="alert">复制失败，请允许剪贴板访问后重试。</span>}
    </div>
  </section>;
}
