<div align="center">
  <h1>vnray</h1>
  <p><strong>用于内网穿透与远程访问的反向代理数据面。</strong></p>
  <p>通过客户端主动建立的隧道，让用户访问内网中的目标服务。</p>
  <p>
    <a href="README.md">English</a> ·
    <a href="README.zh-CN.md"><strong>简体中文</strong></a>
  </p>
  <p>
    <a href="https://go.dev/"><img src="https://img.shields.io/github/go-mod/go-version/hhy50/vnray?style=flat-square" alt="Go 版本"></a>
  </p>
</div>

---

vnray 将公网服务端收到的用户请求桥接到目标网络中的客户端。客户端主动建立并保持连接，代替用户访问目标，再沿同一隧道返回响应，因此无需开放内网的入站访问。

<div align="center">
  <img src="vnray-traffic-path.png" alt="vnray 流量路径" width="900">
</div>

## 功能特性

- **VMess 与 VLESS** — 原生支持 v2ray 协议，可直接使用标准 v2ray 客户端接入。
- **Shadowsocks** — 除 v2ray 协议外，同时支持 Shadowsocks 接入。
- **端口映射** — 通过映射端口暴露客户端侧可访问的服务。
- **灵活出口** — 使用客户端可用的任意网络访问目标。
- **流量控制** — 按用户限制带宽与并发。
- **自定义 Host** — 将域名映射到指定 IP 地址。
- **实时管理台** — 实时查看在线状态、延迟与流量统计。

## 工作原理

| 组件 | 典型部署位置 | 作用 |
| --- | --- | --- |
| **服务端** | 公网 | 接收用户流量与客户端隧道，并将请求桥接到指定客户端。 |
| **客户端** | 内网或目标网络 | 主动保持与服务端的连接，并代替用户访问目标服务。 |

响应沿同一隧道返回，因此用户只需接入服务端。

## 快速开始

以下命令假设 `vnserver` 和 `vnclient` 二进制文件位于当前目录。

### 1. 启动服务端

启动前请检查 [`configs/server.yaml`](configs/server.yaml)。示例配置使用 `9000` 端口接收客户端连接，并在 `8080` 端口提供管理台。

```bash
./vnserver -c configs/server.yaml
```

### 2. 连接客户端

编辑 [`configs/client.yaml`](configs/client.yaml)：

- 将 `server.addr` 设置为服务端公网地址，例如 `server.example.com:9000`。
- 确保 `server.auth_key` 与服务端配置中的 `server.auth.key` 一致。

然后在能够访问目标服务的机器上启动客户端：

```bash
./vnclient -c configs/client.yaml
```

### 3. 登录管理台

访问 `http://<服务端 IP>:8080`，使用 `configs/server.yaml` 中配置的管理员账号登录。已连接的客户端会显示在**客户端**一栏。

<div align="center">
  <img src="admin_client_tab.png" alt="vnray 管理台中的在线客户端列表" width="900">
</div>

### 4. 创建用户

在**用户**一栏中：

1. 选择协议，**推荐使用 VLESS**。
2. 选择作为出口的客户端。
3. 点击**添加**。
4. 复制生成的链接，并导入 v2ray 客户端即可接入。

> **安全提示：** 示例配置包含默认凭据和共享密钥。将服务暴露到公网前，请修改管理员密码和认证密钥。
