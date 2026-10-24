import { api, ApiError } from './api';

interface Ready { service: string; installation_id: string; instance_id: string; port: number }
async function bounded<T>(request: (signal: AbortSignal) => Promise<T>, milliseconds: number, message: string): Promise<T> {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout>;
  const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => { reject(new Error(message)); controller.abort(); }, milliseconds); });
  try { return await Promise.race([request(controller.signal), timeout]); }
  finally { clearTimeout(timer!); }
}

async function readReady(origin: string, milliseconds = 1200): Promise<Ready> {
  return bounded(async signal => {
    const response = await fetch(`${origin}/_console/ready`, { credentials: 'omit', cache: 'no-store', redirect: 'error', signal });
    if (!response.ok) throw new Error('服务尚未恢复');
    const value = await response.json() as Ready;
    if (value.service !== 'to-router' || !/^[a-f0-9]{32}$/.test(value.installation_id) || !/^[a-f0-9]{32}$/.test(value.instance_id) || !Number.isInteger(value.port)) throw new Error('目标地址尚未提供有效控制台');
    return value;
  }, milliseconds, '服务就绪检测超时');
}

/** Observe a new process for this installation before replacing the old page.
 * A POST acknowledgement or an open port alone does not prove restart success. */
export async function restartConsole(onProgress: (message: string) => void): Promise<string> {
  const deadline = Date.now() + 60000;
  onProgress('正在确认重启地址…');
  const [connection, configured] = await bounded(signal => Promise.all([
    api<{ origin: string; port: number }>('/connection', { signal }),
    api<{ port: number }>('/runtime/port', { signal }),
  ]), 5000, '获取重启配置超时，请稍后重试。');
  const source = new URL(window.location.href);
  source.port = String(connection.port);
  const before = await readReady(source.origin);
  const target = new URL(source.origin);
  target.port = String(configured.port || before.port);
  target.hash = '/settings';
  try { await bounded(signal => api('/restart', { method: 'POST', body: '{}', signal }), 3000, '重启请求确认超时'); }
  catch (error) {
    if (error instanceof ApiError) throw error;
    // A lost acknowledgement can still mean the restart was accepted. The
    // installation/process comparison below resolves that uncertainty.
  }
  while (Date.now() < deadline) {
    onProgress(`服务正在重启，等待 ${target.origin} 恢复…`);
    try {
      const after = await readReady(target.origin, Math.min(1200, deadline - Date.now()));
      if (after.installation_id === before.installation_id && after.instance_id !== before.instance_id && after.port === Number(target.port || '80')) {
        onProgress('重启成功，正在打开新控制台…');
        return target.href;
      }
    } catch { /* Connection refusal is expected while the process restarts. */ }
    await new Promise(resolve => setTimeout(resolve, Math.min(700, Math.max(0, deadline - Date.now()))));
  }
  throw new Error(`60 秒内未确认重启成功。请检查 to-router.log 和端口占用；目标地址：${target.origin}。`);
}
