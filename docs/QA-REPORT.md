# QA 终验报告 — UniRate LLM 频控网关

**验证者**：QA（独立终验，未参与本轮任何实现）
**验证时间**：2026-08-23 23:14 – 23:35
**验证范围**：本轮三项工作（性能优化 / 内嵌管理控制台 / 部署流程与安全加固）
**结论**：**PASS，可交付**（生产就绪档位 **Silver**）

本报告遵循 `verifier-critic-pattern`：验证由不写该实现的视角执行，只回答一个问题——
**这次是把问题解决了，还是把验证绕过了？**

---

## 一、硬闸门结果

| 闸门 | 标准 | 结果 | 实测证据 |
|------|------|------|----------|
| 限流精度 | 500 并发打 `limit=50`，恰好通过 50、零误差 | **PASS** | 独立脚本三轮：`200=50 / 429=384`、`200=50 / 429=337`、`200=50 / 429=329`；官方压测三轮均 `通过=50 拒绝=450`。共 **6 轮独立测量全部恰好 50** |
| limiter 测试 | 29 项全 PASS、0 SKIP | **PASS** | `internal/limiter` 29 个测试函数（源码计数）→ 运行 29 项 PASS、**0 SKIP**。`REDIS_ADDR/REDIS_PASSWORD/REDIS_REQUIRED` 三变量齐备，无静默跳过 |
| e2e | 50/50 | **PASS** | `验收结果: 通过 50 / 失败 0` |
| race | 全包无数据竞争 | **PASS** | 全包 `-race`：**160 PASS / 0 FAIL / 0 SKIP / 0 DATA RACE** |
| Redis 故障期 | panic 计数 0 | **PASS** | 独立复现：停 Redis → 120 并发请求（含 rate+concurrency+token 三类规则）→ `panic=0`。降级行为正确：并发规则本地保守配额恰好放行 20（`max_concurrent=20`），`/ready` 仍 200 不误摘流，`/health` 正确报 `degraded` |

限流精度这一项按 lead 要求做了**三次独立测量**（我的脚本 3 轮 + 官方 loadgen 3 轮），
结果完全一致。这是 P0-1 配额放大 + P0-5 计数器污染均已修复的直接证据。

### 全包测试分布

| 包 | 测试项 | 覆盖率 | 断言数 |
|----|--------|--------|--------|
| `admin` | 46 | 47.9% | 85 |
| `adminui` | 24 | 82.2% | 56 |
| `config` | 16 | 40.2% | 49 |
| `limiter` | **29** | **81.2%** | 90 |
| `meta` | 5 | 88.5% | 10 |
| `meter` | 7 | 88.0% | 20 |
| `obs` | 9 | **100%** | 29 |
| `proxy` | 19 | 25.3% | 44 |
| `upstream` | 5 | 80.5% | 14 |
| **合计** | **160** | **52.7%** | **397** |

---

## 二、变异测试记录

**目的**：本轮测试数从 13 涨到 29，但数量不等于有效性。手动破坏关键防御逻辑，
验证对应测试是否真的变红。lead 已验过 `batch_check.lua` 的 `noop` 占位与
`key.go` 的 `winSec<=0` 除零防御，因此我换角度，优先挑**看起来没有测试守护的路径**。

| # | 破坏点 | 破坏方式 | 测试是否变红 | 结论 |
|---|--------|----------|--------------|------|
| 1 | `adminui/gzip.go` `acceptsGzip` | 退化为 `strings.Contains(header,"gzip")` 子串匹配 | **变红** | `TestGzipRejectedWhenQZero` 捕获 4 个用例（`gzip;q=0`、`gzip; q=0`、`gzip;q=0.0`、`deflate, gzip;q=0`）；`TestGzipAcceptedViaQValueForms` 捕获 `*` 通配漏判 |
| 2 | `adminui/adminui.go` `Vary` 头 | 从分支前移入压缩分支内 | **变红** | `TestVaryHeaderAlwaysPresent` 捕获 `Accept-Encoding=""` 与 `identity` 两种情形缺 Vary |
| 3 | `admin/server.go` `extractToken` | 退回 `strings.TrimPrefix(auth,"Bearer ")` | **变红** | 3 个测试捕获，其中 `TestBareTokenInAuthorizationRejected` 报「裸令牌无方案名：必须 401，实际 503 —— **存在鉴权绕过**」 |
| 4 | `limiter/scripts/batch_check.lua` 两阶段提交 | fixed 分支退回「先 INCR 再判断」 | **变红** | 5 个测试捕获，其中 `TestConcurrentExactLimit` 报 `concurrent limit must be exact: want 50, got 25`；`TestTkAdmitRejectDoesNotConsumeOtherQuota` 报「被拒请求污染了其他规则计数器（两阶段不变量被破坏）」 |
| 5 | `admin/server.go` `/admin/metrics` 路由 | 去掉 `s.auth` 中间件 | **变红** | `TestAuthRequired` 与 `TestMetricsAuthOuterThanDependencyGuard` 捕获，报「无凭证必须 401，实际 503 —— auth 未在最外层」 |

