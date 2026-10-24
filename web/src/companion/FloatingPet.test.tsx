// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import FloatingPet from './FloatingPet';
import type { CompanionSnapshot, FinishedRequest, LiveRequest } from './live';

const mockRequest = (overrides: Partial<LiveRequest> = {}): LiveRequest => ({
  id: 'r1',
  model: 'deepseek-chat',
  channel: 'personal-channel',
  channel_id: 1,
  started_at: '2026-10-06T09:00:00Z',
  attempt: 1,
  phase: 'streaming',
  output_tokens: 150,
  output_tokens_source: '',
  tokens_per_second: 68.2,
  first_byte_ms: 180,
  ...overrides,
});

const mockFinished = (overrides: Partial<FinishedRequest> = {}): FinishedRequest => ({
  ...mockRequest(),
  sequence: 1,
  finished_at: '2026-10-06T09:00:05Z',
  outcome: 'success',
  status: 200,
  error_code: '',
  ...overrides,
});

const mockSnapshot = (overrides: Partial<CompanionSnapshot> = {}): CompanionSnapshot => ({
  server_time: '2026-10-06T09:00:00Z',
  session_id: 'test-session',
  sequence: 1,
  active_count: 0,
  active: [],
  recent: [],
  today: {
    records: 12,
    input_tokens: 5432,
    output_tokens: 1890,
    cache_read_tokens: 500,
    cache_write_tokens: 100,
    estimated_records: 2,
  },
  ...overrides,
});

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

