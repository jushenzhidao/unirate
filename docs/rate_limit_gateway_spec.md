# 通用频控代理网关 — 技术规格书 v1.0.0-alpha.1

> 生成时间: 2026-08-23 01:44 +08:00
> 定位: 纯频控能力的智能反向代理网关，通用、透传、动态路由

---

## 1. 概述

### 1.1 项目定位

一个**纯频控能力的智能反向代理网关**，位于客户端与上游服务之间，承担流量关卡职责。

核心特征：
- **通用**：不耦合任何业务逻辑，规则完全配置化
- **透传**：不解析/修改业务报文体，只读元数据（Header、Path、Method 等）
- **动态路由**：基于 `/{biz}/{path}` 模式，支持配置面板 → 请求头 → 环境变量三级上游解析
- **零鉴权侵入**：鉴权信息（Token、API Key 等）完全透传，不做任何校验

### 1.2 核心职责边界

```
  Client        频控代理网关        上游服务
    │                │                │
    │───────────────▶│───────────────▶│
    │                │                │
    │◀───────────────│◀───────────────│
    │                │                │
```

**只做三件事**：
1. 频控检查
2. 代理转发
3. 水位监控

**明确不做的事**：鉴权、参数校验、业务 Body 解析（除 Token 计量所需的极轻量元数据提取）、响应改写、协议转换、熔断降级（仅保留自身过载保护）。

### 1.3 关键术语

| 术语 | 说明 |
|------|------|
| `biz` | 业务域标识，从 URL Path 第一段提取，如 `/order/v1/create` 中 `biz=order` |
| `dimension` | 频控维度，如 `global`、`biz`、`path`、`token`、`ip`、`method` |
| `window` | 时间窗口，如 `1s`、`5m`、`1h`、`1d`、`1w` |
| `inflight` | 飞行中请求数，用于并发控制 |
| `watermark` | 水位百分比，当前使用量 / 配额上限 |
| `token`（大写 T）| 鉴权令牌，从请求头提取 |
| `token`（小写 t）| 大模型 Token 消耗量 |

---

## 2. 解决方案

### 2.1 路由机制

#### URL 模式

```
/{biz}/{path}

示例:
/order/v1/create     → biz=order, path=/order/v1/create
/user/profile        → biz=user, path=/user/profile
```

#### 解析规则

1. 提取 `biz`：取 Path 第一个 Segment（按 `/` 分割后的第一段）
2. 合法性校验：`biz` 只允许 `[a-zA-Z0-9_-]`，非法字符 → `400`
3. 空 `biz`（如 `/v1/create`）→ `400`，明确告知 URL 格式错误
4. **大小写统一转小写**处理
5. **路径透传**：下游收到的也是 `/{biz}/{path}`，不做重写（除非配置 `path_strip_prefix: true`）

#### 路径重写策略（可选）

| 模式 | 转发给下游的 Path | 场景 |
|------|-------------------|------|
| 保留原路径 | `/order/v1/create` | 默认，透传语义最干净 |
| 去掉 biz 前缀 | `/v1/create` | 下游独立部署，不知自身所属 biz |
| 自定义映射 | 配置映射表 | 历史系统迁移 |

### 2.2 上游 Base URL 三级解析

针对 `biz`，按优先级确定上游地址：

| 优先级 | 来源 | 机制 | 场景 |
|--------|------|------|------|
| **P1** | **Redis / MySQL 配置面板** | 运行时查询配置中心 | 线上动态切流、灰度、故障转移 |
| **P2** | **请求头** | 读取 `X-Upstream-Base-URL` | 测试调试、压测指定节点 |
| **P3** | **环境变量** | 启动时加载 `BIZ_{biz大写}` | 默认兜底、静态部署 |

#### 解析流程

```
提取 biz = order
    │
    ▼
[1] 查 Redis / MySQL 配置表
    ├── 命中 → 返回动态上游地址
    └── 未命中
        │
        ▼
[2] 读请求头 X-Upstream-Base-URL
    ├── 存在且合法 → 返回（生产环境可关闭此来源）
    └── 不存在
        │
        ▼
[3] 查环境变量 BIZ_ORDER
    ├── 命中 → 返回静态配置
    └── 未命中 → 返回 502 Bad Gateway
```

#### 配置面板表结构

```sql
CREATE TABLE biz_upstream (
    biz         VARCHAR(64) PRIMARY KEY,
    base_url    VARCHAR(512) NOT NULL,
    path_strip_prefix BOOLEAN DEFAULT FALSE,
    enabled     TINYINT DEFAULT 1,
    updated_at  TIMESTAMP,
    INDEX (enabled, updated_at)
);
```

Redis 等效存储：`HSET gateway:biz_upstream order http://order-svc:8080`

### 2.3 频控策略

#### 2.3.1 规则类型

| 类型 | 说明 | 典型场景 |
|------|------|---------|
| **速率限流 (rate)** | 限制单位时间内的请求数或 Token 数 | API QPS 保护、Token 预算控制 |
| **并发控制 (concurrency)** | 限制同时进行的请求数量 | 长耗时任务防重入（支付、模型推理）|

#### 2.3.2 维度系统

##### 预定义维度

```
global      → 全局总量
biz         → 业务域
path        → 请求路径
token       → 鉴权令牌（SHA256 前16位作为 Key）
ip          → 客户端 IP
method      → HTTP 方法（GET/POST/PUT/DELETE）
```

##### 组合维度表达式

用 `+` 连接多个维度，表示**笛卡尔组合限流**：