**变异测试通过率 5/5。** 测试套件具备真实的缺陷捕获能力，29 项不是靠凑数堆出来的。

**恢复确认**：全部变异点用 `cp` 备份并逐个 `diff -q` 校验恢复（4 处 `IDENTICAL`），
`grep -rn MUTATION internal/` **零残留**，最终 `gofmt` 干净、`go build ./...` `BUILD_OK`、`go vet ./...` `VET_OK`。

### 未被测试守护的路径（advisory，非 blocking）

统计关键函数在测试代码中的引用次数，以下为零引用：

| 函数 | 位置 | 风险评估 |
|------|------|----------|
| `ipAllowed` | `admin/server.go:240` | 行为已被 `TestCIDRAllowlist` 间接覆盖（黑名单外 403 / 白名单内放行），可接受 |
| `contentType` | `adminui/adminui.go:102` | 注释记载「Windows 上 .js 注册为 text/plain 导致控制台白屏」是实测坑，但无测试锁定该行为 |
| `decodePolicyValues` | `admin/policy_codec.go` | 通过 `/admin/policy/validate` 端点测试间接覆盖 |
| `cmd/gateway/main.go` | 327 行装配逻辑 | 覆盖率 0%，完全依赖 e2e。这是有意的取舍（装配代码单测价值低），但 `/admin/metrics` 的 `Metrics` 注入正是在此处，属于「一行漏写就静默失效」的位置 |

---

## 三、测试完整性反作弊门

对比开发前后测试 surface，逐项检测 5 类作弊：

| 检测项 | 结果 | 详情 |
|--------|------|------|
| 测试文件/用例被删 | **通过** | 测试文件数 22 个，无删除痕迹 |
| 断言数下降 | **通过** | 397 个断言（`t.Error/Errorf/Fatal/Fatalf`），各包分布均衡，限流器 90 个最多 |
| 新增 skip/xfail/.only | **通过** | 全仓仅 1 处 `t.Skipf`（`limiter_test.go:57`），且带 `REDIS_REQUIRED=1` 逃生舱可将其转为硬失败。实测 **0 SKIP** |
| 断言硬编码实现输出 | **通过** | 抽查 limiter 测试，断言值来自 Spec 定义的 limit/window 语义，非实现返回值。变异测试 5/5 变红反证断言具备区分力 |
| 测试框架配置篡改 | **通过** | `.golangci.yml` mtime 14:31（早于本轮开发），`Makefile` test 目标含 `-race -count=1 -covermode=atomic`，未见阈值下调。lint 豁免仅 2 处且有明确理由（`ResponseWriter.Write` 响应已发出后无从处理；`_test.go` 放宽 errcheck/bodyclose/noctx） |

**结论：反作弊门通过，绿测可信。**

---

## 四、鉴权边界完整性

穷举 `internal/admin/` 全部注册路由（9 个 `mux.HandleFunc` + 1 个 `mux.Handle`），
逐个验证无凭证响应：

| 端点 | 无凭证 | Content-Type | 判定 |
|------|--------|--------------|------|
| `/admin/bizs` | 401 | `application/json` | 正确 |
| `/admin/bizs/{biz}` | 401 | `application/json` | 正确 |
| `/admin/reload` | 401 | `application/json` | 正确 |
| `/admin/snapshot` | 401 | `application/json` | 正确 |
| `/admin/audit` | 401 | `application/json` | 正确 |
| `/admin/rules/validate` | 401 | `application/json` | 正确 |
| `/admin/policy` | 401 | `application/json` | 正确 |
| `/admin/policy/validate` | 401 | `application/json` | 正确 |
| `/admin/metrics` | **401** | `application/json` | 正确（详见 §六） |
| `/admin/nonexistent` | **404 JSON** | `application/json` | 正确——拼错端点不会伪装成 200+HTML |
| `/admin`（无斜杠） | **404 JSON** | `application/json` | 正确 |
| `/`、`/index.html`、`/app.js` | 200 | `text/html` / `text/javascript` | 设计如此（静态壳不鉴权，否则无处输入令牌） |

**无遗漏鉴权的数据端点。** 未注册路径返回 JSON 404 而非 HTML，`ui.go:44-48` 的
兜底逻辑正确工作。

