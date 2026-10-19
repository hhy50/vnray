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