| 表达式 | 含义 | Redis Key 示例 |
|--------|------|----------------|
| `token` | 单 Token 限流 | `ratelimit:token:sha256_abc:1m:...` |
| `token+ip` | 同一 Token 在同一 IP 上限流 | `ratelimit:token_ip:sha256_abc_192.168.1.1:1m:...` |
| `biz+token` | 同一 Token 在指定 Biz 上限流 | `ratelimit:biz_token:order_sha256_abc:1h:...` |
| `biz+path+token` | 同一 Token 在指定接口上限流 | `ratelimit:biz_path_token:order_/v1/create_sha256_abc:1h:...` |
| `biz+path` | 接口级限流 | `ratelimit:biz_path:order_/v1/create:1m:...` |

#### 2.3.3 时间窗口

窗口表达式语法：`数字 + 单位`

| 单位 | 含义 | 示例 |
|------|------|------|
| `s` | 秒 | `1s`, `10s` |
| `m` | 分钟 | `1m`, `5m` |
| `h` | 小时 | `1h`, `5h` |
| `d` | 天 | `1d`, `7d` |
| `w` | 周 | `1w`, `2w` |

#### 2.3.4 限流算法

| 算法 | 适用窗口 | 特点 |
|------|----------|------|
| **令牌桶 (token_bucket)** | 短窗口（≤1s） | 允许突发，平滑限流 |
| **固定窗口 (fixed_window)** | 任意窗口 | 通用，性能最好，Redis 原子自增 |
| **滑动窗口 (sliding_window)** | 中窗口（1m~1h） | 精度高，无边界突发，Redis ZSet 实现 |

#### 2.3.5 分层限流架构

```
L0: 自身保护（CPU/内存/连接数过载）→ 直接 429
    │
    ├── L1: 全局总限流（保护网关集群）
    │     │
    │     ├── L2: 按 biz 限流（保护下游服务总量）
    │     │     │
    │     │     ├── L3: 按 (biz + path) 限流（热点接口）
    │     │     │     │
    │     │     │     ├── L4: 按客户端维度限流（Token/IP 等）
    │     │     │     │     │
    │     │     │     │     └── 全部通过 → 转发
```

**执行原则**：任何一层触发，立即返回 `429`，不继续后续检查，不转发请求。

#### 2.3.6 存储分层策略

| 层级 | 维度 | 存储 | 理由 |
|------|------|------|------|
| L0 | 自身保护 | 本地内存 | 纳秒级，零外部依赖 |
| L1/L2 | 全局/biz | 本地内存 + Redis 兜底 | biz 数量有限，全量缓存 |
| L3 | biz+path | 本地缓存热点 + Redis | path 数量可控时本地，否则 Redis |
| L4 | 客户端维度 | Redis 为主 | Token/IP 无限，本地无法全量 |
| 并发控制 | 飞行中计数 | Redis INCR/DECR | 跨实例一致性必须 |


### 2.4 Token 消耗频控（大模型场景）

#### 2.4.1 核心矛盾

透传网关不解析 Body，但 Token 数通常藏在响应体 JSON 的 `usage` 字段中，SSE 流式更是逐 chunk 输出。

#### 2.4.2 三种响应场景的采集策略

| 场景 | 响应格式 | Token 来源 | 采集方式 | 精度 |
|------|---------|-----------|---------|------|
| **非流式 JSON** | `{"choices":[...],"usage":{"total_tokens":847}}` | 响应体 `usage` 字段 | 网关轻量解析 JSON 顶层 | 精确 |
| **SSE 流式** | `data: {"choices":[{"delta":{"content":"..."}}]}` | 无统一 `usage`，或最后一条 | 按 chunk 内容实时估算 | 近似 |
| **上游自定义** | 任意格式 | 上游自行回传 | 配置化提取规则 | 取决于上游 |

#### 2.4.3 非流式 JSON：轻量 Body 解析

**透传原则修正**：
- 不修改 Body 内容
- 不解析业务结构（choices/messages）
- **仅读取顶层元数据字段（`usage.total_tokens`）** — 破例，但严格限定范围

在响应完全返回后，做一次性轻量解析：

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "model": "gpt-4",
  "choices": [...],
  "usage": {
    "prompt_tokens": 120,
    "completion_tokens": 727,
    "total_tokens": 847
  }
}
```

**解析规则配置化**：

```yaml
token_extraction:
  content_type: "application/json"
  path: "$.usage.total_tokens"
  fallback: "$.usage.completion_tokens"
```

**性能保证**：
- 只扫描到 `usage` 字段出现即停止，不解析 `choices` 数组
- 使用流式 JSON 解析器，避免全量加载大 Body
- 解析失败 → 进入估算模式，不阻断请求

#### 2.4.4 SSE 流式：Chunk 实时估算（核心方案）

**原理**：按每个 SSE chunk 的 `delta.content` 文本长度，实时估算 Token 数。

```
估算公式（简化版）：
tokens ≈ length(utf8_text) * 0.3   # 英文约 0.3，中文约 0.6，混合取 0.4

更精确的编码估算：
- 使用 tiktoken 分词器（或兼容的 BPE 编码）
- 对每个 chunk 的 content 做实时分词
- 累加得到总 Token 数
```

**实现流程**：

```
Client <--- 实时转发 chunk ---> 网关 <---- 接收上游 chunk
            ^                           |
            |                           +-- 解析 chunk 的 delta.content
            |                               +-- tiktoken 分词 -> 累加计数器
            +-- 同时写入 Redis INCRBY（异步，不阻塞转发）
