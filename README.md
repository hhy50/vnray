# vnray：反向代理数据面

内网穿透 / 远程访问工具：**客户端**主动连接**服务端**，服务端把**用户**的请求桥接给客户端，由客户端访问目标。

![vnray 流量路径](https://github.com/hhy50/vnray/blob/master/vnray-traffic-path.png)

Client 部署在能访问目标的位置（如内网），Server 部署在公网；用户接入 Server 即可访问只有 Client 可达的目标，响应沿同一隧道原路返回。

## 支持的功能

- 原生支持 v2ray 的 VMess / VLESS 协议，直接用 v2ray 客户端即可接入
- 支持 Shadowsocks 协议，可直接用 SS 客户端接入
- 端口映射：把端口映射到客户端侧的服务
- 任意客户端网络：可访问客户端所在网络的任意目标
- 流量控制与并发控制：按用户限速、限制并发
- 自定义 host：自定义域名映射到指定 IP
- 一键复制 vmess:// / vless:// 分享链接，直接导入 v2ray 客户端
- 客户端断线自动重连
- 管理台实时查看在线状态、延迟与流量统计

## 怎么部署

```sh
# 构建
go build -o bin/vnray-server ./cmd/server
go build -o bin/vnray-client ./cmd/client

# 启动服务端（监听 9000，管理台 8080）
./bin/vnray-server -c configs/server.yaml

# 启动客户端（configs/client.yaml，auth_key 须与服务端一致）
./bin/vnray-client -c configs/client.yaml
```

打开管理台 `http://127.0.0.1:8080/admin/`（默认 admin / admin），在「用户管理」注册一个 user_key 并绑定到客户端，即可接入使用。

管理台账号分权限：`admin` 持内建 administrator 角色（全权，也是唯一能进「Administrators / Roles / Permissions」的账号）；日常运维另建账号并在 Roles 页分配角色即可。首启会播种两个预设角色——`运维`（可改配置、启停插件、踢客户端）与 `只读`（只看页面，没有任何写入口）。权限目录与路由的对应关系见 [docs/adr/0038-admin-rbac.md](docs/adr/0038-admin-rbac.md)。

管理台界面是 Vue（`http://127.0.0.1:8080/admin/ui/`），业务页全部在这里：客户端、用户、端口映射、插件、认证密钥、服务配置、日志。`/admin/` 会 302 到 `/admin/ui/`；`/admin/auth/*` 仍是 steward 的登录页，账号/角色/权限/审计几个系统页也由 steward 渲染（侧边栏「系统」里进）。迁移记录与接口清单见 [docs/adr/0039-admin-vue-spa.md](docs/adr/0039-admin-vue-spa.md)。

前端产物是**编译前置条件**：`web/admin/dist` 不入库，缺 `dist/index.html` 时 `go build`
会直接报 `pattern dist/index.html: no matching files found`，不会产出没有界面的二进制。

```bash
npm --prefix web/admin install      # 本机 npm 官方源不可用，加 --registry=https://registry.npmmirror.com
npm --prefix web/admin run build    # 产物写到 web/admin/dist，被 go:embed 打进二进制
go build -o bin/vnray-server ./cmd/server
```

没有 Node 环境时（只想跑后端测试也一样）必须先补上前两步；`go test ./...` 会因为
`cmd/server` 依赖这份产物而一起失败。

