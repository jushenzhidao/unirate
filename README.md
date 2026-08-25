# UniRate — 统一频控网关

反向代理型 API 频控网关。业务侧零改造接入，通过 URL 首段路由到上游，在转发链路上施加多维度限流与 Token 预算控制。

本实现基于 `docs/rate_limit_gateway_spec.md`，但**并未照抄**——该 Spec 经评审为 FAIL（7 个 P0 + 4 个 P1，见 `docs/rate_limit_gateway_review.md`）。所有 P0/P1 缺陷均已在代码层修正，且每项修正都有可执行测试锁定。

## 快速开始

```bash
cp .env.example .env      # 生产环境务必修改 ADMIN_TOKEN 与各密码
docker compose up -d --build
./test/e2e/run.sh         # 端到端验收
```

带监控栈启动：

```bash
docker compose --profile obs up -d
# Grafana http://localhost:29093  Prometheus http://localhost:29092
```

调用方式：

```bash
# 原始调用            http://api.example.com/v1/chat/completions
# 接入后             http://localhost:28080/demo/v1/chat/completions
curl http://localhost:28080/demo/v1/chat/completions \
  -H 'Authorization: Bearer sk-xxx' \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'
```

## 端口职责分离

| 端口 | 用途 | 暴露范围 |
|---|---|---|
| 8080 | 业务代理 | 对外 |
| 9091 | `/metrics` `/live` `/ready` `/health` | 仅监控系统 |
| 9090 | Admin 管理 API | 仅内网 + Bearer 鉴权 |

管理面独立端口是硬性要求。原 Spec 把 `POST /admin/rules` 挂在业务端口且无鉴权，任何调用方都能改写全局限流规则。

## 对评审 P0/P1 的修正

| 编号 | 原设计问题 | 本实现方案 | 验证 |
|---|---|---|---|
| P0-1 | L1/L2 本地令牌桶 + 多实例无状态 → 实际配额放大 N 倍 | 统一 Redis 集中计数；本地仅保留「已知超限」负缓存（只会更严格，不会放宽） | `TestClusterSemantics` |
| P0-2 | 令牌桶 Key 含窗口边界 → 每窗口重建，退化为固定窗口；且与 Token 长窗口配额语义冲突 | 令牌桶 Key 去掉 boundary，改持久 Hash 桶；`token_bucket + metric=token` 组合在 `Validate()` 直接拒绝 | `TestTokenBucketPersistent` |
| P0-3 | Admin 与业务同端口且无鉴权；上游覆盖头「合法性」未定义 | Admin 独立端口 + Bearer（常量时间比较）+ CIDR 白名单 + 审计；上游校验含 scheme 白名单、内网 CIDR 黑名单、**DNS 解析后校验**（防 rebinding） | `TestSSRFBlocked` `TestDNSRebindingBlocked` |
| P0-4 | 并发计数器用 INCR/DECR，异常路径不释放 → 永久泄漏 | ZSet + `request_id` 成员 + deadline 清扫；`defer` 无条件释放 | `TestConcurrencyNoLeak` |
| P0-5 | 固定窗口先 INCR 后判断，超限不回滚 → 计数器被污染 | 两阶段单脚本原子求值：全部规则先只读试算，仅当全通过才统一写入 —— **不存在需要回滚的中间态** | `TestNoCounterPollution` |
| P0-6 | Token ×1.2 预扣只扣不退 → 预算系统性提前 20% 耗尽 | 预扣—核销—退差三段式账本，`settle` 按差额修正 | `TestSettleRefundsOverCharge` |
| P0-7 | Redis / MySQL / etcd 三套配置源并存，权威不明 | 收敛为 MySQL(SoT) → Redis(读取层+Pub/Sub) → 本地 atomic 缓存；**删除 etcd** | e2e 热更新链路 |
| P1-8 | Key 用 `_` 连接，path 自身含 `_` `/` → 可碰撞、无法反解 | 分隔符改 `|`，维度值经 `safeVal()` 编码；窗口边界统一 epoch 秒；d/w 按业务时区对齐 | `TestKeyCollision` |
| P1-9 | 直接信任 XFF → IP 维度可被随机 XFF 完全绕过 | `trusted_proxy_hops` 从右往左取值；默认 0（完全不信任）；token 来源与前缀剥离规则配置化 | `TestXFFSpoofingBlocked` |
| P1-10 | Redis 故障时 L3/L4 一律 Fail-Open → Token 预算全失效 | 按 metric 分治：`request` 可 Fail-Open，`token`/并发降级为本地保守配额（总量÷实例数） | `TestDegradeMode` |
| P1-11 | `/health` 含 Redis 连通性，与 Fail-Open 设计冲突 → 依赖抖动导致全站摘流 | `/live` 只看进程；`/ready` 只看配置；`/health` 供排障，任何情况返回 200 | e2e C 组 |

## 超出评审范围的额外修正

压测中发现一个评审未覆盖的高危缺陷：

> **偶发超时导致限流静默失效。** 500 并发打 `limit=50` 的规则，实测通过 134 个。根因不是 Lua 脚本——脚本是原子的。是连接池打满导致命令排队超时，触发 `degrade()` 走 Fail-Open 放行。评审 P1-10 只讨论了「Redis 宕机」，但生产中**偶发超时的发生频率远高于宕机**。

修复：引入熔断器区分「偶发抖动」与「真正故障」。熔断器未打开时，超时一律 Fail-Close；只有连续失败达到阈值才认定故障并进入降级。同时把超时从 50ms 放宽到 200ms、连接池提到 256。回归测试 `TestTransientTimeoutMustNotFailOpen` 锁死该行为。