### 静态资产机密扫描

用扩展关键词（`password|secret|api_key|mysql|dsn|redis://|private_key|sk-|ghp_|Basic <b64>|10.x|192.168.x|127.0.0.1` 等）
扫描磁盘资产 + **通过 HTTP 实取全部 25 个资产再扫**（防 embed 与磁盘不一致）：

命中项全部是**提示文案里的变量名**，无任何值泄露：
- `index.html:51` 「令牌由部署时的 ADMIN_TOKEN 提供，仅存于本页会话」
- `page-login.js:74` 「令牌无效。请核对部署配置中的 ADMIN_TOKEN」
- `api.js:11` `var TOKEN_KEY = 'unirate_admin_token'`（localStorage 键名）

**无机密泄露。**

---

## 五、前端安全独立验证

不采信前端自述报告，自行构造载荷并在**真实浏览器**（Chromium via agent-browser）验证。

### 载荷注入

通过 Admin API 将载荷写入 SoT，覆盖全部渲染面：

| 注入面 | 载荷 | 落库确认 |
|--------|------|----------|
| `X-Operator` 头 | `<img src=x onerror=window.__XSS_OP__=1>` | 已落库（`operator` 字段） |
| 规则名 | `<svg onload=window.__XSS_RULE__=1></svg>evil` | 已落库 |
| `base_url` | `http://mock-upstream:9000/"><script>window.__XSS_URL__=1</script>` | 已落库 |
| policy 变更 operator | `<script>window.__XSS_POLICY__=1</script>` | 已落库（`detail` 字段） |
| CSV 公式注入 | `=cmd\|' /C calc'!A1` | 已落库 |

### 浏览器实测结果

登录控制台后逐页 `eval` 检测 DOM：

| 页面 | 4 个探针 | `img[onerror]` | `svg[onload]` | 内联 `<script>` |
|------|----------|----------------|---------------|-----------------|
| `#/audit` | 全 `null` | 0 | 0 | 1（index.html 主题防白闪脚本，非注入） |
| `#/rules` | 全 `null` | 0 | 0 | 1（同上） |
| `#/config` | 全 `null` | 0 | 0 | 1（同上） |

载荷全部以**纯文本**形式呈现，例：
```
operator_cells: [" <img src=x onerror=window.__XSS_OP__=1>", " =cmd|' /C calc'!A1",
                 " <script>window.__XSS_POLICY__=1</script>"]
```

**结论：XSS 防护有效，零注入节点。**

### `operator` 字段专项确认（lead 追问未获答复的点）

代码级追溯 `operator` 的全部渲染路径：
- `app.js:182` → `el.textContent = API.session.operator()`
- `page-audit.js:123` → `U.el('span', {...}, [U.icon('i-user'), ' ', it.operator || 'unknown'])`
  走 children 数组 → `dom.js:append` → `document.createTextNode`
- `page-audit.js:46` → 仅用于 `indexOf` 筛选，不渲染

**`operator` 字段确认安全**，与 `detail` 同等防护级别。

### 渲染基座审计

`dom.js` 是唯一建节点入口，安全契约由代码强制而非约定：
```javascript
else if (k === 'html') throw new Error('innerHTML is forbidden: use text');
```
全仓 `innerHTML` 仅 1 处实际使用（`app.js:28` 注入编译期固定的 SVG sprite，非用户数据）。

### CSV 公式注入防护

`page-audit.js:157` `csvCell` 对 `^[=+\-@\t\r]` 开头的单元格前置单引号。
实测载荷 `=cmd|' /C calc'!A1` 被正确处理。**防护到位。**

---

## 六、`/admin/metrics` 端点专项（lead 点名遗留项）

镜像已重建（`2026-08-23T15:29:47Z`，晚于全部相关源码 mtime），端点已真实接线：

| 检查项 | 期望 | 实测 | 判定 |
|--------|------|------|------|
| 路由注册 | `server.go:120` | 已注册，中间件链 `auth → allowMethods → requireMetrics` | 正确 |
| `main.go` 注入 | `admin.Options{Metrics: metrics}` | `main.go:204` 已注入 | 正确 |
| 无凭证 | 401 | **401** + `{"error":"unauthorized"}` | 正确 |
| 带凭证 | 200 + Prometheus 文本 | **200**，`text/plain; version=0.0.4` | 正确 |
| 与 obs 端口一致 | 逐字节相同 | admin **14347** 字节 = obs **14347** 字节，14 个指标名集合完全一致 | 正确 |
| HEAD 无正文 | `size=0` | **200 size=0** | 正确 |
| 写方法 | 405 | POST/PUT/DELETE/PATCH **全 405** | 正确 |
| 安全头 | `no-store` + `nosniff` | `Cache-Control: no-store`、`X-Content-Type-Options: nosniff` | 正确 |

