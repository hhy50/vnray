// 与 Go 侧 admin/api*.go 的响应结构一一对应。改了那边记得同步这里。

export interface Envelope<T> {
  code: number
  msg: string
  data: T
}

export interface SessionUser {
  username: string
  name: string
}

export interface Session {
  user: SessionUser
  csrf: string
}

export interface Status {
  version: string
  started_at: string
  uptime_seconds: number
  client_count: number
  active_tunnels: number
  traffic_bytes: number
}

export interface ClientInterface {
  name: string
  mac: string
  addrs: string[]
}

export interface Client {
  id: number
  name: string
  online: boolean
  local: boolean
  version: string
  os: string
  arch: string
  hostname: string
  remote: string
  connected_at: string | null
  last_heartbeat: string | null
  rtt_ms: number
  outbounds: string[]
  interfaces: ClientInterface[]
  up_bytes: number
  down_bytes: number
}

export interface ClientList {
  clients: Client[]
  total: number
  platforms: string[]
}

export interface HostRow {
  domain: string
  ip: string
}

export interface IPMapRow {
  from: string
  to: string
}

export interface ClientDetail {
  name: string
  online: boolean
  remote: string
  local: boolean
  log_level: string
  reported_json: string
  server_config: string
  stored_raw: boolean
  has_stored: boolean
  updated_at: string
  hosts: HostRow[]
  ip_maps: IPMapRow[]
  concurrency: number
}

export interface ClientOption {
  name: string
  label: string
}

export interface User {
  protocol: string
  user_key: string
  name: string
  client: string
  enabled: boolean
  limit_up_kb: number
  limit_down_kb: number
  concurrency: number
  log_level: string
  up_bytes: number
  down_bytes: number
  created_at: string
  updated_at: string
  shareable: boolean
  share_link: string
  // 旧内核（Xray < 26.2.6）专用的链接，仅自签证书时返回；两代链接不能混用。
  share_link_legacy?: string
}

export interface UserList {
  users: User[]
  total: number
  clients: ClientOption[]
  protocols: string[]
}

export interface PortMap {
  id: number
  client: string
  remote_port: number
  local_host: string
  local_port: number
  limit_up_kb: number
  limit_down_kb: number
  tls_enabled: boolean
  tls_configured: boolean
  enabled: boolean
  remark: string
  listening: boolean
  client_online: boolean
  updated_at: string
}

export interface PortMapList {
  rows: PortMap[]
  clients: ClientOption[]
  public_host: string
}

export interface PortKV {
  protocol: string
  port: number
}

export interface PortSlot {
  protocol: string
  label: string
  field: string
  value: number
}

export interface PluginTLS {
  protocol: string
  enabled: boolean
  server_name: string
  self_signed: boolean
  not_after?: string
  fingerprint?: string
}

export interface Plugin {
  name: string
  enabled: boolean
  ports: PortKV[]
  params: Record<string, string>
  slots: PortSlot[]
  accepts_users: boolean
  tls?: PluginTLS
}

export interface PluginList {
  plugins: Plugin[]
  server_port: number
}

export interface KeyRow {
  key: string
  scope: string
  created_at: string
  updated_at: string
}

export interface Logs {
  path: string
  query: string
  lines: string[]
  truncated: boolean
}

export interface ServiceConfig {
  public_host: string
  heartbeat_enabled: boolean
  heartbeat_interval: string
  heartbeat_timeout: string
  log_level: string
  traffic_flush_interval: string
  keepalive_interval: string
  keepalive_timeout: string
  max_stream_window_kb: number | null
  accept_backlog: number | null
  stream_open_timeout: string
  log_rotate_max_size_mb: number | null
  log_rotate_max_backups: number | null
  log_rotate_max_age_days: number | null
  server_proxy_enabled: boolean
  server_proxy_public: string
  server_proxy_private: string
  server_proxy_routing: string
  log_to_file: boolean
}