beforeEach(() => {
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
  vi.stubGlobal('requestAnimationFrame', vi.fn(() => 1));
  vi.stubGlobal('cancelAnimationFrame', vi.fn());
  vi.stubGlobal('open', vi.fn());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('FloatingPet component', () => {
  it('renders transparent container with wails drag attributes and 0 concurrency standby state', async () => {
    const live = {
      snapshot: mockSnapshot({ active_count: 0, active: [] }),
      connection: 'connected' as const,
      notice: null,
      scene: 'idle' as const,
    };

    const { container } = render(<FloatingPet live={live} theme="dark" />);

    // Container is transparent and has draggable attributes
    const root = container.firstElementChild as HTMLElement;
    expect(root.className).toContain('bg-transparent');
    expect(root.getAttribute('data-wails-draggable')).toBe('drag');

    // Mascot button has no drag
    const stageBtn = screen.getByRole('button', { name: '摸摸蓝鲸和 Clawd' });
    expect(stageBtn).toBeTruthy();

    // In 0 concurrency, head speed pills are not shown
    expect(screen.queryByLabelText('实时并发梯队')).toBeNull();
    expect(screen.queryByLabelText('聚合并发药丸')).toBeNull();

    // On hover, standby status badge is revealed
    fireEvent.mouseEnter(root);
    const statusBadge = screen.getByRole('status', { name: '待机状态' });
    expect(statusBadge.textContent).toContain('待机打盹中 · 准备就绪');
  });

  it('renders done庆祝态 when 0 concurrency has a finished request notice', () => {
    const live = {
      snapshot: mockSnapshot({ active_count: 0, active: [] }),
      connection: 'connected' as const,
      notice: {
        scene: 'done' as const,
        request: mockFinished({ outcome: 'success' }),
        until: Date.now() + 2000,
      },
      scene: 'done' as const,
    };

    const { container } = render(<FloatingPet live={live} theme="dark" />);
    const root = container.firstElementChild as HTMLElement;
    fireEvent.mouseEnter(root);

    const statusBadge = screen.getByRole('status', { name: '待机状态' });
    expect(statusBadge.textContent).toContain('本次任务已完成');
  });

  it('renders 1 concurrency vertical luminous pill with exact ID, model name and speed', () => {
    const req1 = mockRequest({
      id: 'req-1',
      model: 'deepseek-chat',
      tokens_per_second: 68.2,
      phase: 'streaming',
    });

    const live = {
      snapshot: mockSnapshot({ active_count: 1, active: [req1] }),
      connection: 'connected' as const,
      notice: null,
      scene: 'work' as const,
    };

    render(<FloatingPet live={live} theme="dark" />);

    const tier = screen.getByLabelText('实时并发梯队');
    expect(tier).toBeTruthy();
    expect(tier.textContent).toContain('#1');
    expect(tier.textContent).toContain('deepseek-chat');
    expect(tier.textContent).toContain('68.2 tok/s');
    expect(screen.queryByLabelText('聚合并发药丸')).toBeNull();
  });

  it('renders 2~3 concurrency vertical luminous pills with estimated ≈ prefix and waiting state', () => {
    const req1 = mockRequest({
      id: 'req-1',
      model: 'deepseek-chat',
      tokens_per_second: 72.4,
      phase: 'streaming',
    });
    const req2 = mockRequest({
      id: 'req-2',
      model: 'claude-3-5-sonnet',
      tokens_per_second: 45.0,
      output_tokens_source: 'estimated',
      phase: 'streaming',
    });
    const req3 = mockRequest({
      id: 'req-3',
      model: 'gpt-4o',
      tokens_per_second: null,
      phase: 'waiting',
    });

    const live = {
      snapshot: mockSnapshot({ active_count: 3, active: [req1, req2, req3] }),
      connection: 'connected' as const,
      notice: null,
      scene: 'surf' as const,
    };

    render(<FloatingPet live={live} theme="dark" />);

    const tier = screen.getByLabelText('实时并发梯队');
    expect(tier).toBeTruthy();

    // Check #1
    expect(tier.textContent).toContain('#1');
    expect(tier.textContent).toContain('deepseek-chat');
    expect(tier.textContent).toContain('72.4 tok/s');

    // Check #2 with estimated ≈ prefix
    expect(tier.textContent).toContain('#2');
    expect(tier.textContent).toContain('claude-3-5-sonnet');
    expect(tier.textContent).toContain('≈45.0 tok/s');

    // Check #3 with waiting state
    expect(tier.textContent).toContain('#3');
    expect(tier.textContent).toContain('gpt-4o');
    expect(tier.textContent).toContain('等待首包');
  });

  it('collapses into single aggregate pill for 4+ concurrency and expands drawer on hover or click', () => {
    const reqs: LiveRequest[] = [
      mockRequest({ id: 'r1', model: 'deepseek-chat', tokens_per_second: 85.5, first_byte_ms: 120 }),
      mockRequest({ id: 'r2', model: 'claude-3-5-sonnet', tokens_per_second: 50.2, first_byte_ms: 210 }),
      mockRequest({ id: 'r3', model: 'gpt-4o', tokens_per_second: 60.0, first_byte_ms: 300 }),
      mockRequest({ id: 'r4', model: 'qwen-plus', tokens_per_second: 40.8, first_byte_ms: 150 }),
    ];

    const live = {
      snapshot: mockSnapshot({ active_count: 4, active: reqs }),
      connection: 'connected' as const,
      notice: null,
      scene: 'surf' as const,
    };

    render(<FloatingPet live={live} theme="dark" />);

    // Individual tier is hidden, aggregate pill is displayed
    expect(screen.queryByLabelText('实时并发梯队')).toBeNull();
    const aggButton = screen.getByRole('button', { name: '聚合并发药丸' });
    expect(aggButton).toBeTruthy();
    expect(aggButton.textContent).toContain('🔥 4 并发在飞 · 峰值 85.5 tok/s');

    // Drawer is closed initially
    expect(screen.queryByLabelText('并发通道详情抽屉')).toBeNull();

    // Hover opens the drawer
    fireEvent.mouseEnter(aggButton);
    const drawer = screen.getByLabelText('并发通道详情抽屉');
    expect(drawer).toBeTruthy();
    expect(drawer.textContent).toContain('全部并发通道 (4)');
    expect(drawer.textContent).toContain('#1');
    expect(drawer.textContent).toContain('deepseek-chat');
    expect(drawer.textContent).toContain('85.5 tok/s');
    expect(drawer.textContent).toContain('120ms');

    expect(drawer.textContent).toContain('#4');
    expect(drawer.textContent).toContain('qwen-plus');
    expect(drawer.textContent).toContain('40.8 tok/s');
    expect(drawer.textContent).toContain('150ms');

    // Leave aggregate pill closes drawer
    fireEvent.mouseLeave(drawer);
    expect(screen.queryByLabelText('并发通道详情抽屉')).toBeNull();

    // Click toggles the drawer
    fireEvent.click(aggButton);
    expect(screen.getByLabelText('并发通道详情抽屉')).toBeTruthy();
    fireEvent.click(aggButton);
    expect(screen.queryByLabelText('并发通道详情抽屉')).toBeNull();
  });

  it('toggles L2 micro control dock on clicking mascot or quick button with usage stats and controls', () => {
    const toggleThemeMock = vi.fn();
    const live = {
      snapshot: mockSnapshot({
        active_count: 0,
        active: [],
        today: {
          records: 28,
          input_tokens: 12500,
          output_tokens: 4320,
          cache_read_tokens: 1000,
          cache_write_tokens: 200,
          estimated_records: 3,
        },
      }),
      connection: 'connected' as const,
      notice: null,
      scene: 'idle' as const,
    };

    render(<FloatingPet live={live} theme="dark" toggleTheme={toggleThemeMock} />);

    // Dock is initially closed
    expect(screen.queryByLabelText('极简控制坞')).toBeNull();

    // Click mascot triggers onGreet and opens dock
    const stageBtn = screen.getByRole('button', { name: '摸摸蓝鲸和 Clawd' });
    fireEvent.click(stageBtn);

    const dock = screen.getByLabelText('极简控制坞');
    expect(dock).toBeTruthy();
    expect(dock.textContent).toContain('to-router 伴侣控制坞');
    expect(dock.textContent).toContain('今日调用概览');
    expect(dock.textContent).toContain('28'); // records
    expect(dock.textContent).toContain('12,500'); // formatted input tokens
    expect(dock.textContent).toContain('4,320'); // formatted output tokens
    expect(dock.textContent).toContain('3 条调用含估算数据');

    // Theme toggle button
    const themeBtn = screen.getByRole('button', { name: '切换主题' });
    fireEvent.click(themeBtn);
    expect(toggleThemeMock).toHaveBeenCalledTimes(1);

    // Pause/Play animation button
    const pauseBtn = screen.getByRole('button', { name: '暂停动画' });
    expect(pauseBtn.getAttribute('aria-pressed')).toBe('false');
    fireEvent.click(pauseBtn);
    expect(screen.getByRole('button', { name: '恢复动画' }).getAttribute('aria-pressed')).toBe('true');

    // Console button triggers desktop launch without navigating away pet hash
    const consoleBtn = screen.getByRole('button', { name: '返回控制台' });
    expect(consoleBtn).toBeTruthy();
    fireEvent.click(consoleBtn);

    // Close button
    const closeBtn = screen.getByRole('button', { name: '关闭控制坞' });
    fireEvent.click(closeBtn);
    expect(screen.queryByLabelText('极简控制坞')).toBeNull();

    // Quick dock trigger button opens dock as well
    const quickTrigger = screen.getByRole('button', { name: '打开控制坞' });
    fireEvent.click(quickTrigger);
    expect(screen.getByLabelText('极简控制坞')).toBeTruthy();
  });

  it('renders dock with light theme styles when theme="light"', () => {
    const live = {
      snapshot: mockSnapshot({ active_count: 0, active: [] }),
      connection: 'connected' as const,
      notice: null,
      scene: 'idle' as const,
    };

    render(<FloatingPet live={live} theme="light" />);

    // Open dock
    const stageBtn = screen.getByRole('button', { name: '摸摸蓝鲸和 Clawd' });
    fireEvent.click(stageBtn);

    const dock = screen.getByLabelText('极简控制坞');
    expect(dock.className).toContain('text-slate-800');
    expect(dock.className).toContain('bg-white/95');
  });

  it('integrates with live telemetry API by default when live prop is omitted', async () => {
    const data = mockSnapshot({
      active_count: 2,
      active: [
        mockRequest({ id: 'live-1', model: 'api-model-1', tokens_per_second: 99.1 }),
        mockRequest({ id: 'live-2', model: 'api-model-2', tokens_per_second: 88.2 }),
      ],
    });

    const fetcher = vi.fn().mockImplementation((url: string) => {
      if (url === '/api/companion') return Promise.resolve(jsonResponse(data));
      return Promise.resolve(jsonResponse({}, 404));
    });
    vi.stubGlobal('fetch', fetcher);

    render(<FloatingPet theme="dark" />);

    // Initially loading
    expect(screen.getByRole('status', { name: '待机状态' }).textContent).toContain('正在连接网关');

    // After fetch settles
    await waitFor(() => {
      expect(screen.getByLabelText('实时并发梯队')).toBeTruthy();
    });

    const tier = screen.getByLabelText('实时并发梯队');
    expect(tier.textContent).toContain('api-model-1');
    expect(tier.textContent).toContain('99.1 tok/s');
    expect(tier.textContent).toContain('api-model-2');
    expect(tier.textContent).toContain('88.2 tok/s');
  });
});


it('keeps all 100 multi-window quota entries operable in the list and dock controls outside that scroll region', async () => {
  const items = Array.from({ length: 100 }, (_, index) => ({
    channel_id: 5, credential_id: `fixture-${index}`, channel_name: '示例Codex', credential_name: `凭证${index}`,
    kind: 'windows', enabled: true, balances: [], stale: false, checked_at: '2026-10-19T00:00:00Z',
    windows: Array.from({ length: 4 }, (_, windowIndex) => ({ id: `window-${windowIndex}`, label: `窗口${windowIndex}`, remaining_percent: 50, resets_at: '2026-10-20T00:00:00Z' })),
  }));
  const fetcher = vi.fn((url: string) => Promise.resolve(jsonResponse(url === '/api/quotas' ? { items, refresh_seconds: 0 } : url === '/api/companion/snapshot' ? mockSnapshot() : { requested: true })));
  vi.stubGlobal('fetch', fetcher);
  render(<FloatingPet quotaTargets={items} live={{ snapshot: mockSnapshot(), connection: 'connected', notice: null, scene: 'idle' }} />);
  fireEvent.click(screen.getByRole('button', { name: '打开控制坞' }));
  const list = await screen.findByRole('region', { name: '关注额度列表' });
  await waitFor(() => expect(within(list).getAllByRole('button', { name: /^查看渠道/ })).toHaveLength(3));
  const more = screen.getByRole('button', { name: '展开更多（97）' });
  expect(list.contains(more)).toBe(false);
  fireEvent.click(more);
  await waitFor(() => expect(within(list).getAllByRole('button', { name: /^查看渠道/ })).toHaveLength(100));
  const last = within(list).getByRole('button', { name: '查看渠道 示例Codex 凭证99' });
  // jsdom verifies DOM/focus/action boundaries, not physical scrolling or geometry.
  last.focus(); expect(document.activeElement).toBe(last); fireEvent.click(last);
  await waitFor(() => expect(fetcher.mock.calls.some(([url]) => url === '/api/desktop/open-channel')).toBe(true));
  const write = fetcher.mock.calls.find(([url]) => url === '/api/desktop/open-channel')! as unknown as [string, RequestInit];
  expect(JSON.parse(String(write[1].body))).toEqual({ channel_id: 5, credential_id: 'fixture-99' });
  for (const name of ['暂停动画', '返回控制台', '关闭桌宠', '收起']) expect(list.contains(screen.getByRole('button', { name }))).toBe(false);
  expect(list.tabIndex).toBe(0);
  fireEvent.click(screen.getByRole('button', { name: '收起' }));
  await waitFor(() => expect(within(list).getAllByRole('button', { name: /^查看渠道/ })).toHaveLength(3));
});