```

**关键保证**：
- 分词和计数是**异步旁路**，不影响 chunk 透传延迟
- 计数器用本地内存累加，SSE 结束后一次性刷到 Redis（减少 Redis 压力）
- 如果 SSE 连接异常断开，用本地已累加值刷盘，不丢失
- 估算值 × 1.2 安全缓冲系数后再扣减，防止估算偏低导致超卖

**SSE 结束后的精确值补扣（OpenAI 兼容模式）**：

如果上游是 OpenAI 兼容接口，最后一条 data 可能包含 `usage`：

```
data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":727,"total_tokens":847}}
data: [DONE]
```

网关维护 SSE 帧解析状态机：
- 检测到含 `usage` 的帧 → 提取精确值，用精确值覆盖估算值
- 如果没检测到 → 回退到估算值

#### 2.4.5 Token 采集配置模型

```yaml
token_metering:
  mode: auto                    # auto / json_body / sse_estimate / header / disabled

  json_body:                    # 模式1: JSON 响应体提取
    path: $.usage.total_tokens
    required: false
    fallback_path: $.usage.completion_tokens

  sse_estimate:                 # 模式2: SSE 实时估算
    encoder: cl100k_base        # tiktoken 编码名
    estimate_ratio: 0.4         # 无编码器时的字符->token 估算系数
    accumulate_local: true      # 本地累加，SSE 结束后再刷 Redis
    safety_buffer: 1.2          # 安全缓冲系数

  header:                       # 模式3: 响应头提取（上游配合时）
    name: X-Usage-Tokens

  fixed_estimate:               # 模式4: 固定值估算
    per_request: 1000
```

#### 2.4.6 校准机制

运行一段时间后，对比**估算值**与上游实际返回的 `usage`（采样请求做全量解析）：

```
校准因子 = 实际总 Token / 估算总 Token
每周自动更新校准因子，缩小长期漂移。
```

### 2.5 并发控制（In-Flight Limiting）

#### 2.5.1 与速率限流的本质区别

| | 速率限流 | 并发控制 |
|--|---------|---------|
| 限制对象 | 单位时间内的请求数量 | 同一时刻正在处理的请求数量 |
| 典型场景 | API QPS 保护 | 长耗时任务防重入 |
| 计数方式 | 请求进入时 +1 | 请求进入时 +1，完成时 -1 |
| 泄漏风险 | 无（窗口到期自动清零） | **有**（如果 -1 没执行，计数器永久泄漏）|

#### 2.5.2 实现机制

**飞行中计数器**：

```
请求进入:
  Redis INCR  concurrency:token:sha256_abc:llm:inflight
  如果返回值 > max_concurrent -> 拒绝 429（或排队等待）
  否则 -> 放行，开始透传

请求完成（无论成功/失败/超时）:
  必须执行 Redis DECR 释放计数器
```

**防泄漏兜底**：

| 兜底策略 | 机制 | 代价 |
|---------|------|------|
| **Redis TTL** | Key 设置 30s 过期，自然清零 | 30s 内可能误判 |
| **本地定时心跳** | 每 5s 扫描本地飞行中请求，超时强制 DECR | 需维护超时表 |
| **请求级超时** | 每个请求绑定超时时间，超时后自动释放 | 最精确，推荐 |

**建议组合**：Redis TTL（30s）+ 请求级超时（如 120s）双重兜底。

**拒绝 vs 排队**：
- **直接拒绝**（默认）：返回 429，客户端自己重试。简单、无状态。
- **排队等待**：请求进入队列，等前面任务完成再转发。需要维护队列，有状态，复杂度高。

**建议默认直接拒绝**，排队作为高级可选功能。

#### 2.5.3 典型配置

```yaml
- name: 单用户并发锁
  type: concurrency
  dimensions: [token]
  max_concurrent: 1
  timeout: 120s        # 120s 后强制释放
```

### 2.6 水位监控

#### 2.6.1 定义

```
水位 = 当前窗口内已使用量 / 配额上限 * 100%
```

#### 2.6.2 数据采集路径

| 规则类型 | 数据来源 | 延迟 | 精度 |
|---------|---------|------|------|
| 本地令牌桶/窗口 | 直接读取内存计数器 | 实时 | 精确 |
| Redis 固定窗口 | 定期轮询 GET 或请求后异步更新 | 5~10s | 精确 |
| Redis 滑动窗口 | ZCARD 查询 | 5~10s | 精确 |
| 并发控制 | 读取飞行中计数器 | 实时 | 精确 |

#### 2.6.3 告警分级

| 水位 | 状态 | 动作 |
|------|------|------|
| < 60% | 安全 | 正常记录 Metrics |
| 60%~80% | 注意 | 面板黄色提示 |
| 80%~95% | 告警 | 推送告警（钉钉/企业微信/Webhook），面板红色 |
| >= 95% | 危险 | 紧急告警 + 可选自动收紧（下调 20% 配额作为缓冲）|

#### 2.6.4 与自动扩缩容联动

水位持续高位（如连续 3 个窗口 > 90%）可触发：
1. **告警通知**：通知业务方评估是否需要扩容
2. **自动提额**：如果配置了弹性配额，临时提升 20%（需有总预算上限）
3. **熔断降级**：如果下游已明显过载，临时收紧限流保护上游

### 2.7 高可用与自我保护

| 场景 | 策略 |
|------|------|
| 配置中心（Redis/MySQL）宕机 | 使用本地缓存的最后有效配置，继续服务 |
| Redis 宕机（影响 L3/L4） | L0/L1/L2 本地频控继续工作；L3/L4 Fail-Open 或保守限流 |
| 上游不可达 | 直接透传上游返回的错误（502/504），网关不包装 |
| 自身 CPU/内存过载 | L0 自我保护触发，CPU>80% 或连接数超阈值时直接 429 |
| 请求头指定非法上游地址 | 忽略或返回 400，防止 SSRF |
| SSE 异常断开 | 用本地已累加 Token 值刷盘，不丢失计数 |
| 计数器泄漏 | Redis TTL + 请求级超时双重兜底 |

---

## 3. 详细架构

### 3.1 系统架构

```
+-----------------------------------------------------------------------------+
|                              客户端层                                        |
|         Web App / Mobile / 第三方服务 / 大模型 SDK                           |
+-----------------------------------------------------------------------------+
                                      |
                                      v
