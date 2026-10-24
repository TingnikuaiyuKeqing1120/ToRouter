import { memo, useEffect, useMemo, useRef, useState } from 'react';
import {
  Activity,
  Sparkles,
  BarChart3,
  BookOpen,
  Coins,
  Copy,
  KeyRound,
  Layers,
  LogOut,
  Moon,
  Plug,
  ScrollText,
  Search,
  Settings as SettingsIcon,
  Sun,
  Tag,
  Unplug,
  X,
} from 'lucide-react';
import { api } from '../api';
import { useToast } from './ui';

export interface CommandPaletteProps {
  isOpen: boolean;
  onClose: () => void;
  onNavigate: (pageId: string) => void;
  currentPage: string;
  toggleTheme: () => void;
  theme: 'dark' | 'light';
  onLogout: () => void;
}

interface PaletteAction {
  id: string;
  title: string;
  subtitle?: string;
  category: '页面导航' | '快捷操作';
  icon: typeof BarChart3;
  keywords: string[];
  disabled?: boolean;
  run: () => void;
}

export const CommandPalette = memo(function CommandPalette({
  isOpen,
  onClose,
  onNavigate,
  currentPage,
  toggleTheme,
  theme,
  onLogout,
}: CommandPaletteProps) {
  const [query, setQuery] = useState('');
  const [selectedIndex, setSelectedIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const toast = useToast();
  const [connection, setConnection] = useState<{ origin: string; host: string } | null>(null);
  const [connectionError, setConnectionError] = useState('');
  useEffect(() => {
    if (!isOpen) return;
    const abort = new AbortController();
    setConnection(null); setConnectionError('');
    api<{ origin: string; port: number }>('/connection', { signal: abort.signal })
      .then(value => {
        const url = new URL(value.origin);
        if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) throw new Error('网关地址无效');
        if (!abort.signal.aborted) setConnection({ origin: url.origin, host: url.host });
      })
      .catch(() => { if (!abort.signal.aborted) setConnectionError('无法获取网关地址，暂不可复制'); });
    return () => abort.abort();
  }, [isOpen]);
  const unavailable = connectionError || '正在获取网关地址，暂不可复制';


  const actions = useMemo<PaletteAction[]>(() => [
    {
      id: 'nav-usage',
      title: '用量概览',
      subtitle: '查看请求数、Tokens 与费用趋势',
      category: '页面导航',
      icon: BarChart3,
      keywords: ['yongliang', 'gailan', 'dashboard', 'usage', 'metrics', 'stats'],
      run: () => onNavigate('usage'),
    },
    {
      id: 'nav-channels',
      title: '上游接入',
      subtitle: '管理 API Key、OAuth 账号与渠道探测',
      category: '页面导航',
      icon: Unplug,
      keywords: ['qudao', 'shangyou', 'jieru', 'channels', 'providers', 'upstream', 'key'],
      run: () => onNavigate('channels'),
    },
    {
      id: 'nav-models',
      title: '公开模型',
      subtitle: '配置对外暴露模型与上游渠道绑定',
      category: '页面导航',
      icon: Layers,
      keywords: ['moxing', 'xiayou', 'models', 'routes', 'downstream'],
      run: () => onNavigate('models'),
    },
    {
      id: 'nav-client-access',
      title: '客户端接入',
      subtitle: '创建客户端 API Key 与集成代码',
      category: '页面导航',
      icon: KeyRound,
      keywords: ['jieru', 'kehuduan', 'api-key', 'client', 'access', 'token', 'curl'],
      run: () => onNavigate('client-access'),
    },
    {
      id: 'nav-model-specs',
      title: '参数配置',
      subtitle: '管理具体上游模型能力与原生映射',
      category: '页面导航',
      icon: BookOpen,
      keywords: ['canshu', 'peizhi', 'parameters', 'specs', 'guige'],
      run: () => onNavigate('model-specs'),
    },
    {
      id: 'nav-prices',
      title: '价格配置',
      subtitle: '调整模型官方标价与渠道支出折算',
      category: '页面导航',
      icon: Coins,
      keywords: ['jiage', 'peizhi', 'prices', 'rates', 'pricing', 'cost'],
      run: () => onNavigate('prices'),
    },
    {
      id: 'nav-tags',
      title: '标签配置',
      subtitle: '管理标签与标签组',
      category: '页面导航',
      icon: Tag,
      keywords: ['biaoqian', 'peizhi', 'tags', 'groups'],
      run: () => onNavigate('tags'),
    },
    {
      id: 'nav-status',
      title: '运行状态',
      subtitle: 'RUNTIME STATUS · 查看运行异常与恢复凭据',
      category: '页面导航',
      icon: Activity,
      keywords: ['yunxing', 'zhuangtai', 'runtime', 'status', 'cooldown'],
      run: () => onNavigate('status'),
    },
    {
      id: 'nav-mcp',
      title: 'MCP控制台',
      subtitle: 'MCP CONSOLE · 管理客户端连接与授权',
      category: '页面导航',
      icon: Plug,
      keywords: ['mcp', 'kongzhitai', 'console', 'management', '管理连接'],
      run: () => onNavigate('mcp'),
    },
    {
      id: 'nav-logs',
      title: '请求日志',
      subtitle: '查看调用明细、TTFT 首字延迟与排错',
      category: '页面导航',
      icon: ScrollText,
      keywords: ['rizhi', 'qingqiu', 'logs', 'requests', 'trace', 'ttft'],
      run: () => onNavigate('logs'),
    },
    {
      id: 'nav-catalog',
      title: '模型数据库',
      subtitle: '查询来源规格与报价',
      category: '页面导航',
      icon: BookOpen,
      keywords: ['moxing', 'shujuku', 'database', 'catalog'],
      run: () => onNavigate('catalog'),
    },
    {
      id: 'nav-settings',
      title: '系统设置',
      subtitle: '管理访问密钥、额度刷新周期与诊断',
      category: '页面导航',
      icon: SettingsIcon,
      keywords: ['shezhi', 'xitong', 'settings', 'config', 'quota'],
      run: () => onNavigate('settings'),
    },
    {
      id: 'act-theme',
      title: theme === 'dark' ? '切换为浅色模式' : '切换为暗黑模式',
      subtitle: `当前正在使用 ${theme === 'dark' ? '暗黑模式' : '浅色模式'}`,
      category: '快捷操作',
      icon: theme === 'dark' ? Sun : Moon,
      keywords: ['zhuti', 'anhei', 'qianse', 'theme', 'dark', 'light'],
      run: toggleTheme,
    },
    {
      id: 'nav-companion',
      title: '桌宠管理',
      subtitle: '开启或关闭桌面小宠',
      category: '页面导航',
      icon: Sparkles,
      keywords: ['zhuochong', 'companion', 'pet', 'donghua'],
      run: () => onNavigate('companion'),
    },
    {
      id: 'act-copy-base-url',
      title: '复制网关 BaseURL',
      subtitle: connection ? `${connection.origin}/v1` : unavailable,
      disabled: !connection,
      category: '快捷操作',
      icon: Copy,
      keywords: ['fuzhi', 'wangguan', 'url', 'baseurl', 'copy', 'endpoint'],
      run: async () => {
        if (!connection) return;
        const url = `${connection.origin}/v1`;
        try {
          await navigator.clipboard.writeText(url);
          toast(`已复制网关 BaseURL ${url}`);
        } catch {
          toast(`复制失败，请手动复制：${url}`);
        }
      },
    },
    {
      id: 'act-copy-probe',
      title: '复制本地监听端口',
      subtitle: connection?.host || unavailable,
      disabled: !connection,
      category: '快捷操作',
      icon: Copy,
      keywords: ['fuzhi', 'dizhi', 'port', 'host'],
      run: async () => {
        if (!connection) return;
        try {
          await navigator.clipboard.writeText(connection.host);
          toast(`已复制本地监听地址 ${connection.host}`);
        } catch {
          toast(`复制失败，请手动复制：${connection.host}`);
        }
      },
    },
    {
      id: 'act-logout',
      title: '退出登录',
      subtitle: '安全退出当前控制台管理会话',
      category: '快捷操作',
      icon: LogOut,
      keywords: ['tuichu', 'dengchu', 'logout', 'exit', 'signout'],
      run: onLogout,
    },
  ], [currentPage, onNavigate, onLogout, theme, toggleTheme, toast, connection, unavailable]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return actions;
    return actions.filter(item => {
      if (item.title.toLowerCase().includes(q)) return true;
      if (item.subtitle?.toLowerCase().includes(q)) return true;
      if (item.category.toLowerCase().includes(q)) return true;
      return item.keywords.some(k => k.toLowerCase().includes(q));
    });
  }, [actions, query]);

  useEffect(() => {
    setSelectedIndex(0);
  }, [query]);

  useEffect(() => {
    if (isOpen) {
      setQuery('');
      setSelectedIndex(0);
      const timer = setTimeout(() => inputRef.current?.focus(), 50);
      return () => clearTimeout(timer);
    }
  }, [isOpen]);

  useEffect(() => {
    if (!isOpen) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        onClose();
        return;
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setSelectedIndex(i => (i + 1 < filtered.length ? i + 1 : 0));
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setSelectedIndex(i => (i - 1 >= 0 ? i - 1 : filtered.length - 1));
        return;
      }
      if (e.key === 'Enter') {
        e.preventDefault();
        const action = filtered[selectedIndex];
        if (action && !action.disabled) {
          action.run();
          onClose();
        }
      }
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, filtered, selectedIndex, onClose]);

  // Scroll selected item into view
  useEffect(() => {
    if (!isOpen || !listRef.current) return;
    const activeEl = listRef.current.querySelector<HTMLElement>('[data-active="true"]');
    if (activeEl && typeof activeEl.scrollIntoView === 'function') {
      activeEl.scrollIntoView({ block: 'nearest' });
    }
  }, [selectedIndex, isOpen]);

  if (!isOpen) return null;

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="命令导航面板"
      className="fixed inset-0 z-50 flex items-start justify-center p-4 pt-16 sm:pt-24 bg-black/65 backdrop-blur-sm animate-fade-in"
      onClick={e => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="w-full max-w-xl overflow-hidden rounded-2xl border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-2xl transition-all">
        {/* Search Input Bar */}
        <div className="flex items-center gap-3 border-b border-[var(--color-border)] px-4 py-3.5">
          <Search size={18} className="shrink-0 text-[var(--color-text-muted)]" />
          <input
            ref={inputRef}
            type="text"
            className="flex-1 bg-transparent text-sm font-medium text-[var(--color-text-primary)] placeholder-[var(--color-text-muted)] outline-none"
            placeholder="搜索功能、页面或快捷操作 (支持拼音)..."
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
          {query ? (
            <button
              type="button"
              onClick={() => setQuery('')}
              className="rounded p-1 text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            >
              <X size={15} />
            </button>
          ) : (
            <kbd className="rounded border border-[var(--color-border)] bg-[var(--color-surface-2)] px-1.5 py-0.5 font-mono text-[10px] text-[var(--color-text-muted)]">
              ESC 关闭
            </kbd>
          )}
        </div>

        {/* Action List */}
        <div
          ref={listRef}
          className="max-h-[380px] overflow-y-auto p-2 scroll-smooth"
        >
          {filtered.length === 0 ? (
            <div className="py-12 text-center text-sm text-[var(--color-text-muted)]">
              未找到匹配命令或页面
            </div>
          ) : (
            filtered.map((item, index) => {
              const isSelected = index === selectedIndex;
              const isCurrent = item.id.replace('nav-', '') === currentPage;
              const Icon = item.icon;

              return (
                <button
                  key={item.id}
                  type="button"
                  disabled={item.disabled}
                  data-active={isSelected}
                  className={`flex w-full items-center gap-3 rounded-xl px-3.5 py-2.5 text-left transition-all ${
                    isSelected
                      ? 'bg-[var(--color-surface-hover)] text-[var(--color-text-primary)] shadow-sm'
                      : 'text-[var(--color-text-secondary)] hover:bg-[var(--color-surface-2)]'
                  }`}
                  onMouseEnter={() => setSelectedIndex(index)}
                  onClick={() => {
                    item.run();
                    onClose();
                  }}
                >
                  <div
                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border ${
                      isSelected
                        ? 'border-[var(--brand-blue)]/30 bg-[var(--brand-blue)]/15 text-[var(--brand-blue)]'
                        : 'border-[var(--color-border)] bg-[var(--color-surface-2)] text-[var(--color-text-muted)]'
                    }`}
                  >
                    <Icon size={16} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-xs font-semibold text-[var(--color-text-primary)]">
                        {item.title}
                      </span>
                      {isCurrent && (
                        <span className="rounded bg-[var(--brand-blue)]/15 px-1.5 py-0.2 text-[10px] font-medium text-[var(--brand-blue)]">
                          当前页
                        </span>
                      )}
                    </div>
                    {item.subtitle && (
                      <p className="truncate text-[11px] text-[var(--color-text-muted)]">
                        {item.subtitle}
                      </p>
                    )}
                  </div>
                  {isSelected && (
                    <kbd className="shrink-0 rounded border border-[var(--color-border)] bg-[var(--color-surface-1)] px-1.5 py-0.5 font-mono text-[10px] text-[var(--color-text-muted)]">
                      回车执行
                    </kbd>
                  )}
                </button>
              );
            })
          )}
        </div>

        {/* Footer Hints */}
        <div className="flex items-center justify-between border-t border-[var(--color-border)] bg-[var(--color-surface-2)]/50 px-4 py-2 text-[11px] text-[var(--color-text-muted)]">
          <div className="flex items-center gap-3">
            <span>
              <kbd className="rounded border border-[var(--color-border)] px-1 py-0.5 text-[10px]">↑</kbd>{' '}
              <kbd className="rounded border border-[var(--color-border)] px-1 py-0.5 text-[10px]">↓</kbd> 切换
            </span>
            <span>
              <kbd className="rounded border border-[var(--color-border)] px-1.5 py-0.5 text-[10px]">↵</kbd> 选择
            </span>
          </div>
          <span>to-router 控制台快捷命令</span>
        </div>
      </div>
    </div>
  );
});

export default CommandPalette;
