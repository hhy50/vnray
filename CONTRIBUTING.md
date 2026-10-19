# 贡献规范

本文件是本项目的开发契约，参照 Go 官方 [Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)、[Effective Go](https://go.dev/doc/effective_go)、Uber Go Style Guide 以及 Kubernetes、etcd、hashicorp、v2ray-core 等项目的通行做法制定，并按本项目（教学型、小内核、可迭代）的规模裁剪。

规范分两级：

- **必须（MUST）**：CI 强制或 review 一票否决。
- **应该（SHOULD）**：默认遵守，偏离需在 PR 中说明理由。

## 1. 设计原则

1. **教学可读优先**：代码是给人读的。巧妙但隐晦的写法，展开成朴素写法加注释。
2. **小内核、薄分层**：核心机制（dispatcher / pipe / outbound）保持零业务依赖；装配逻辑（App 类型、依赖构造、配置翻译）只允许出现在装配包：`internal/core`（server，入口 `initServerApp` 手写组装）与 `internal/cli`（client，命令装配手写），均不用依赖注入框架。
3. **依赖最小化**：标准库能解决的不引第三方。新增运行时依赖必须在 PR 中说明理由、许可证与维护活跃度。
4. **每个改动可回滚**：小步提交、小 PR（建议 ≤ 400 行 diff），一个 PR 只做一件事。

## 2. 目录职责与依赖方向

分层依赖**只能单向向下**，禁止反向依赖和同层环依赖：

```text
cmd/                进程入口：只调用 internal/core（server）或 internal/cli（client），不放业务逻辑
internal/cli/       客户端命令层 + 装配（手写）：cobra 命令、配置覆盖、Dial 与启动、重连循环
internal/client/    客户端运行时：Dial（认证握手）/ Client.Serve（请求流、隧道流、心跳应答）
internal/core/ 服务端命令层 + 装配 + 数据面服务实现（ConnectorService：握手、心跳、转发选路与会话表）；持有 GORM/SQLite 等服务端专用依赖，与 cli 分包以隔离 client 二进制
admin/              管理面：Bindings（user_key 绑定表，写穿 store）、AdminService（Web 管理页，读 transport.SessionManager）
vmess/              VMess 入站协议实现（AEAD 请求头 + 分块载荷），被 internal/plugin 的 v2ray 插件使用
vless/              VLESS 入站协议实现（明文头 + 明文载荷），被 internal/plugin 的 v2ray 插件使用
shadowsocks/        Shadowsocks AEAD 入站协议实现，被 internal/plugin 的 ss 插件使用
internal/acl/       目标网段守卫：包装 dispatch.Dispatcher 在分发前校验
internal/transport/ 会话层：SessionManager（会话登记表）/ Session 包装 / UserRegistry 契约 / 隧道与双向搬运工具
internal/tcp/       事件驱动多路复用监听器（功能级）：Start 打开监听并驱动事件循环；TCP 接入 + 会话/流的建立与事件派发；以 Session/Stream 抽象对外，不向上层泄漏多路复用实现
internal/dispatch/ 数据面内核：Dispatcher / Link / OutboundHandler
internal/protocol/  DMP 二进制帧编解码（固定头 10 B，魔数 0x2026）
internal/pipe/      基础原语：单向内存管道，不依赖项目内任何其他包
internal/store/     绑定表可插拔存储（GORM）：Store 接口 + memory/sqlite/mysql；只依赖 GORM 与驱动，不依赖数据面包
internal/routine/   基础原语：统一协程启动（routine.Go，带 panic recover 与日志）
internal/extension/ 扩展三级生命周期：Service / Component / Feature 契约与可选 Drainer（只有契约，编排由 core.Instance 承担）；不依赖数据面包
internal/config/    配置结构：YAML 加载与校验；不依赖 core/transport
internal/logging/   横切设施：日志
configs/            随仓库发布的示例配置，由测试加载校验
```

规则：

- `pipe` 不得 import 项目内任何包；`dispatch` 只允许依赖 `pipe`（及 `internal/logging` 这类无状态横切设施）。
- `internal/extension` 只依赖标准库与 `internal/logging`；实现包按结构化接口满足三级契约，不反向依赖编排者。新增能力先按**生命周期**定级：进程级（与程序同生共死、无 Stop）→ 服务 Service；可任意启停的中间单元 → 组件 Component；最细能力 → 功能 Feature。详见 [docs/adr/0006](docs/adr/0006-extension-levels.md)。
- `internal/config` 保持为纯数据包（标准库 + yaml），不 import `core`/`transport`；配置到数据面对象的翻译在装配层完成（客户端翻译在 `internal/cli`）。
- `cmd` 只允许调用 `internal/cli` 或 `internal/core` 的启动入口（三行 main）；参数解析、配置装配、依赖构造、组件启动的编排全部在对应装配包完成。
- `dispatch` 不得 import `internal/transport`；协议层只通过 `dispatch.Dispatcher` 等接口与内核交互。
- `internal/transport` 不得 import `admin`：会话登记簿要实现 `transport.SessionEvents`，反向依赖会构成环（装配只放在 `internal/cli` / `internal/core`）。
- 实现包的默认归宿是 `internal/`；例外只有 `vmess/`、`vless/`、`shadowsocks/`、`admin/` 四个放在仓库根——它们因此不再受 `internal` 的导入限制，成为可被外部 import 的公开包（改动其导出 API 属于破坏性变更）。
- 服务端与客户端同处一个模块，但**依赖必须能分开**：client 侧（`internal/cli`、`internal/client`）不得 import `internal/core`/`admin`/`internal/store`，否则 SQLite 驱动的 init 会被链进 client 二进制。
- 包名用小写短词、单数、与目录同名；不要 `util`、`common` 这类无信息量的包，应按职责命名。

依赖方向通过 CI 中的 `go vet` 与后续引入的 `depguard` 规则强制。

## 3. 编码风格

### 3.1 格式（必须）

- 提交前必须通过 `gofmt -s -l`（CI 强制）；推荐本地配置 goimports 自动整理 import。
- import 分组顺序：标准库 / 第三方 / 本项目，组间空行，组内字典序。
- 行宽不做硬限制，但单行长到需要横向滚动时应该人为折行。

### 3.2 命名与注释

- 遵循 Go 惯用法：驼峰命名、首字母缩略词保持一致大小写（`ID`、`URL`、`HTTPPayload`）。
- 变量名作用域越短越短，作用域越长越要有信息量：`for _, r := range rules` 可以，跨函数的 `r` 不行。
- 接收器名称在整个类型的方法集里保持一致，短名即可（如 `func (d *DefaultDispatcher)` 用 `d`）。
- **所有导出标识符必须有 godoc 注释**（CI 用 `revive` 检查），注释以标识符名称开头；包注释以 `Package xxx` 开头。
- 注释必须精简：**只给特别需要注意的地方写注释**——并发与关闭语义、反直觉行为、必须读实现才能发现的坑；显而易见的代码一律不写。注释解释"为什么"，而不是复述"做什么"。
- **禁止行尾注释，尤其禁止把注释写在结构体字段后面**；确需说明时写在被描述对象的上方独立行。本条对 Agent 额外生效，见 `AGENTS.md`。

### 3.3 错误处理（必须）

- 错误是值，不是异常：库代码禁止用 panic 处理可预期错误；panic 只允许出现在 `main` 顶层兜底和"程序员写错代码"的初始化阶段。
- 错误必须被处理或显式忽略。忽略时写 `_ =` 并在同行或上一行注释说明为什么可以忽略（项目中 `_ = conn.Close()` 这类清理调用沿用此约定）。
- 包装错误用 `fmt.Errorf("上下文: %w", err)`，上下文小写开头、不带标点、不带重复的 `failed to` 堆砌。
- 需要被调用方判断的错误用哨兵错误或自定义错误类型，调用方用 `errors.Is` / `errors.As`，不要比较字符串。

```go
var ErrDialCanceled = errors.New("dial canceled")

type ProtocolError struct{ Line string }
func (e *ProtocolError) Error() string { ... }
```

- 错误只处理一次：要么记录日志、要么包装返回，不要记录后又原样返回导致重复日志。

### 3.4 context 与接口

- `context.Context` 作为函数第一个参数，命名 `ctx`，禁止放进结构体长期持有（会话级组件可在构造时持有根 ctx 的衍生 ctx，但必须有对应 cancel 并在 `Close` 中调用）。
- 接口由**消费方**定义，大小以调用方实际用到的方法为准；`Dispatcher` 是三方法以内的小接口，新增方法先想清楚是否该拆。
- 构造函数返回具体类型，让调用方自己决定要不要抽象成接口。
- 参数 ≥ 4 个或预期继续扩展的构造函数使用 functional options 模式；当前规模下简单结构体配置（如 `RejectOutbound{Reason: ...}`）保持不变。

### 3.5 其他

- 魔法数字提常量并注明含义（参考 `pipe.pipeLimit`）。
- 时间用 `time.Duration` 表达，禁止裸 `time.Sleep(5000)`。
- 不使用全局可变状态；`internal/logging` 的全局 logger 是唯一的特许例外，新增全局单例需在 PR 中论证。

## 4. 并发规范（本项目的重点）

本项目的核心复杂度在并发，以下条款全部为**必须**：

1. **每个 goroutine 都要有明确的生命周期归属**：谁启动、谁等待它退出、什么信号触发它退出，三者在代码中都能直接看出来。新起 goroutine 时同步写出它的退出路径。
2. **所有生产代码的协程必须经 `routine.Go(name, fn)` 启动**（name 用"包.组件.职责"命名）：panic 统一 recover 并记录 Error 日志，不允许裸 `go` 击穿进程。若协程结果必须送达调用方（如经 channel 返回错误），需在闭包内保证 panic 路径也送达（参考 `internal/core/outbound.go` 上行泵）。测试代码不受此约束——测试中 panic 应当失败。
2. **等待用 WaitGroup 或 channel，禁止"启动后不管"**。参考 `internal/transport` 中 `Client.Serve()` 的 `wg.Wait()` 与 `core.DirectOutbound` 的 `done` channel。
3. **取消信号一律走 context**：可取消的阻塞操作必须能被 `ctx.Done()` 解除；网络拨号用 `DialContext`，管道操作要能被 `Interrupt` 唤醒。
4. **channel 发送方负责关闭**；多个发送方时由协调者关闭。禁止向可能已关闭的 channel 发送。
5. **资源清理成对出现**：拿到 `net.Conn` 立刻写 `defer conn.Close()`；`context.WithCancel` 立刻写 `defer cancel()`。新增 Close 类型必须说明幂等性。
6. 双向搬运按方向收尾（半关闭）：某一方向读到 EOF 只对**那一端** `Close()`，另一方向继续搬运；只有出错才两端一起 Close。半关闭由端点自身提供：yamux 的 `Close` 即半关闭（发 FIN、只禁本地写、读方向保留），VMess 用户侧的带内终结块随 `vmess.Conn.Close` 一并发出（见 `vmess/server.go`）。对隧道流的 `Close` 只允许服务端转发侧使用；客户端 `handleTunnel` 不对隧道流发 FIN，只把读尽传播成 `link.Writer.Close()`。半关闭后对端 FIN 与未读缓冲的安全由 yamux 原生保证（流完全关闭后 `Read` 仍先交付缓冲再 EOF），定向回归见 `internal/tcp/halfclose_test.go`，参考 `internal/transport/forward.go` 的 `ProxyBidirectional`、[docs/adr/0023-reinstate-server-half-close.md](docs/adr/0023-reinstate-server-half-close.md)、[docs/adr/0024-switch-smux-to-yamux.md](docs/adr/0024-switch-smux-to-yamux.md) 与 [docs/adr/0025-close-instead-of-closewrite.md](docs/adr/0025-close-instead-of-closewrite.md)。
7. 带 `sync.Mutex`/`sync.Cond` 的结构体禁止值拷贝（`go vet` 的 copylocks 检查），需要共享一律传指针。
8. 新增并发代码必须：
   - 通过 `go test -race`；
   - 配可重复执行的测试（`-count=N` 或压力参数）；
   - 覆盖取消/提前结束路径，而不只是 happy path。

## 5. 日志规范

- 库代码禁止 `fmt.Print*`；所有日志走 `internal/logging`，命令入口只允许向 `os.Stderr` 输出最终错误。
- 级别语义：
  - `Debug`：载荷、字节内容、单次搬运细节；
  - `Info`：连接建立、路由选择、请求/响应完成等生命周期事件；
  - `Warn`：单个请求失败但会话可继续；
  - `Error`：组件级故障。
- 只用包级静态调用：`logging.Infof(...)` 或 `logging.Warn("msg", "err", err)`（kv 交替），不持有 logger 对象、不做 `Named`/`With` 派生。模块归属用消息前缀表达（`"forward: ..."`、`"v2ray: ..."`）；上下文关联字段（`remote`、`stream_id`、`destination`、`outbound`）在调用点按需作为 kv 参数传入。
- 默认级别不得打印请求/响应载荷；载荷内容只允许出现在 Debug 级别。
- 禁止记录任何凭据、令牌等敏感字段。当前协议无认证，未来加入后此条由安全评审把关。

## 6. 测试规范

### 6.1 基本要求

- 优先使用标准库 `testing`；只有在断言/ mocking 明显降低维护成本时才评估引入 testify，且需全项目统一，不允许只在单个包里引入。
- 测试用例多于两个分支时用表驱动 + `t.Run` 子测试。
- 每个包的测试既测 happy path，也测错误路径（拨号失败、协议错误、取消、EOF）。`TestClientDialFailure` 是拨号失败路径的范例，新功能参照这个粒度补错误路径。
- **测试不允许依赖公网**：不连百度、不依赖外部 DNS。需要目标服务时用 `net.Listen("127.0.0.1:0")` 起本地服务，或用 `net.Pipe` / `httptest`。
- 不允许依赖固定端口；随机端口从 listener 的实际地址取。
- 禁止靠固定 sleep 等条件成立，等 channel、等 EOF、轮询短超时可以；超时时长至少为最慢 CI 环境留 2 倍余量（本机环境差异——如 macOS cgo 解析器——也在考虑范围内）。
- **依赖时间行为的测试**（心跳、超时）把周期配成很小的值（如 20ms）来压缩等待，再用短超时轮询等结果落地，不要靠长 sleep；断言只校验数值落在合理区间，不锁死具体时延。参考 `TestClientProxyHeartbeat`。
- 无共享状态依赖的测试鼓励 `t.Parallel()`；依赖日志输出的测试注意串行化或自行隔离。

### 6.2 并发测试

- 数据面改动必须本地跑过 `go test -race ./...`。
- 并发路径的测试必须可重复：`go test -short ./internal/transport -count=20` 是提交前命令，出现一次 flake 即视为缺陷，不允许重跑掩盖。加 `-short` 是为了跳过 128 MiB 的端到端下载测试——重复性检查针对的是并发路径，不必把大传输跑 20 遍。
- flaky 测试的修法优先级：消除时序假设 > 同步信号 > 放宽超时；不允许简单地把超时调大来"治"flaky，除非根因就是环境耗时。

### 6.3 命名

- 测试函数 `TestXxx`/`TestXxx_Yyy`；辅助函数以 `newTestXxx` / `mustXxx` 命名，失败时调用 `t.Helper()` + `t.Fatal`。
- fixture 就近放在测试文件或 `testdata/` 目录；`testdata/` 只放测试数据。

## 7. API 与协议的兼容性

- 仓库未发布 v1 前，`core` 的导出接口仍允许演进，但任何破坏 `Dispatcher`/`OutboundHandler` 签名的改动必须在 PR 与 CHANGELOG 中显式标注 `BREAKING`。
- 线路协议（`protocol` 中的 DMP 二进制帧）变更：**魔数不含版本、两端不做协商**（见 [ADR 0014](docs/adr/0014-fixed-header-layout.md)），所以改线格式等于硬切——旧对端会在 `bad magic` 处被拒。因此协议格式的说明以 [docs/PROTOCOL.md](docs/PROTOCOL.md) 为准，**改协议必须同步改文档、且必须写一篇 ADR 说明不兼容范围**（对应「旧 ADR 不删、新增标注 supersedes」的规矩）。
- **消息类型编号只增不改**：新增类型在末位追加（当前 `Data=1` / `DataResult=2` / `Error=3` / `Ping=4` / `Pong=5` / `Auth=6` / `AuthResult=7` / `Forward=8` / `ForwardResult=9` / `Tunnel=10` / `TunnelResult=11` / `UserAuth=12`），不复用历史上被删除类型的编号；`validType` 用显式枚举而非区间判断，避免空号被误判为合法。v1.0.0 之前若有调整，必须在 ADR 中记录编号变更。
- v1.0.0 之后遵循[语义化版本](https://semver.org/lang/zh-CN/)：破坏性变更升 major，向后兼容的功能升 minor，修复升 patch。

## 8. 文档要求

- 行为变更与文档变更在同一个 PR 中提交。以下内容改动时必须同步 README：命令行参数、协议格式、模块职责表、范围与限制。
- 重要的架构取舍记录到 `docs/adr/NNNN-title.md`（架构决策记录）：为什么反向开流、管道为什么有 Close/Interrupt 两种语义、协议为什么选行文本等。决策被推翻时不删旧 ADR，新增一篇标注 supersedes。
- 示例命令保证可直接复制运行。

## 9. 依赖管理与安全

- `go.mod` / `go.sum` 由 `go mod tidy` 生成，CI 校验工作区干净；不手改 indirect 行。
- 升级依赖在独立 PR 中完成，标题标注 `chore(deps)`，不与功能改动混合。
- 本项目定位是教学演示：**无认证、无目标白名单，禁止把监听暴露到公网**。所有可能让用户误以为可直接生产使用的改动，都必须同步检查 README "范围与限制" 一节的表述。
- 发现安全问题走私下披露（邮件维护者），不直接提公开 issue。

## 10. Git 与提交规范

采用 [Conventional Commits](https://www.conventionalcommits.org/)：

```text
<type>(<scope>): <subject>

type: feat / fix / refactor / perf / test / docs / chore / ci
scope: core | server | pipe | cmd | app | acl | manager | logging | ci（可省略）
```

- subject 用祈使句、首字母不大写、句末无句号，建议 50 字符以内；正文说明 why 与影响面。
- 破坏性改动在页脚写 `BREAKING CHANGE: ...`。
- 分支：`main` 始终保持可发布、CI 全绿；开发分支命名 `feat/xxx`、`fix/xxx`、`docs/xxx`。
- 提交历史在合并前整理成可读序列（rebase/squash 视 PR 情况），不合并 "fix typo"、"address review" 这类噪声提交。

## 11. PR 与评审

- PR 描述包含：**动机、方案、影响面、测试方式**；修 bug 先描述复现路径。
- CI 必须全绿：格式、`go mod tidy` 干净、`go vet`、`-race` 测试、server 重复测试。
- 合并前的自查清单：
  - [ ] 改动符合分层依赖方向，没有给内核包引入业务概念
  - [ ] 新起的每个 goroutine 都有退出路径，资源有 defer 清理
  - [ ] 错误被包装或显式忽略，没有裸返回/吞错
  - [ ] 新增导出标识符有 godoc 注释，行为变更同步了 README/ADR
  - [ ] 新增代码有错误路径测试；并发测试 `-race` 与 `-count=20` 本地通过
  - [ ] 没有引入公网依赖、固定端口、固定 sleep
  - [ ] 日志级别合理，默认级别不打印载荷
- 评审文化：评论对事不对人；`nit:` 前缀表示不阻塞合并的小建议；作者对所有评论显式 resolve 或回复后再请求复审。

## 12. 开发环境

- Go 版本：以 `go.mod` 声明为准（当前 1.22）；本地允许更新版本开发，但不得使用新版本专属语法导致旧版无法构建。
- 提交前最小命令集（后续会由 Makefile 封装）：

```sh
gofmt -s -l cmd internal
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
go test -race ./...
go test -short ./internal/transport -count=20 -timeout=60s
```

## 13. 迭代节奏

- 用 milestone 组织迭代，每个里程碑有明确主题与退出标准，路线图见 [docs/ROADMAP.md](docs/ROADMAP.md)。
- 一个里程碑内的 PR 顺序：规范/测试先行 → 机制实现 → 文档收尾；不把未经测试的机制合入 main。
- 大改动先出 ADR 或在 issue 中讨论方案再写代码。