+-----------------------------------------------------------------------------+
|                           负载均衡层（Nginx/云 SLB）                         |
|                    轮询 / 最少连接 / 一致性哈希                               |
+-----------------------------------------------------------------------------+
                                      |
                         +------------+------------+
                         |            |            |
                  +------v------+ +---v----+  +-----v-----+
                  |  网关实例-1  | | 实例-2  |  |  实例-N   |
                  |  (无状态)    | |(无状态) |  | (无状态)   |
                  +------+------+ +---+----+  +-----+-----+
                         |            |            |
                         +------------+------------+
                                      |
  +----------------------------------------------------------------------------+
  |                         配置中心 + 分布式频控存储（Redis Cluster）          |
  |  +-----------------+  +-----------------+  +-----------------+  +----------+ |
  |  |  biz_upstream   |  |   biz_limits    |  |  rate counters  |  |concurrency| |
  |  |  (Hash)         |  |   (Hash/JSON)   |  |  (String/INCR)  |  |  counters   | |
  |  +-----------------+  +-----------------+  +-----------------+  +----------+ |
  +----------------------------------------------------------------------------+
                                      |
                                      v
  +----------------------------------------------------------------------------+
  |                                    上游服务群                                |
  |  +---------+  +---------+  +---------+  +---------+  +---------------------+ |
  |  | order   |  |  user   |  | payment |  |   llm   |  |     ...             | |
  |  | -svc    |  |  -svc   |  |  -svc   |  |  -svc   |  |                     | |
  |  +---------+  +---------+  +---------+  +---------+  +---------------------+ |
  +----------------------------------------------------------------------------+
```

### 3.2 请求处理完整数据流

```
Client: POST /llm/v1/chat/completions
        Header: Authorization: Bearer eyJhbG...
                X-Forwarded-For: 192.168.1.1
    |
    v
[1] 解析元数据
    biz=llm, path=/llm/v1/chat/completions, method=POST
    token=SHA256(eyJhbG...)[:16]=a3f9b2c1
    ip=192.168.1.1
    |
    v
[2] 解析上游地址（P1 配置面板 > P2 请求头 > P3 环境变量）
    base_url = http://llm-svc:8080
    |
    v
[3] 加载该 Biz 的规则列表
    规则列表: [biz:1s:令牌桶], [token:1h:固定窗口], [concurrency:token:1], [global:1m:Token:令牌桶]
    |
    v
[4] 逐条执行限流检查
    |
    +- 规则1: concurrency:token -> Redis INCR
    |   飞行中 2 / 3 -> 通过
    |
    +- 规则2: biz:1s -> 本地令牌桶
    |   通过
    |
    +- 规则3: token:1h -> Redis 固定窗口
    |   通过
    |
    +- 规则4: global:1m:Token -> 本地令牌桶（预检）
        通过
    |
    v
[5] 全部通过 -> 转发请求
    URL: http://llm-svc:8080/llm/v1/chat/completions
    Header: 全量透传（含 Authorization）
    Body: 流式透传
    |
    v
[6] 上游返回 SSE 流式响应
    |
    +- 逐 chunk 实时转发给 Client
    |   |
    |   +-- 旁路: 解析 delta.content -> tiktoken 分词 -> 本地累加
    |
    +- SSE 结束
    |   |
    |   +-- 旁路: 一次性 Redis INCRBY(累加值 * 1.2 缓冲系数)
    |   +-- 如果最后 frame 含 usage -> 用精确值覆盖估算值
    |
    v
[7] 释放并发计数器
    Redis DECR concurrency:token:a3f9b2c1:llm:inflight
    |
    v
[8] 更新水位监控数据
    |
    v