## 架构

```
客户端
  │  /{biz}/{path}
  ▼
┌─────────────── 网关 (无状态，可横向扩) ───────────────┐
│ 元数据提取 → 上游解析(SSRF校验) → Token预算准入        │
│      → 限流批量原子判定(单次Redis往返) → 并发占位     │
│      → 转发 → SSE零缓冲透传 + Token增量预扣           │
│      → 结束: 精确usage核销退差 + 并发释放             │
└──────────────────────────────────────────────────────┘
      │                    │                  │
      ▼                    ▼                  ▼
   Redis              MySQL(SoT)          上游服务
 计数器/账本        配置 ← Admin写入
 配置读取层
```

限流判定的**全部规则合并为一次 Redis 往返**，单请求 RTT 与规则数解耦（解决评审 Advisory-4）。

## 规则配置

```json
{
  "name": "user-qps",
  "type": "rate",
  "metric": "request",
  "dimensions": ["biz", "token"],
  "window": "1s",
  "limit": 10,
  "algorithm": "fixed_window"
}
```

| 字段 | 取值 |
|---|---|
| `type` | `rate` / `concurrency` |
| `metric` | `request` / `token`（仅 rate） |
| `dimensions` | `global` `biz` `path` `token` `ip` `method` 的组合；`global` 不可与其他组合 |
| `window` | `1s` `5m` `1h` `1d` `2w`。`d`/`w` 按 `TZ_OFFSET_SECONDS` 自然对齐 |
| `algorithm` | `fixed_window` / `sliding_window` / `token_bucket` |

约束（加载期强制校验，非运行期）：
- `token_bucket` 不可与 `metric=token` 共用 —— Token 预算是窗口总量语义
- `sliding_window` 的 `limit` 上限 100000 —— ZSet 成员数等于窗口内请求数

## 管理 API

```bash
T="Authorization: Bearer $ADMIN_TOKEN"
curl -H "$T" http://127.0.0.1:29090/admin/bizs                  # 列出配置
curl -H "$T" -X POST http://127.0.0.1:29090/admin/rules/validate -d '[...]'  # 规则试算
curl -H "$T" -X POST http://127.0.0.1:29090/admin/bizs -d '{...}'           # 写入并热更新
curl -H "$T" -X DELETE http://127.0.0.1:29090/admin/bizs/{biz}
curl -H "$T" http://127.0.0.1:29090/admin/audit                 # 审计日志
curl -H "$T" http://127.0.0.1:29090/admin/snapshot              # 当前生效快照
```

写入路径：校验 → MySQL 事务（配置 + 审计日志同事务）→ Redis 快照 → Pub/Sub 广播 → 各实例秒级热更新。轮询兜底防 Pub/Sub 丢消息。

## 关键环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ADMIN_TOKEN` | — | **必填**，< 16 字符或为空则拒绝启动 |
| `ADMIN_ADDR` | `127.0.0.1:9090` | 默认只绑回环 |
| `TRUSTED_PROXY_HOPS` | `0` | 前置可信代理层数。0 = 不信任 XFF |
| `ALLOW_HEADER_UPSTREAM` | `false` | 是否允许请求头指定上游 |
| `UPSTREAM_ALLOWLIST` | — | 上游主机白名单，配置后仅允许列表内 |
| `TZ_OFFSET_SECONDS` | `28800` | 业务时区偏移，影响 `d`/`w` 窗口对齐 |
| `INSTANCES` | `1` | 集群实例数，用于降级时的本地配额估算 |
| `REDIS_POOL_SIZE` | `256` | 池过小会导致排队超时被误判为故障 |
| `REDIS_TIMEOUT` | `200ms` | 50ms 在压测中被证明过于激进 |
| `TOKEN_FLUSH_INTERVAL` | `1s` | SSE Token 增量刷盘间隔，决定超卖窗口大小 |

## 已知取舍

**Token 准入是「余额已耗尽则拒绝」而非硬上限。** Token 消耗只在响应后可知，因此单窗口最多超支「并发数 × 单请求消耗」。需要硬上限应在业务侧配合 `max_tokens` 参数。

**Token 估算器基于字符类型加权，不是 BPE 分词。** 估算值只用于 SSE 期间的临时预扣，结束时一律用上游 `usage` 核销。仅当上游完全不返回 usage 时才依赖估算，此时误差由 `safety_buffer` 覆盖。引入 tiktoken 移植库需内嵌数 MB 词表且不同模型词表不同，对通用网关不划算。

**Fail-Open 的适用边界。** `metric=request` 规则在 Redis 真正故障时放行，这是可用性优先的选择。若你的场景不接受任何超额，应把关键规则改为 `metric=token` 或并发类型（走本地保守配额路径）。

**指标标签不含 path 取值。** `dimension` 标签只暴露维度名组合（如 `biz.path`），不含具体取值，避免高基数打爆 Prometheus。

## 开发

本机无需 Go 工具链，全程容器内构建：

```bash
# 单元测试（需 Redis）
docker run -d --name unirate-test-redis -p 16399:6379 redis:7.2-alpine
docker run --rm -v "$PWD":/app -w /app \
  -e REDIS_ADDR=host.docker.internal:16399 \
  --add-host=host.docker.internal:host-gateway \
  -e CGO_ENABLED=1 golang:1.22-alpine \
  sh -c "apk add --no-cache gcc musl-dev && go test -race ./..."
```

CI（`.github/workflows/ci.yml`）包含：gofmt / vet / golangci-lint / go mod tidy 一致性 / race 单测 + 覆盖率 / govulncheck / gosec / 多架构镜像构建 / 完整栈 e2e。
