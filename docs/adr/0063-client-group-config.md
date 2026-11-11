# ADR 0063：客户端独立配置分组和实例名

- 状态：已采纳
- 日期：2026-11-11
- Supersedes：[ADR 0062](0062-client-groups.md) 中客户端配置 `name` 承载完整 `group:name` 的约定；保留其身份和授权格式。

## 决策

客户端 YAML 顶层增加 `group`，`name` 只表示组内实例名：

```yaml
group: office
name: desktop-01
```

上报名称为 `office:desktop-01`。`group` 缺省或为空时使用 `default`；`name` 缺省或为空时生成 `<hostname>-<IPv4>`，取不到 IPv4 时省略 IP 后缀。只设置分组也能使用自动生成的实例名。

两个配置字段都不允许包含 `:` 或 `*`。装配层负责组合完整名称，协议和服务端仍使用 `group:name`，接入密钥范围仍为 `*`、`group:*` 或 `group:name`。不增加协议字段或消息类型，不改变进程内固定名称和重连行为。OpenWrt 的 UCI、运行时配置和 LuCI 表单同步拆分字段。

## 迁移

已有 `name: office:desktop-01` 必须拆成上述两个字段，不自动解析旧写法，避免显式 group 与 name 中的分组冲突。完整身份保持不变时，已有服务端绑定、端口映射和密钥无需修改；此前名称留空的配置继续生成相同的默认身份。

## 验证

覆盖 YAML 加载、独立字段校验、缺省和空分组、显式和自动实例名、上报完整身份，以及 OpenWrt 配置传递。