[9] 响应原样透传给 Client（已完成）
```

### 3.3 模块设计

#### 3.3.1 接入层（Ingress）

- **协议支持**：HTTP/1.1、HTTP/2、gRPC over HTTP/2、WebSocket、SSE
- **连接模型**：异步非阻塞，基于 goroutine（Go 场景）
- **TLS 终结**：由网关做 TLS 终结，读取 HTTP 头做路由和频控

#### 3.3.2 规则引擎（Rule Engine）

- 从本地缓存加载该 biz 的规则列表
- 按优先级排序，逐条执行
- 任何一条触发即拒绝

#### 3.3.3 频控决策层（Rate Limiter）

- **本地令牌桶**：L0/L1/L2，纳秒级判断
- **Redis 固定窗口**：L3/L4，Lua 原子脚本
- **Redis 滑动窗口**：高精度场景，ZSet 实现
- **并发锁**：Redis INCR/DECR + TTL 兜底

#### 3.3.4 Token 计量层（Token Meter）

- **JSON 解析器**：轻量顶层扫描，只读 `usage` 字段
- **SSE 解析器**：帧解析状态机，旁路分词累加
- **分词器**：BPE 编码实现（Go 自研或 Rust FFI）
- **校准器**：周期性对比估算值与实际值，更新校准因子

#### 3.3.5 路由转发层（Proxy）

- 连接池维护（对上游长连接）
- Body 流式透传（Request/Response 均不加载内存）
- Header 全量透传，追加代理标识头（可选）

#### 3.3.6 配置管理层（Config Manager）

- 定时从 Redis/MySQL 拉取配置
- 本地全量缓存，秒级热更新
- 配置校验（格式、冲突检测）
- 失败时保留旧配置并告警

#### 3.3.7 监控层（Monitor）

- Metrics 暴露（Prometheus 格式）
- 水位实时计算与告警
- Tracing（TraceID 注入下游 Header）
- 日志（决策日志：规则、频控结果、路由目标、耗时）


### 3.4 存储设计

#### 3.4.1 Redis Key 规范

| 用途 | Key 格式 | 示例 |
|------|---------|------|
| 全局限流 | `ratelimit:global:{window}:{boundary}` | `ratelimit:global:1s:20250823010000` |
| Biz 限流 | `ratelimit:biz:{biz}:{window}:{boundary}` | `ratelimit:biz:order:1m:202508230100` |
| 接口限流 | `ratelimit:biz_path:{biz}_{path}:{window}:{boundary}` | `ratelimit:biz_path:order_/v1/create:1h:2025082300` |
| Token 限流 | `ratelimit:token:{hash}:{window}:{boundary}` | `ratelimit:token:a3f9b2c1:1h:2025082300` |
| 组合限流 | `ratelimit:{dim1}_{dim2}:{val1}_{val2}:{window}:{boundary}` | `ratelimit:biz_token:order_a3f9b2c1:1h:2025082300` |
| 并发计数 | `concurrency:{dims}:{biz}:inflight` | `concurrency:token:llm:inflight` |
| 配置存储 | `gateway:biz_upstream` | Hash: {order: http://...} |
| 规则存储 | `gateway:biz_limits:{biz}` | Hash: {rules: json} |

#### 3.4.2 固定窗口 Lua 脚本

```lua
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local expire = tonumber(ARGV[2])

local current = redis.call('INCR', key)
if current == 1 then
    redis.call('EXPIRE', key, expire)
end

if current > limit then
    return 0
else
    return 1
end
```

#### 3.4.3 滑动窗口 Lua 脚本

```lua
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])

local min_score = now - window
redis.call('ZREMRANGEBYSCORE', key, 0, min_score)
local count = redis.call('ZCARD', key)

if count >= limit then
    return 0
else
    redis.call('ZADD', key, now, now .. '_' .. ARGV[4])
    redis.call('EXPIRE', key, window)
    return 1
end
```

#### 3.4.4 Token 扣减 Lua 脚本

```lua
local key = KEYS[1]
local tokens = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local expire = tonumber(ARGV[3])

local current = redis.call('INCRBY', key, tokens)
if current == tokens then
    redis.call('EXPIRE', key, expire)
end

if current > limit then
    return 0
else
    return 1