**该端点验证通过，8/8。**

> 说明：早前一次探测该端点返回 404，原因是当时运行的是旧镜像（构建于
> `15:02:30`，而 `metrics.go` mtime 为 `23:06:16`）。镜像重建后行为正确。
> 这条记录保留，用于说明「容器行为 ≠ 源码行为」，验证时必须核对镜像时间戳。

---

## 七、前端 gzip 验证（lead 裁决项）

| 检查项 | 结果 |
|--------|------|
| `Content-Encoding: gzip` | **存在** |
| `Vary: Accept-Encoding` | **存在**（压缩与非压缩两条路径都带，`adminui.go:180` 放在分支前） |
| `gzip;q=0` 正确拒绝 | **是**（无 `Content-Encoding` 行） |
| `*` 通配正确接受 | **是** |
| 压缩效果 | `app.js` 9041 → 3634 字节；全量 25 资产 199,958 → 74,102（**2.70x**） |

> 压缩比与 lead 裁决时的实测 3.4x（199,804 → 58,696）有差异，原因是统计口径：
> 我按 HTTP `Content-Length` 累加，**含未达 512B 阈值不压缩的小文件**（`gzipMinSize=512`）。
> 单看大文件（`app.js` 2.49x、总文本资产）与其一致。不构成缺陷。

---

## 八、配置热更新正确性

### 上下限校验时机（关键：400 而非 503）

12 个越界/非法值全部返回 **400**，**无一例 503**，证明校验在写库前完成：

| 载荷 | HTTP | 错误信息 |
|------|------|----------|
| `max_request_body_mb: 9999` | 400 | `must be within [1, 256], got 9999` |
| `max_request_body_mb: 0` | 400 | `must be within [1, 256], got 0` |
| `upstream_timeout: "999m"` | 400 | `must be within [1s, 10m0s], got 16h39m` |
| `upstream_timeout: "1ms"` | 400 | `must be within [1s, 10m0s], got 1ms` |
| `config_poll_interval: "1s"` | 400 | `must be within [5s, 5m0s]` |
| `config_poll_interval: "10m"` | 400 | `must be within [5s, 5m0s]` |
| `log_level: "trace"` | 400 | `must be one of debug/info/warn/error` |
| `instances: 0` | 400 | `must be within [1, 1024], got 0` |
| `instances: 99999` | 400 | `must be within [1, 1024], got 99999` |
| `token_flush_interval: "1ms"` | 400 | `must be within [100ms, 10s]` |
| `token_flush_interval: "60s"` | 400 | `must be within [100ms, 10s]` |
| `expose_rule_name: "notabool"` | 400 | `invalid boolean "notabool" (want true/false)` |

边界情形（`{}`、`{"values":{}}`、`notjson`）亦全部 400。
`reset` 未知键 400 `invalid reset keys`——拼错键名不会静默无效。

代码印证：`policy.go:54` 的 `ValidatePolicyOverrides` 在 `policy.go:80` 的
`s.db == nil` 守卫**之前**执行，且守卫刻意放在 handler 内而非中间件层
（GET 在 SoT 抖动时仍可读配置）。

### `overridden_by_env` 三态准确性

lead 的追问是「能否区分『env 未设置』与『env 设成与默认值相同的值』」。**能。**

| key | value | default | env_value | source | `overridden_by_env` | compose 实际 |
|-----|-------|---------|-----------|--------|---------------------|--------------|
| `upstream_timeout` | 1m0s | 1m0s | 1m0s | env | **true** | `UPSTREAM_TIMEOUT: 60s`（= 默认值） |
| `max_request_body_mb` | 32 | 32 | 32 | default | **false** | 未设置 |

`upstream_timeout` 的 env 值 60s 恰好等于默认 1m0s，三个值完全相同，
但仍正确报 `overridden_by_env: true`；`max_request_body_mb` 未设置报 `false`。

机制：`policy_spec.go:162` 的 `EnvExplicitlySet` 用 `os.LookupEnv` 判存在性，
**不比对值**。这是唯一正确的实现方式——比对值必然无法区分这两种情形。

### 热更新真实生效

用 `expose_rule_name` 验证（效果直接可观测）：

```
初始 true  → 打到 429，X-Ratelimit-Rule: qa-hot-rule   （头存在）
改为 false → 3 秒后请求，(规则名头已消失)                 （头消失）
改回 true  → 3 秒后 429，X-Ratelimit-Rule: qa-hot-rule  （头恢复）
```

