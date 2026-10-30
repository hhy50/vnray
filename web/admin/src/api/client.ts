import type {
  ClientConfig,
  ClientDetail,
  ClientList,
  Envelope,
  HostRow,
  IPMapRow,
  KeyRow,
  Logs,
  PortMapList,
  PluginList,
  ServiceConfig,
  Session,
  Status,
  UserList,
} from './types'
import { useUserStore } from '../stores/user'

// ApiError 携带 HTTP 状态码，调用方据此区分"未登录"与"操作失败"。
export class ApiError extends Error {
  status: number

  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

// request 是所有接口的唯一出口。path 以 /admin/ 开头时按绝对路径使用
// （登录/登出不在 /admin/ops 下），否则自动补 /admin/ops 前缀。
async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method ?? 'GET').toUpperCase()
  const url = path.startsWith('/admin/') ? path : '/admin/ops' + path
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (init.body) headers['Content-Type'] = 'application/json'
  // 动态获取 store，避免模块初始化时 pinia 未就绪
  try {
    const userStore = useUserStore()
    if (method !== 'GET' && method !== 'HEAD' && userStore.csrf) {
      headers['X-CSRF-Token'] = userStore.csrf
    }
  } catch {
    /* store 未初始化时跳过 */
  }

  const res = await fetch(url, {
    credentials: 'same-origin',
    ...init,
    headers: { ...headers, ...((init.headers as Record<string, string>) ?? {}) },
  })

  if (res.status === 401) {
    // 401 时只抛错，由上层（路由守卫 / store）决定跳转方式，
    // 避免整页刷新导致的循环跳转。
    throw new ApiError('未登录', 401)
  }

  const body = (await res.json().catch(() => null)) as
    | (Envelope<T> & { status?: boolean })
    | null
  if (!res.ok || (body && typeof body.code === 'number' && body.code !== 0)) {
    const message =
      (body as { msg?: string } | null)?.msg ||
      res.statusText ||
      `HTTP ${res.status}`
    throw new ApiError(message, res.status)
  }
  return (body && 'data' in body ? (body as Envelope<T>).data : (body as T)) as T
}

const query = (params: Record<string, string | number | boolean | undefined>) => {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

const json = (method: string, body?: unknown): RequestInit => ({
  method,
  body: body === undefined ? undefined : JSON.stringify(body),
})

export const api = {
  session: () => request<Session>('/api/session'),
  status: () => request<Status>('/api/status'),

  // 登录
  login: (username: string, password: string) =>
    request<{ csrf: string }>('/admin/auth/login', json('POST', { username, password })),
  logout: () => request<{ ok: boolean }>('/admin/auth/logout', json('POST', {})),

  clients: (params: { q?: string; platform?: string } = {}) =>
    request<ClientList>(`/api/clients${query(params)}`),
  clientDetail: (name: string) =>
    request<ClientDetail>(`/api/clients/${encodeURIComponent(name)}`),
  clientConfigSave: (name: string, body: { config: string; log_level: string }) =>
    request<{ name: string }>(`/api/clients/${encodeURIComponent(name)}/config`, json('PUT', body)),
  clientHostsSave: (
    name: string,
    body: { hosts: HostRow[]; ip_maps: IPMapRow[]; concurrency: number },
  ) =>
    request<{ name: string; pushed: boolean }>(
      `/api/clients/${encodeURIComponent(name)}/hosts`,
      json('PUT', body),
    ),
  clientKick: (id: number) => request<{ id: number }>(`/api/clients/${id}/kick`, json('POST')),

  users: (params: { q?: string; protocol?: string } = {}) =>
    request<UserList>(`/api/users${query(params)}`),
  userSave: (body: {
    ss_method?: string
    protocol: string
    user_key: string
    name: string
    client: string
    enabled?: boolean
  }) => request(`/api/users`, json('POST', body)),
  userDelete: (protocol: string, key: string) =>
    request(`/api/users${query({ protocol, key })}`, json('DELETE')),
  userStatus: (protocol: string, key: string, enabled: boolean) =>
    request(`/api/users/status${query({ protocol, key, enabled })}`, json('POST')),
  userLimits: (
    protocol: string,
    key: string,
    body: { up_kbps: number; down_kbps: number; concurrency: number; log_level: string },
  ) => request(`/api/users/limits${query({ protocol, key })}`, json('PUT', body)),

  portmaps: () => request<PortMapList>('/api/portmaps'),
  portMapSave: (
    body: {
      client: string
      remote_port: number
      local_host: string
      local_port: number
      limit_up_kb: number
      limit_down_kb: number
      tls_enabled: boolean
      cert_pem: string
      key_pem: string
      remark: string
      enabled: boolean
    },
    id?: number,
  ) => (id ? request(`/api/portmaps/${id}`, json('PUT', body)) : request(`/api/portmaps`, json('POST', body))),
  portMapToggle: (id: number) => request(`/api/portmaps/${id}/toggle`, json('POST')),
  portMapDelete: (id: number) => request(`/api/portmaps/${id}`, json('DELETE')),

  plugins: () => request<PluginList>('/api/plugins'),
  pluginAction: (name: string, action: 'start' | 'stop' | 'port' | 'tls', params: Record<string, string> = {}) =>
    request(
      `/api/plugins/${encodeURIComponent(name)}/${action}`,
      json(action === 'start' || action === 'stop' ? 'POST' : 'PUT', action === 'stop' ? undefined : { params }),
    ),

  keys: () => request<{ keys: KeyRow[] }>('/api/keys'),
  keySave: (body: { key: string; scope: string }) => request('/api/keys', json('POST', body)),
  keyDelete: (key: string) => request(`/api/keys${query({ key })}`, json('DELETE')),

  logs: (params: { q?: string; lines?: number } = {}) =>
    request<Logs>(`/api/logs${query(params)}`),

  serviceConfig: () => request<ServiceConfig>('/api/service-config'),
  serviceConfigSave: (body: ServiceConfig) =>
    request('/api/service-config', json('PUT', body)),

  clientConfig: () => request<ClientConfig>('/api/client-config'),
  clientConfigSave: (body: ClientConfig) =>
    request('/api/client-config', json('PUT', body)),
}
