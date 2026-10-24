// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { restartConsole } from './restart';

const install = 'a'.repeat(32), previous = 'b'.repeat(32), next = 'c'.repeat(32);
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status });
beforeEach(() => vi.useFakeTimers());
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

function mockServer(ready: () => Response, restartStatus = 200) {
  const fetcher = vi.fn(async (input: string | URL | Request) => {
    const url = String(input);
    if (url === '/api/connection') return json({ port: 17878, origin: 'http://127.0.0.1:17878' });
    if (url === '/api/runtime/port') return json({ port: 19000 });
    if (url === '/api/restart') return json(restartStatus === 200 ? { ok: true } : { error: { message: 'rejected' } }, restartStatus);
    if (url.includes(':17878/_console/ready')) return json({ service: 'to-router', installation_id: install, instance_id: previous, port: 17878 });
    if (url.includes(':19000/_console/ready')) return ready();
    throw new Error('unexpected fetch '+url);
  });
  vi.stubGlobal('fetch', fetcher);
  return fetcher;
}

describe('restart console recovery', () => {
  it('waits for the same installation and a new process before opening the saved port', async () => {
    let count = 0;
    const fetcher = mockServer(() => {
      count++;
      return json({ service: 'to-router', installation_id: count === 2 ? 'd'.repeat(32) : install, instance_id: count === 1 ? previous : next, port: 19000 });
    });
    const progress = vi.fn();
    const result = restartConsole(progress);
    await vi.runAllTimersAsync();
    expect(await result).toBe(`http://${window.location.hostname}:19000/#/settings`);
    expect(count).toBe(3);
    expect(fetcher.mock.calls.filter(([url]) => url === '/api/restart')).toHaveLength(1);
    expect(progress).toHaveBeenLastCalledWith('重启成功，正在打开新控制台…');
  });

  it('reports a bounded failure instead of leaving the page stuck or navigating to another app', async () => {
    mockServer(() => json({ service: 'to-router', installation_id: 'd'.repeat(32), instance_id: next, port: 19000 }));
    const assertion = expect(restartConsole(() => {})).rejects.toThrow('60 秒内未确认重启成功');
    await vi.runAllTimersAsync();
    await assertion;
  });

  it('does not claim readiness when the management request is refused', async () => {
    const fetcher = mockServer(() => { throw new Error('should not poll'); }, 403);
    await expect(restartConsole(() => {})).rejects.toThrow('rejected');
    expect(fetcher.mock.calls.some(([url]) => String(url).includes(':19000'))).toBe(false);
  });

  it('continues observing a successful restart when its acknowledgement hangs', async () => {
    const fetcher = mockServer(() => json({ service: 'to-router', installation_id: install, instance_id: next, port: 19000 }));
    const normal = fetcher.getMockImplementation()!;
    fetcher.mockImplementation(input => String(input) === '/api/restart' ? new Promise<Response>(() => {}) : normal(input));
    const result = restartConsole(() => {});
    await vi.runAllTimersAsync();
    expect(await result).toBe(`http://${window.location.hostname}:19000/#/settings`);
    expect(fetcher.mock.calls.some(([url]) => String(url).includes(':19000'))).toBe(true);
  });

  it('bounds preflight requests and never sends a restart with unknown configuration', async () => {
    const fetcher = vi.fn(() => new Promise<Response>(() => {}));
    vi.stubGlobal('fetch', fetcher);
    const assertion = expect(restartConsole(() => {})).rejects.toThrow('获取重启配置超时');
    await vi.runAllTimersAsync();
    await assertion;
    expect(fetcher.mock.calls).toHaveLength(2);
    expect(vi.getTimerCount()).toBe(0);
  });
});