**全程未重启**：`docker inspect` 前后均为
`started=2026-08-23T15:02:37Z restarts=0`。
网关日志两次 `runtime policy updated` + `config hot-reloaded via pubsub`。

### 禁改键防护

14 个变体（7 个键 × 大写/小写）全部返回 **400 `unknown config key`**：

`ADMIN_ALLOW_CIDRS`、`ALLOW_HEADER_UPSTREAM`、`UPSTREAM_ALLOWLIST`、
`TRUSTED_PROXY_HOPS`、`REAL_IP_HEADER`、`TOKEN_HEADERS`、`TZ_OFFSET_SECONDS`
及其小写形式。

**无法通过 `/admin/policy` 修改**，白名单机制（`config.PolicyKeys`）而非黑名单，
将来新增键不会意外开放。

---

## 九、部署文档可执行性

照 `docs/DEPLOYMENT.md` §2「部署后验证」逐条实跑（未覆盖 `.env`，避免打断他人工作）：

| 章节 | 命令 | 判定标准 | 实测 | 结果 |
|------|------|----------|------|------|
| 2.1 | `curl $OBS/live` | 200 | 200 | PASS |
| 2.1 | `curl $OBS/ready` | 200 | 200 | PASS |
| 2.1 | `curl $OBS/health` | `redis.ok=true, version>0, biz_count>=2` | `True / 4 / 5` | PASS |
| 2.1 | `docker compose ps --format '{{.Status}}'` | 4 容器 healthy | **命令报错** | **FAIL**（见 P2-2） |
| 2.2 | `curl $PROXY/demo/v1/chat/completions` | 200 + `total_tokens` | 200，含该字段 | PASS |
| 2.3 | 25 并发打 `limit=10` | 200 落 8~13 且 429>0 | `200=10 429=15` | PASS |
| 2.4 | 无凭证 / 占位值 / 真令牌 | 401 / 401 / 200 | 401 / 401 / 200 | PASS |
| 2.5 | 业务端口访问 admin / metrics | 非 200 | 502 / 502 | PASS |
| 2.6 | `docker compose ps --format '{{.Ports}}'` | 只见 127.0.0.1:29090 | **命令报错** | **FAIL**（见 P2-2） |
| 2.7 | `/admin/policy` items 数 | 7 | 7 | PASS |
| 2.7 | validate `50ms` | 400 | 400 | PASS |
| 6.1 | `grep unirate_redis_breaker_open` | 0 | `unirate_redis_breaker_open 0` | PASS |
| 6.1 | `grep unirate_degraded_total` | 有输出 | **空** | **FAIL**（见 P2-1） |

**文档整体质量高**：每条命令都有明确判定标准，无「确保配置正确」这类无法执行的空话。
`§2.3` 特别注明「必须并发：串行 curl 会跨窗口边界」并给出两种失败模式的区分诊断，
`§6.4` 的排查链每步都有判据。发现 2 处技术性错误（均 P2）。

`make init` 未实跑（会覆盖 `.env` 打断其他人），改为静态审阅：
`scripts/init-env.sh` 逻辑正确（`.env` 存在则拒绝覆盖、`openssl rand -base64 24` 生成、
`chmod 600`），文档 §1.2 对「为何拒绝覆盖」的解释（凭证与已有数据卷不匹配）准确。

---

## 十、P0 规则全量扫描

| 规则 | 扫描范围 | 结果 |
|------|----------|------|
| emoji 作功能图标 | 129 文件（`internal/**` `docs/**` `cmd/**` `test/**` `README.md` `Makefile` `docker-compose.yml` `Dockerfile` `.env.example` `scripts/**` `deploy/**`），字符范围含变体选择符 `\uFE00-\uFE0F`、`\u20E3`、`\U0001FA00-\U0001FAFF` | **零命中** |
| 紫粉渐变 | `internal/` `docs/design/`，关键词 `purple\|pink\|A855F7\|EC4899\|D946EF\|from-purple\|to-pink` | **零命中**（2 处命中均为 DESIGN.md/design-tokens.json 中的**禁令声明本身**） |
| AI 模板味 | `welcome to\|lorem ipsum\|sign up today\|your one-stop\|unleash\|elevate your` | **零命中** |
| 弹跳缓动 | `cubic-bezier(0.68\|bounce` | **零命中** |

> 扫描时我把字符范围扩到了 `\u2190-\u21FF`（箭头）与 `\u2B00-\u2BFF`，
> 命中 262 处全部是 `→` 箭头字符（README 架构图、压测输出格式化）。
> 箭头是排版符号不是 emoji 图标，不计入违规。图标全部来自 `icons.svg` sprite，
> `dom.js:icon()` 是唯一图标入口。