end
```

### 3.5 高可用设计

| 风险点 | 防护策略 |
|--------|---------|
| 单点故障 | 无状态设计，多实例部署，前置 SLB |
| 配置中心故障 | 本地缓存最后有效配置，继续服务 |
| Redis 故障 | 本地 L0~L2 继续工作；L3/L4 Fail-Open 或保守限流 |
| 上游不可达 | 直接透传上游错误码，网关不包装 |
| 自身过载 | L0 自我保护，CPU>80% 或连接数超阈值时直接 429 |
| SSE 异常断开 | 本地累加值刷盘，Redis TTL 兜底 |
| 计数器泄漏 | Redis TTL + 请求级超时双重兜底 |

---

## 4. 业务架构图

```
+----------------------------------------------------------------------------------+
|                                    客户端层                                       |
|  +---------+  +---------+  +---------+  +---------+  +-------------------------+ |
|  | Web App |  | Mobile  |  | 第三方  |  | 大模型  |  |        SDK              | |
|  |         |  |   App   |  |  服务   |  |  客户端  |  |   (Python/JS/Go)        | |
|  +----+----+  +----+----+  +----+----+  +----+----+  +------------+------------+ |
|       |            |            |            |                  |               |
|       +------------+------------+------------+                  |               |
|                              |                                 |               |
|                              |                                 |               |
|                              v                                 |               |
|  +--------------------------------------------------------------------------------+ |
|  |                              负载均衡层（Nginx/云 SLB）                        | |
|  |                    轮询 / 最少连接 / 一致性哈希（Token 亲和）                    | |
|  +--------------------------------------------------------------------------------+ |
|                              |                                                   |
|                   +----------+----------+                                        |
|                   |          |          |                                        |
|            +------v------+ +-v------+ +--v------+                              |
|            |  网关实例-1  | | 实例-2  | |  实例-N  |                              |
|            |  (无状态)    | |(无状态) | | (无状态) |                              |
|            +------+------+ +---+----+ +--+------+                              |
|                   |          |          |                                        |
|                   +----------+----------+                                        |
|                              |                                                   |
|  +--------------------------------------------------------------------------------+ |
|  |                         配置中心 + 分布式频控存储（Redis Cluster）              | |
|  |  +---------------+  +---------------+  +---------------+  +----------------+   | |
|  |  |  biz_upstream |  |   biz_limits  |  |  rate counters|  |concurrency     |   | |
|  |  |  (Hash)       |  |   (Hash/JSON) |  |  (String/INCR)|  |  counters      |   | |
|  |  +---------------+  +---------------+  +---------------+  +----------------+   | |
|  +--------------------------------------------------------------------------------+ |
|                              |                                                   |
|                              v                                                   |
|  +--------------------------------------------------------------------------------+ |
|  |                                    上游服务群                                  | |
|  |  +---------+  +---------+  +---------+  +---------+  +---------------------+   | |
|  |  | order   |  |  user   |  | payment |  |   llm   |  |     ...             |   | |
|  |  | -svc    |  |  -svc   |  |  -svc   |  |  -svc   |  |                     |   | |
|  |  +---------+  +---------+  +---------+  +---------+  +---------------------+   | |
|  +--------------------------------------------------------------------------------+ |
+----------------------------------------------------------------------------------+
```

---

## 5. 技术架构图

```
+----------------------------------------------------------------------------------+
|                                    客户端层                                       |
|  +---------+  +---------+  +---------+  +---------+  +-------------------------+ |
|  | Web App |  | Mobile  |  | 第三方  |  | 大模型  |  |        SDK              | |
|  |         |  |   App   |  |  服务   |  |  客户端  |  |   (Python/JS/Go)        | |
|  +----+----+  +----+----+  +----+----+  +----+----+  +------------+------------+ |
|       |            |            |            |                  |               |
|       +------------+------------+------------+                  |               |
|                              |                                 |               |
|                              v                                 |               |
|  +--------------------------------------------------------------------------------+ |
|  |                              负载均衡层（Nginx/云 SLB）                        | |
|  |                    轮询 / 最少连接 / 一致性哈希（Token 亲和）                    | |
|  +--------------------------------------------------------------------------------+ |
|                              |                                                   |
|                   +----------+----------+                                        |
|                   |          |          |                                        |
|            +------v------+ +-v------+ +--v------+                              |
|            |  网关实例-1  | | 实例-2  | |  实例-N  |                              |
|            |  (无状态)    | |(无状态) | | (无状态) |                              |
|            |             | |        | |        |                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  |Ingress | | | |Ingress| | |Ingress|                              |
|            |  | 接入层  | | | | 接入层| | | 接入层|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Rule   | | | | Rule | | | Rule |                              |
|            |  | Engine | | | |Engine| | |Engine|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Rate   | | | | Rate | | | Rate |                              |
|            |  |Limiter | | | |Limiter| | |Limiter|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Token  | | | |Token | | |Token |                              |
|            |  | Meter  | | | |Meter | | |Meter |                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Proxy  | | | |Proxy | | |Proxy |                              |
|            |  | 转发层  | | | |转发层| | |转发层|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Config | | | |Config| | |Config|                              |
|            |  | Manager| | | |Manager| | |Manager|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            |  | Monitor| | | |Monitor| | |Monitor|                              |
|            |  +--------+ | | +----+ | | +----+ |                              |
|            +------+------+ +---+----+ +--+------+                              |
|                   |          |          |                                        |
|                   +----------+----------+                                        |
|                              |                                                   |
|  +--------------------------------------------------------------------------------+ |
|  |                         配置中心 + 分布式频控存储（Redis Cluster）              | |
|  |  +---------------+  +---------------+  +---------------+  +----------------+   | |
|  |  |  biz_upstream |  |   biz_limits  |  |  rate counters|  |concurrency     |   | |
|  |  |  (Hash)       |  |   (Hash/JSON) |  |  (String/INCR)|  |  counters      |   | |
|  |  +---------------+  +---------------+  +---------------+  +----------------+   | |
|  +--------------------------------------------------------------------------------+ |
|                              |                                                   |
|                              v                                                   |
|  +--------------------------------------------------------------------------------+ |
|  |                                    上游服务群                                  | |
|  |  +---------+  +---------+  +---------+  +---------+  +---------------------+   | |
|  |  | order   |  |  user   |  | payment |  |   llm   |  |     ...             |   | |
|  |  | -svc    |  |  -svc   |  |  -svc   |  |  -svc   |  |                     |   | |
|  |  +---------+  +---------+  +---------+  +---------+  +---------------------+   | |
|  +--------------------------------------------------------------------------------+ |
+----------------------------------------------------------------------------------+
```

### 模块交互关系

```
  Client Request
       |
       v
  +---------+     +---------+     +---------+     +---------+
  | Ingress |---->|  Rule   |---->|  Rate   |---->|  Proxy  |----> Upstream
  |  接入层  |     | Engine  |     | Limiter |     | 转发层   |
  +---------+     +---------+     +---------+     +---------+
                      ^               ^
                      |               |
                  +---------+     +---------+
                  | Config  |     |  Token  |
                  | Manager |     |  Meter  |
                  +---------+     +---------+
                      ^
                      |
                  +---------+
                  | Monitor |
                  +---------+
