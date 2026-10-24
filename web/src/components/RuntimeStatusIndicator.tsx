import { useRuntimeStatus } from '../hooks/useRuntimeStatus';

export function RuntimeStatusIndicator() {
  const { data, error } = useRuntimeStatus();
  if (!data || error) return null;
  return data.problem_count > 0
    ? <span className="runtime-nav-count" aria-label={`${data.problem_count} 个异常凭据`}>{data.problem_count}</span>
    : <span className="runtime-nav-dot" aria-label="未发现异常" title="未发现异常" />;
}