---

## 十一、失效模式核对（6 类）

| 失效模式 | 结果 | 依据 |
|----------|------|------|
| Happy-path 偏差 | **通过** | 异常路径有测试：Redis 故障降级（`degrade_test.go` 212 行）、越界配置、鉴权失败、并发竞争、上游超时。e2e 含 Redis 停机恢复全流程 |
| 沉默逻辑错误 | **通过** | 变异测试 4 直接验证：破坏两阶段提交后 `TestTkAdmitDoesNotBreakLaterRuleCommit` 报「plans 空洞使 Phase 2 提前终止」——这类静默错误有测试守护 |
| 幻觉依赖/接口 | **通过** | `go.mod` 直接依赖数量少（`go-redis`、`modernc.org/sqlite`、`otel` 等），`go build` 全绿，无不存在的 API 调用 |
| 缺失系统上下文 | **通过** | 多实例场景被显式处理：`metrics.go` 顶部论证了「为何不做后端预聚合」（多实例采样基线不一致）；`instances` 配置用于降级期配额分摊 |
| 性能盲区 | **通过** | 有压测基线（4 个 JSON 归档）、ADR-008 定义了判定阈值、`evalsha=11.35µs` 有埋点、双侧 CPU 归因强制要求 |
| 静默缺失 | **部分**（见 P2-1） | 文档引用的 `unirate_degraded_total` 指标不存在，排障时会拿到空输出而无任何报错——这正是「静默缺失」的形态，只是发生在文档侧而非代码侧 |

---

## 十二、发现的缺陷

### P0 致命：**0 个**

### P1 严重：**0 个**

### P2 一般：3 个

#### P2-1 部署文档引用了不存在的指标名

**证据**：
```bash
$ grep -n 'degraded_total' docs/DEPLOYMENT.md
468:curl -s $OBS/metrics | grep unirate_degraded_total
571:curl -s $OBS/metrics | grep -E 'unirate_degraded_total|breaker_open'

$ curl -s $OBS/metrics | grep unirate_degraded_total
（空）

$ curl -s $OBS/metrics | grep -i degrad
unirate_degraded_decisions_total{biz="demo",mode="reject"} 13
```
实际指标名是 `unirate_degraded_decisions_total`（`obs/registry.go:53` 定义为
`newCounterVec("degraded_decisions_total", ...)`）。

**期望**：文档命令能返回降级计数。

**影响**：`§6.1 Redis 挂了会怎样` 与 `§6.6 429 过多` 两处排障流程。
运维照文档执行会得到空输出，可能误判为「没有发生降级」，
而这恰恰是文档自己强调「必须纳入告警」的关键信号。属于静默失效。

**复现步骤**：
1. `source .env && OBS=http://127.0.0.1:${OBS_PORT}`
2. `curl -s $OBS/metrics | grep unirate_degraded_total`
3. 观察：无输出、无报错

**修复建议**：`docs/DEPLOYMENT.md:468` 与 `:571` 的指标名改为
`unirate_degraded_decisions_total`。

#### P2-2 部署文档的 `docker compose ps --format` 命令在当前 Docker 版本不可执行

**证据**：
```bash
$ docker compose ps --format '{{.Status}}'
format value "{{.Status}}" could not be parsed: parsing failed

$ docker compose ps --format '{{.Ports}}'
format value "{{.Ports}}" could not be parsed: parsing failed
```

**期望**：文档 §2.1（判断容器是否 healthy）与 §2.6（确认 admin 端口只绑回环）
的命令可复制执行。

**影响**：§2.6 是**安全验证项**——确认管理面没有对全网暴露。命令报错会让执行者
跳过这项检查，或误以为环境有问题。

**复现步骤**：
1. `cd` 到项目根目录
2. 执行 `docker compose ps --format '{{.Ports}}'`
3. 观察报错

**修复建议**：改用兼容写法，例如
`docker ps --filter name=unirate --format '{{.Names}}\t{{.Ports}}'`
或 `docker compose ps --format json | python3 -m json.tool`。
§2.6 的「从另一台机器验证」那条建议本身是可执行且更可靠的，可提为主要手段。

#### P2-3 `cmd/gateway` 装配代码零覆盖

**证据**：`go test -cover` 显示 `cmd/gateway coverage: 0.0% of statements`（327 行）。

**期望**：至少对关键注入点有守护。

**影响**：`/admin/metrics` 的 `Metrics` 注入就在 `main.go:204`。若这一行被误删，
端点会返回 503 而非 401——所有 `internal/admin` 的单测仍会全绿（测试用
`newTestServer` 自行装配 Options），只有 e2e 或人工才能发现。
本轮该端点确实经历过「代码写好但镜像未重建 → 404」的状态，说明这条链路脆弱。