```

---

## 6. 接口文档

### 6.1 运行时接口（网关对外暴露）

#### 6.1.1 代理入口

```
ANY /{biz}/{path}
```

**请求**：
- Method: 任意 HTTP 方法（GET/POST/PUT/DELETE/PATCH/...）
- Path: `/{biz}/{path}`，`biz` 为业务域标识
- Headers: 全量透传，包括 `Authorization`、`Content-Type` 等
- Body: 流式透传，不解析

**响应**：
- Status: 上游返回的原生状态码，或网关生成的错误码
- Headers: 全量透传上游响应头
- Body: 流式透传

**网关可能返回的错误码**：

| 状态码 | 场景 | 响应体 |
|--------|------|--------|
| `400` | URL 格式错误（非法 biz、空 biz 等） | `{"error":"invalid biz format"}` |
| `429` | 频控触发（任何层级） | `{"error":"rate limit exceeded","rule":"{rule_name}","retry_after":{seconds}}` |
| `502` | 无可用的上游地址 | `{"error":"no upstream available"}` |
| `503` | 网关自身过载保护触发 | `{"error":"gateway overloaded"}` |

#### 6.1.2 健康检查

```
GET /health
```

**响应**：
```json
{
  "status": "healthy",
  "version": "1.0.0",
  "uptime": "72h15m",
  "connections": 1247,
  "cpu_percent": 12.5,
  "memory_mb": 128
}
```

#### 6.1.3 Metrics 暴露（Prometheus）

```
GET /metrics
```

**暴露的指标**：

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `gateway_requests_total` | Counter | `biz`, `path`, `status` | 总请求数 |
| `gateway_request_duration_seconds` | Histogram | `biz`, `path` | 请求延迟 |
| `gateway_rate_limit_hits_total` | Counter | `biz`, `rule_name`, `dimension` | 频控触发次数 |
| `gateway_rate_limit_watermark` | Gauge | `biz`, `rule_name` | 当前水位百分比 |
| `gateway_concurrency_inflight` | Gauge | `biz`, `dimension` | 当前飞行中请求数 |
| `gateway_token_usage_total` | Counter | `biz`, `token_source` | Token 消耗总量 |
| `gateway_upstream_errors_total` | Counter | `biz`, `upstream`, `error_type` | 上游错误数 |

### 6.2 管理接口（配置面板调用）

#### 6.2.1 查询 Biz 列表

```
GET /admin/biz
```

**响应**：
```json
{
  "biz_list": [
    {"biz": "order", "upstream": "http://order-svc:8080", "rules_count": 5},
    {"biz": "user", "upstream": "http://user-svc:8080", "rules_count": 3},
    {"biz": "llm", "upstream": "http://llm-svc:8080", "rules_count": 4}
  ]
}
```

#### 6.2.2 查询 Biz 规则

```
GET /admin/biz/{biz}/rules
```

**响应**：
```json
{
  "biz": "llm",
  "upstream": "http://llm-svc:8080",
  "rules": [
    {
      "id": 12,
      "name": "LLM 全局Token分钟限流",
      "type": "rate",
      "metric": "token",
      "dimensions": ["global"],
      "window": "1m",
      "limit": 500000,
      "algorithm": "token_bucket",
      "burst": 50000,
      "watermark": 85,
      "used": 420000,
      "token_source": "x-usage-tokens"
    },
    {
      "id": 14,
      "name": "单用户并发控制",
      "type": "concurrency",
      "dimensions": ["token"],
      "max_concurrent": 3,
      "timeout": 120,
      "inflight": 2
    }
  ]
}
```

#### 6.2.3 新增规则

```
POST /admin/biz/{biz}/rules
```

**请求体**：
```json
{
  "name": "单用户Token小时限流",
  "type": "rate",
  "metric": "request",
  "dimensions": ["token"],
  "window": "1h",
  "limit": 10000,
  "algorithm": "fixed_window",
  "watermark": 80
}
```

**响应**：
```json
{
  "id": 20,
  "biz": "llm",
  "name": "单用户Token小时限流",
  "created_at": "2026-08-23T01:44:00+08:00"
}
```

#### 6.2.4 删除规则

```
DELETE /admin/biz/{biz}/rules/{rule_id}
```

**响应**：
```json
{
  "deleted": true,
  "rule_id": 20
}
```

#### 6.2.5 更新上游地址

```
PUT /admin/biz/{biz}/upstream
```

**请求体**：
```json
{
  "base_url": "http://llm-svc-v2:8080",
  "path_strip_prefix": false
}

**响应**：
```json
{
  "biz": "llm",
  "base_url": "http://llm-svc-v2:8080",
  "updated_at": "2026-08-23T01:44:00+08:00"
}
```

#### 6.2.6 查询实时水位

```
GET /admin/biz/{biz}/watermark
```

**响应**：
```json
{
  "biz": "llm",
  "watermarks": [
    {
      "rule_id": 12,
      "rule_name": "LLM 全局Token分钟限流",
      "limit": 500000,
      "used": 420000,
      "percentage": 84.0,
      "status": "warning"
    },
    {
      "rule_id": 14,
      "rule_name": "单用户并发控制",
      "max_concurrent": 3,
      "inflight": 2,
      "percentage": 66.7,
      "status": "safe"
    }
  ]
}
```

### 6.3 配置模型

#### 6.3.1 Biz 配置

```yaml
biz: "llm"
upstream:
  base_url: "http://llm-svc:8080"
  path_strip_prefix: false
  enabled: true
rules:
  - id: 12
    name: "LLM 全局Token分钟限流"
    type: "rate"
    metric: "token"           # request / token
    dimensions: ["global"]
    window: "1m"
    limit: 500000
    algorithm: "token_bucket" # token_bucket / fixed_window / sliding_window
    burst: 50000              # 仅令牌桶有效
    watermark: 85             # 告警阈值百分比
    token_source: "x-usage-tokens"  # 仅 token metric 有效

  - id: 13
    name: "单用户Token小时限流"
    type: "rate"
    metric: "token"
    dimensions: ["token"]
    window: "1h"
    limit: 200000
    algorithm: "fixed_window"
    watermark: 80
    token_source: "x-usage-tokens"

  - id: 14
    name: "单用户并发控制"
    type: "concurrency"
    dimensions: ["token"]
    max_concurrent: 3
    timeout: 120              # 秒
```

