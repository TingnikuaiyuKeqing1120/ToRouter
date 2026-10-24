export function clearLegacyKey() {
  try {
    sessionStorage.removeItem('to-router.access-key');
  } catch {
    /* Storage may be disabled. */
  }
}

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message);
  }
}

export async function api<T>(
  path: string,
  options: RequestInit & { suppressAuthExpired?: boolean } = {}
): Promise<T> {
  const { suppressAuthExpired, ...requestOptions } = options;
  const headers = new Headers(options.headers);
  headers.delete('Authorization');
  headers.set('X-To-Router', '1');
  if (options.body) headers.set('Content-Type', 'application/json');

  const response = await fetch(`/api${path}`, {
    ...requestOptions,
    credentials: 'same-origin',
    headers,
  });

  if (!response.ok) {
    if (response.status === 401 && !options.signal?.aborted && !suppressAuthExpired) {
      window.dispatchEvent(new Event('auth-expired'));
    }
    let message = `请求失败（${response.status}）`;
    try {
      const body = await response.json();
      const detail = typeof body.error === 'string' ? body.error : body.error?.message || body.message;
      if (typeof detail === 'string') message = detail;
    } catch {
      /* Non-JSON errors use the HTTP status. */
    }
    throw new ApiError(
      response.status === 401
        ? path === '/auth/login'
          ? '管理员密钥不正确，请检查后重试。'
          : '登录会话无效或已过期，请重新登录。'
        : message,
      response.status
    );
  }

  if (response.status === 204) return undefined as T;
  const body = await response.text();
  return body ? (JSON.parse(body) as T) : (undefined as T);
}

export const errorMessage = (error: unknown) =>
  error instanceof Error ? error.message : '发生未知错误，请重试。';
