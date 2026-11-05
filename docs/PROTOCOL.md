# vnray 协议

## 客户端认证传输 v2

内网客户端先完成以下传输握手，采用服务端选择的加密或明文模式，再建立 yamux 并发送 DMP Auth。DMP 魔数与消息编号不变。所有多字节长度使用大端，nonce 计数器使用小端。

管理台「服务配置 → 加密传输」对应 `transport_encryption`，默认 true。保存持久化并影响后续握手；已连接会话保持原模式，重连后采用新模式。客户端只配置 auth_key，无需配置加密开关。旧库缺段时保持开启，保存 API 省略该字段时保留原值。

### 密钥与握手

`auth_key` 是 32 字节随机 PSK 的规范标准 Base64（含填充，44 个字符），由管理台后端或 `vnserver keygen` 生成。不是口令，不进行口令拉伸，也不接受旧口令。原始 PSK 不出现在明文握手中。

公开 key_id = SHA256(`"vnray transport key id v1\x00"` || PSK)，32 字节。它是可关联连接的公开标识，不是随机每次改变的身份。

握手按顺序交换三条固定长度消息：

1. C → S：Hello = `"VNRAY"` || `0x02`（版本）|| `0x01`（支持 ChaCha20-Poly1305）|| `0x00`（保留）|| key_id[32] || client_random[32]，共 72 字节。
2. S → C：server_random[32] || mode[1] || server_proof[32]，共 65 字节。mode 为 0（握手后明文）或 1（握手后 AEAD）。
3. C → S：client_proof[32]，共 32 字节。

两个随机数独立从系统密码随机源生成，每次连接重新生成。未知版本、未知套件、非零保留位、未知 mode 均拒绝。派生公式：

```text
transcript_hash = SHA256(Hello || server_random || mode)
material = HKDF-SHA256(PSK, salt=transcript_hash,
                       info="vnray transport v2 keys", length=96)
proof_key = material[0:32]
client_to_server_key = material[32:64]
server_to_client_key = material[64:96]
server_proof = HMAC-SHA256(proof_key, "vnray transport v2 server finished")
client_proof = HMAC-SHA256(proof_key, "vnray transport v2 client finished")
```

客户端先验证 server_proof，再发送 client_proof；服务端验证 client_proof 后才允许启动 yamux。mode 同时受双方证明保护，途中篡改会导致握手失败。禁止早期数据。重放旧 Hello 会得到新的 server_random，旧 proof 无法使用；握手不依赖时间戳和缓存。证明使用常量时间比较。握手总时限 5 秒，支持上下文取消；完成后清除底层连接的握手 deadline。

服务端密钥表非空时按 key_id 查表，后续 Auth 仍按客户端名匹配范围；表为空时才使用配置文件随机密钥，无密钥或数据库错误都拒绝。Auth 的 `key` 字段为已证明 key_id 的小写十六进制编码（64 字符），必须与传输握手身份一致；两种模式都不发送原始 PSK。服务端重新查询该身份并验证当前范围授权，删除密钥或缩小范围可阻止尚未完成 Auth 的连接。旧数据库条目保留但不能建立新传输。

服务端启动时将密钥和授权范围加载为 key_id 内存索引，握手及范围授权平均 O(1)，不在每次连接时扫描数据库。管理台写入成功后同步更新索引；写入结果不确定或重载失败会使索引失效，重新加载成功前拒绝认证。有效缓存不要求数据库持续在线。直接修改数据库或多个服务端共享数据库时，需要重启相应服务端或通过程序调用 ReloadAuthKeys 同步索引，见 [ADR 0044](adr/0044-auth-key-index.md)。

### 数据记录

mode=0 时，握手完成后直接传输 yamux 字节，不使用以下 AEAD 记录。此模式的业务数据没有保密与完整性保护。mode=1 时使用以下记录格式。

每方向独立维护密钥、nonce 和记录数，不共享状态。初始 nonce 计数器为 0；每次 AEAD 调用递增一次。12 字节 nonce = 4 字节零 || 8 字节小端计数器。