**修复建议**（非本轮）：加一条 e2e 断言，验证 `/admin/metrics` 带凭证返回 200
且与 obs 端口输出一致。当前 e2e 的 §G 只检查 obs 端口的指标。

---

## 十三、已知遗留状态确认

| 遗留项 | lead 要求 | 我的确认结果 |
|--------|-----------|--------------|
| `/admin/metrics` 端点 | 重点验证 | **8/8 全部通过**（详见 §六）。路由注册、`main.go` 注入、401、200、字节级一致、HEAD、405、安全头 |
| 前端 gzip | 验 `Content-Encoding` 与 `Vary` 都在 | **两者都在**（详见 §七）。另验了 `q=0` 拒绝与 `*` 接受 |
| 6 个文件超 300 行 | 已登记技术债，本轮不拆，不作为 blocking | **未作为 blocking**。但需报告：验证期间 `server.go` 被拆成了 `biz.go` + `audit.go`（详见 §十四），与此裁决冲突 |
| `hotreload_test.go` flaky | 连跑 15 次以上 | **连跑 25 次零失败、零数据竞争**。`TestApplyPolicy*` / `TestOptionsSnapshotIsCopy` / `TestConcurrentPolicyUpdateAndRead` / `TestExposeRuleNameGovernsHeaderLeak` 全部稳定。**未复现** |

---

## 十四、验证期间的环境事件（非产品缺陷，但影响可追溯性）

两次事件打断验证，均非我的操作，记录以保证结论可审计：

**事件 1（23:14–23:15）工作区一度不可编译。**
我的第一条 `go build ./...` 报 `s.handleBizs undefined`，11 秒后重跑报
`server.go:293:1: syntax error: unexpected EOF`。原因是 `internal/admin/` 下
新出现 `biz.go`（mtime 23:14:21，与我读取同秒），`server.go` 同时被截短——
有人正在把 `server.go` 拆分。23:15:38 后收口，编译恢复正常。

这与 lead 的「本轮不拆」裁决冲突。**我按收口后的状态（`server.go` + `biz.go` + `audit.go`）
完成全部验证，所有结论对应这一状态**，最终 `go build` / `go vet` / `gofmt` 全绿。

**事件 2（23:26:50）全栈被外部停止。**
容器全部 `Exited (0)` 优雅退出，网关日志 `shutdown signal received`。
我全程只发 HTTP 请求与跑容器化 `go test`，未执行任何 compose 生命周期命令；
`test/perf/loadgen` 与 `test/perf/run.sh` 均无 teardown 逻辑（已 grep 确认）。
数据卷未丢失。我自行 `docker compose up -d` 恢复后补跑了受影响的两项
（限流精度、压测），两项均 PASS。

**值得肯定**：`loadgen` 在环境不可达时报
`健康检查失败，拒绝产出垃圾数据` 并 `exit 1`，而不是产出一份看起来正常
但毫无意义的数据。这个 fail-fast 设计避免了一次错误结论。

---

## 十五、生产就绪评级

按 `production-readiness-scorecard.md` 七维 × 三档评分。**总档取各维最低档。**