#### 6.3.2 全局默认配置

```yaml
gateway:
  # 自身保护
  self_protection:
    max_cpu_percent: 80
    max_memory_mb: 2048
    max_connections: 100000

  # 上游解析
  upstream_resolution:
    sources:
      - config_panel    # P1
      - request_header  # P2
      - env_var         # P3
    request_header_name: "X-Upstream-Base-URL"
    env_var_prefix: "BIZ_"

  # Token 计量
  token_metering:
    mode: "auto"
    json_body:
      path: "$.usage.total_tokens"
      fallback_path: "$.usage.completion_tokens"
    sse_estimate:
      encoder: "cl100k_base"
      estimate_ratio: 0.4
      accumulate_local: true
      safety_buffer: 1.2

  # 监控
  monitor:
    metrics_port: 9090
    log_level: "info"
    watermark_alert_webhook: "https://hooks.example.com/alert"
```

### 6.4 Redis Key 生成规范

#### 6.4.1 速率限流 Key

```
ratelimit:{dimensions}:{dimension_values}:{window}:{window_boundary}

示例:
ratelimit:biz:order:1s:20250823010000
ratelimit:biz_path:order_/v1/create:1m:202508230100
ratelimit:token:a3f9b2c1:1h:2025082300
ratelimit:biz_token:order_a3f9b2c1:1h:2025082300
ratelimit:global:_:1m:202508230100
```

#### 6.4.2 并发控制 Key

```
concurrency:{dimensions}:{dimension_values}:{biz}:inflight

示例:
concurrency:token:a3f9b2c1:llm:inflight
concurrency:token_ip:a3f9b2c1_192.168.1.1:llm:inflight
```

#### 6.4.3 窗口边界计算

```python
def window_boundary(timestamp, window):
    unit = window[-1]
    num = int(window[:-1])

    if unit == 's':
        return str((timestamp // num) * num)
    elif unit == 'm':
        seconds = num * 60
        return str((timestamp // seconds) * seconds)
    elif unit == 'h':
        seconds = num * 3600
        return str((timestamp // seconds) * seconds)
    elif unit == 'd':
        seconds = num * 86400
        return str((timestamp // seconds) * seconds)
    elif unit == 'w':
        seconds = num * 604800
        return str((timestamp // seconds) * seconds)
```

---

## 7. 技术栈选型

### 7.1 推荐方案：Go 为主

| 模块 | 推荐方案 | 备选 |
|------|---------|------|
| **网关核心** | Go 1.22+ | Rust（团队有经验时） |
| **HTTP 框架** | 标准库 net/http + httputil.ReverseProxy | valyala/fasthttp（极致性能） |
| **SSE 解析** | 自研轻量 SSE 帧解析器（Go） | 无 |
| **Token 分词** | Go 自研 BPE 分词器 | Rust FFI（超大规模时） |
| **Redis 客户端** | redis/go-redis v9 | valkey-io/valkey-go |
| **配置中心** | etcd + viper | Consul |
| **监控** | Prometheus + Grafana | 自建 |
| **管理后台** | Python + FastAPI（独立服务） | Go 内嵌简单管理端 |

### 7.2 选型理由

**Go 作为主力**：
- 性能足够：net/http 可轻松支撑 10万+ QPS，延迟 P99 < 5ms
- 开发效率：goroutine + channel 模型非常适合代理流水线架构
- 部署友好：单二进制文件 + 静态编译，Docker 镜像 < 20MB
- 生态成熟：go-redis、prometheus client、viper 等库完善

**Token 分词器**：先用 Go 实现。只有当 profiling 证明分词是瓶颈（单实例 SSE > 5000 连接且 CPU > 80%）时，再换成 Rust FFI。过早优化是陷阱。

---

## 8. 关键设计决策总结

| 决策点 | 最终选择 | 理由 |
|--------|----------|------|
| 路由模式 | `/{biz}/{path}` 提取 `biz`，原路径透传 | 约定清晰，零路径重写逻辑 |
| 上游解析 | 配置面板 > 请求头 > 环境变量 | 动态运维优先，调试次之，静态兜底 |
| 鉴权 | 完全透传 | 服务边界清晰，不碰业务 |
| 频控分层 | L0 自身保护 -> L1 全局 -> L2 Biz -> L3 Path -> L4 客户端 | 层层防护，精准限流 |
| 频控存储 | L0~L2 本地内存，L3~L4 Redis | 热点路径本地处理，分布式场景兜底 |
| 故障策略 | 配置失效用本地缓存；Redis 失效 Fail-Open | 保证网关自身高可用，不挡业务 |
| Body 处理 | 流式透传，零解析（Token 计量除外） | 大文件友好，内存安全 |
| Token 计量 | SSE 旁路分词估算 + 结束时精确值补扣 | 兼顾透传承诺与计量精度 |
| 并发控制 | Redis INCR/DECR + TTL + 请求超时兜底 | 防泄漏，跨实例一致 |
| 技术栈 | Go 1.22+ 为主，Python 管理后台为辅 | 性能、效率、运维三角最优 |

---

*文档结束*