```text
AEAD_Seal(length[2], nonce=N) [18 字节]
AEAD_Seal(payload[length], nonce=N+1) [length+16 字节]
```

长度为明文载荷字节数，范围 1..16384。两次 AEAD 均使用空 AAD；顺序由独立方向密钥及隐式 nonce 绑定，记录不能重排、插入或复制。零长度 Write 不产生记录。读端先验证长度，再分配有界缓冲并验证完整载荷，验证前不交付明文。

每方向处理完 65536 条记录，在下一条前执行：

```text
next_key = HKDF-SHA256(current_key, salt=nil,
                       info="vnray transport v1 rekey", length=32)
nonce = 0
record_count = 0
```

每代最多 131072 次 AEAD 调用、1 GiB 明文载荷，换代由记录数确定，不引入额外控制帧。上行和下行可以独立换代。

认证失败、非法长度和 I/O 错误立即关闭整个 TCP 连接，错误后不恢复读写。传输没有独立的已认证 close_notify，底层 EOF（即使发生在记录边界）作为 unexpected EOF 处理，不能声称检测到了正常完成；正常逻辑流结束由加密内的 yamux FIN 表示。加密包装不暴露 TCP 半关闭，yamux 流的半关闭语义保持不变。

### 兼容与边界

BREAKING：传输 v1 与 v2 不兼容，服务端和内网客户端必须同步升级；生成密钥无需更换。不允许握手失败后自动降级，明文只由服务端在已认证的 v2 握手中选择。主监听端口保留 yamux v0（首字节 0）的原有 vn-v1 用户路径，该路径仅允许 UserAuth，客户端 Auth 被拒绝。v2 客户端首字节为 `V`，其它首字节或不合法 Hello 被拒绝。VLESS/VMess/SS 入站及管理台 HTTP 不由此层加密。

PSK 持有者可模拟任一端，本设计没有前向保密；按范围分发不同密钥可限制信任域。删除密钥拒绝新会话，既存会话需主动断开。参考 [ADR 0043](adr/0043-client-aead-transport.md) 与 [ADR 0045](adr/0045-negotiated-transport-encryption.md)。

## 配置下发

## 分层配置快照

服务端使用 `ConfigPush`（消息类型 14）下发完整 JSON 快照；首次连接和重连时，相同快照放在 `AuthResult`（消息类型 7）的 `config` 字段中。

```json
{
  "client_config": {
    "custom_hosts": [{"domain": "vpn.route", "ip": "10.30.0.12"}],
    "ip_maps": [{"from": "192.169.30.12", "to": "10.30.0.12"}]
  },
  "global_config": {
    "custom_hosts": [{"domain": "vpn.route", "ip": "10.60.0.12"}],
    "ip_maps": [{"from": "192.169.30.*", "to": "10.60.0.*"}]
  }
}
```

两个顶层 key 始终发送，空层为 `{}`；层内省略的字段或空数组表示清空该层对应规则。每次收到快照都替换两层配置，不能保留上次遗漏的规则。

客户端查询规则：

1. Host 按小写且去末尾点的域名查独立表，未命中再查全局表，均未命中时使用正常 DNS。
2. IPv4 映射先按独立规则的顺序寻找首条匹配，再在未命中时查询全局规则。
3. 命中即返回，包括目标 IP 与原 IP 相同的情况；不将结果交给另一层二次改写。

上例中 `192.169.30.12` 转为 `10.30.0.12`，`192.169.30.13` 转为 `10.60.0.13`。只清空 `client_config` 后，两者都使用全局映射。

## 兼容性

本次仅改变配置 JSON 结构，帧头和消息编号保持不变。旧版使用顶层 `custom_hosts` / `ip_maps`，不能读取新的分层配置；新版也不将旧版的平铺结构当作分层配置。部署需同步更新服务端与客户端，配置数据库无需迁移。见 [ADR 0042](adr/0042-client-config-layers.md)。