| 维度 | 档位 | 证据 | 距 Gold 差什么 |
|------|------|------|----------------|
| 测试 + 回归 | **Gold** | 160 测试 0 失败 0 跳过、race 干净、**变异测试 5/5 变红**、e2e 50/50、限流精度 6 轮独立验证恰好 50、**独立测试作者**（QA 写测试、开发按测试实现）、hotreload 25 次稳定 | 已达 Gold |
| 契约 | **Silver** | Admin API 契约明确（9 端点 + 方法白名单 + 统一 JSON 错误模型 `{"error":...}`）、`overridden_by_env` 三态语义在文档与代码注释双向对齐、`/admin/metrics` 与 obs 端口字节级一致 | 契约破坏未在 CI 即红（无契约测试）；无版本与弃用策略 |
| 安全 | **Silver** | 三重隔离（回环监听 + Bearer 常量时间比较 + CIDR 白名单）、弱令牌黑名单拒绝启动、**XSS 真实浏览器实测零注入**、CSV 公式注入防护、CSP 全方向锁死、SSRF 开关刻意不可页面改、静态资产零机密、审计与变更同事务 | 无 SAST/依赖扫描门；无供应链加固（未见 SBOM/签名）；无威胁面定期复核 |
| 无障碍 | **Silver** | 全交互元素有 `aria-label`（快照可见 `region "操作反馈"`、`navigation "模块导航"`、`group "自动刷新间隔"`）、状态不单靠颜色承载（`dom.js:STATUS` 同时输出图标形状+文字+颜色）、语义化 landmark（banner/main/navigation）、键盘可达 | 无屏读端到端走查；无自动化 a11y 扫描进门禁；对比度未实测 |
| 性能 | **Silver** | 压测基线归档（4 个 JSON）、ADR-008 预声明阈值、QPS 中位数 23,293 / p99 20.64ms、`evalsha=11.35µs` 埋点、双侧 CPU 归因强制、gzip 2.70x、批量 Lua 合并往返消除双 RTT | 归因显示「两侧均未饱和，瓶颈可能在客户端」——**容量结论尚未成立**（需专用压测环境）；无退化阻断门 |
| 可观测 | **Silver** | 14 个 Prometheus 指标、结构化 JSON 日志、`/live` `/ready` `/health` 三探针语义分离、熔断状态可观测、审计日志完整、直方图桶单点定义 | 无 SLI/SLO 定义；无告警规则入库（文档说「必须纳入告警」但未提供规则）；无分布式追踪 |
| 发布安全 | **Silver** | 可回退（文档 §5.2 有步骤）、`make migrate` 幂等、优雅关闭 + `SHUTDOWN_GRACE`、`/ready` 摘流、升级前强制备份、配置降级保留最后有效快照 | 无渐进发布/金丝雀；回滚预案未演练；无特性开关；无自动回滚触发 |

### 总档：**Silver**

**达到商业级交付最低线。** 短板集中在「加固」层面（CI 契约门、供应链扫描、
SLO 与告警规则、渐进发布），这些属于平台/流程建设，不是本轮功能范围。

七维**无一维停留在 Bronze**，测试维度达 Gold。

---

## 十六、放行结论

### verdict: **PASS — 可交付**

**判据**：
1. **5 个硬闸门全部 PASS**，限流精度经 6 轮独立测量确认零误差
2. **P0 缺陷 0 个、P1 缺陷 0 个**
3. **变异测试 5/5 变红**——绿测经得起「测试真能捕获缺陷吗」的拷问
4. **反作弊门通过**——0 SKIP、397 断言、框架配置未被篡改，绿灯不是靠改松验证换来的
5. **生产就绪 Silver**，七维无 Bronze 短板
6. 前端 XSS、鉴权边界、配置热更新三项**均由我独立复验**，未采信任何自述报告
7. 工程状态干净：`gofmt` 无输出、`go build ./...` OK、`go vet ./...` OK

### 3 条 P2 建议在下一轮处理

按 lead 的过度设计护栏，这 3 条我只标 P2 不标阻断——它们都不构成正确性缺陷、
需求未满足或契约安全数据完整性破坏：

- **P2-1**（文档指标名错误）建议本轮顺手改掉，一行的事，且影响的是安全告警链路
- **P2-2**（compose 命令不兼容）建议本轮改掉，§2.6 是安全验证项
- **P2-3**（装配代码零覆盖）建议下一轮加一条 e2e 断言，不必本轮做

### 需要 lead 裁决的一件事

验证期间 `server.go` 被拆成了 `biz.go` + `audit.go`，与「本轮不拆」的裁决冲突。
拆分本身质量没问题（编译、测试、vet 全绿，我的全部结论都基于拆分后的状态），
但**收口后改动生产代码这个动作本身**需要你确认是否批准。若未批准，
建议按你原裁决回滚，我可在回滚后重跑受影响的 `admin` 包测试（46 项，约 8 秒）。

---

## 附录：回归集建议

本轮 P0 缺陷 0 个，无需新增回归用例。但变异测试暴露的 4 个已有测试值得标记为
**回归保护关键项**，任何时候变红都意味着历史缺陷复现：

| 测试 | 守护的历史缺陷 | 文件 |
|------|----------------|------|
| `TestConcurrentExactLimit` | P0-1 配额放大 / P0-5 计数器污染 | `internal/limiter/limiter_test.go` |
| `TestTkAdmitDoesNotBreakLaterRuleCommit` | `plans` 空洞导致 Phase 2 静默跳过 | `internal/limiter/tkadmit_test.go` |
| `TestBareTokenInAuthorizationRejected` | `TrimPrefix` 鉴权绕过 | `internal/admin/server_test.go` |
| `TestVaryHeaderAlwaysPresent` | 缺 Vary 致中间层缓存回放乱码 | `internal/adminui/gzip_test.go` |

建议在 CI 中将这 4 项标为不可跳过（例如独立 job 或注释标记），
它们对应的都是「修过一次、再犯就是回归」的缺陷。
