# 客户端配置上报协议

本节规定 `ConfigReport` 的客户端身份字段；其余帧类型、二进制帧头及安全握手不因本次调整改变。

## 上报载荷

安全握手后，客户端通过首次 `ConfigReport` 注册身份，随后定期使用同样的字段更新环境和配置摘要：

```json
{
  "client": {
    "group": "office",
    "name": "desktop-01",
    "hostname": "desktop-01"
  }
}
```

- `client.group` 是分组，`client.name` 是组内实例名，两项均为必填非空字符串，均不得包含 `:` 或 `*`。
- 客户端配置中的空分组在上报前填为 `default`；空实例名在上报前生成主机名与 IPv4 后缀。
- 上报不包含拼接的 `group:name` 或服务端绑定 key。版本、系统、网卡、路由等原有摘要字段继续使用原结构。
- 同一会话内 group 和 name 均不可变；修改任一项会被拒绝并关闭会话。
- 服务端应答使用原有 `ConfigReportResult`：成功时返回配置快照，拒绝时返回失败原因。
- 用户认证应答中的客户端摘要也使用独立的 group、name，在线时包含完整上报信息，离线时仅包含身份。

## 兼容性

客户端与服务端必须同步升级。旧版仅上报 `name: "office:desktop-01"` 的载荷不再接受。此变更不分配或重排消息类型编号，不调整二进制帧头与安全握手。

服务端内部仍以 `office:desktop-01` 为绑定键，因此已有用户绑定、端口映射、配置和密钥范围可以继续使用。管理 API 的 `key` 仅供这些操作定位客户端，展示字段分别为 group 与 name。详见 [ADR 0064](adr/0064-client-report-identity-fields.md)。
