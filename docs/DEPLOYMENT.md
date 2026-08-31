# UniRate 部署手册

面向实际执行部署的人。每条都可复制执行，凡是要判断的地方都写清判断依据。

配置分层的判断理由见 [`decisions/CONFIG-TIERING.md`](decisions/CONFIG-TIERING.md)。

---

## 1. 首次部署

### 1.1 前置条件

| 依赖 | 要求 | 检查命令 |
|------|------|----------|
| Docker | 含 compose v2 | `docker compose version` |
| openssl | 生成凭证用 | `openssl version` |
| 空闲端口 | 28080 / 29091 / 29090 | `lsof -nP -iTCP:28080 -sTCP:LISTEN` |

本机**不需要** Go 工具链，构建在容器内完成。

### 1.2 生成凭证

```bash
make init
```

生成 `.env`（权限 600），凭证来自 `openssl rand -base64 24`。

`.env` 已存在时会**拒绝覆盖**并退出非 0。这是刻意的：新生成的 `REDIS_PASSWORD`
与正在运行的 Redis 实例不匹配，表现为网关启动后计数器读写全部失败，
而错误信息完全指不到密码上。确需重置：

```bash
mv .env .env.bak && make init
make down && docker compose up -d --build
```

`make down` 会清空 `biz_config`、`audit_log`、`runtime_config`（都在 SQLite 卷里）。
先备份，方式见 §5.1 —— **不要直接 `docker cp` 库文件**，那样拿到的备份是空的。

### 1.3 为什么没有默认凭证

`docker-compose.yml` 里所有凭证都是 `${VAR:?提示}`（必填），缺失即中止。

这不是为了增加麻烦。本项目曾给 `ADMIN_TOKEN` 配默认值
`change-me-admin-token-32chars-min`，实测用它可直接访问管理面：

```
curl :29090/admin/snapshot -H "Authorization: Bearer change-me-admin-token-32chars-min"
→ HTTP 200
```

该值公开写在仓库中，等于管理面无鉴权。**占位默认值比没有默认值危险得多** ——
它看起来像已经配好了，没人会去复查。降低部署门槛的正确做法是一键生成强随机值。

网关启动时会校验 `ADMIN_TOKEN`：非空 + ≥32 字符 + 不在弱值黑名单
（`change-me` 家族、`admin`、`password`、`.env.example` 全部占位值、零熵重复字符）。
不通过则拒绝启动并打印生成命令。

不要 `cp .env.example .env` —— 模板里的占位值会被两道校验同时拒绝。

### 1.4 启动

```bash
docker compose up -d --build

# 含监控栈
docker compose --profile obs up -d --build
```

### 1.5 验证

```bash
make e2e
```

56 条断言全通过才算成功。脚本从 `.env` 读取 `ADMIN_TOKEN`，读不到直接失败 ——
拿不到真令牌的鉴权验收是无效的。

用 `make e2e` 而不是直接跑 `./test/e2e/run.sh`：脚本依赖 `docker-compose.test.yml`
提供的 `mock-upstream` 夹具与 demo 种子数据，缺了它网关能起、`/live` 也通，
但脚本会在前置检查处退出 1。`make e2e` 已封装 overlay 与前置清理。

---

## 2. 部署后验证

看到容器 `running` 不代表部署成功。以下命令逐条执行，每条都给出判定标准。

```bash
cd /path/to/unirate && source .env
PROXY=http://127.0.0.1:${PROXY_PORT}
OBS=http://127.0.0.1:${OBS_PORT}
ADMIN=http://127.0.0.1:${ADMIN_PORT}
```

### 2.1 进程与依赖

```bash
# 期望：2 个容器 redis + gateway（含 obs profile 为 4），全部 healthy
docker compose ps

# 期望：HTTP 200
curl -s -o /dev/null -w '%{http_code}\n' $OBS/live

# 期望：HTTP 200。若为 503，说明配置尚未加载（见 §6.3）
curl -s -o /dev/null -w '%{http_code}\n' $OBS/ready

# 期望：redis.ok=true, config.version>0, biz_count>=2
curl -s $OBS/health
```

`config.version` 必须 **> 0**。等于 0 意味着配置从未成功从 SoT 加载，
此时网关无法路由任何业务域 —— 这是 `/ready` 返回 503 最常见的原因。

### 2.2 业务链路真的通

```bash
# 期望：HTTP 200，响应体含 total_tokens
curl -s $PROXY/demo/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-verify' \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'
```

### 2.3 限流真的生效（不是"配置看起来对"）

```bash
# demo-ip-qps 规则 limit=10/1s。并发打 25 个，期望 200 约 10 个、429 若干。
# 必须并发：串行 curl 会跨窗口边界，结果混合两个窗口的配额，无法判定。
sleep 1.1
d=$(mktemp -d)
for i in $(seq 1 25); do
  curl -s -o /dev/null -w '%{http_code}\n' --max-time 5 \
    $PROXY/demo/status/200 -H 'Authorization: Bearer sk-verify-burst' > $d/$i &
done; wait
echo "200: $(cat $d/* | grep -c 200)   429: $(cat $d/* | grep -c 429)"
rm -rf $d
```

判定：200 数落在 **8~13**，且 429 > 0。

- 25 个全 200 → 限流未生效，检查 `/health` 的 `config.version` 与规则是否加载
- 200 数远低于 8 → 计数器被超限请求污染（本项目已修，若复现是回归）

### 2.4 管理面鉴权真的有效

```bash
# 期望 401
curl -s -o /dev/null -w '%{http_code}\n' $ADMIN/admin/bizs

# 期望 401
curl -s -o /dev/null -w '%{http_code}\n' $ADMIN/admin/bizs \
  -H 'Authorization: Bearer change-me-admin-token-32chars-min'

# 期望 200
curl -s -o /dev/null -w '%{http_code}\n' $ADMIN/admin/bizs \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

第二条是回归断言。它返回 200 意味着仓库占位值又能用了 —— **立即停止部署**。

### 2.5 管理面没有暴露在业务端口

```bash
# 期望：非 200（400/403/502 均可）。返回 200 是提权漏洞。
curl -s -o /dev/null -w '%{http_code}\n' -X POST $PROXY/admin/bizs \
  -H "Authorization: Bearer $ADMIN_TOKEN" -d '{"biz":"x","base_url":"http://evil"}'

# 期望：非 200。业务端口不得暴露指标（信息泄露）
curl -s -o /dev/null -w '%{http_code}\n' $PROXY/metrics
```

### 2.6 Admin 端口没有对外监听

```bash
# 期望：只看到 127.0.0.1:29090，不能是 0.0.0.0
#
# 注意：不要用 `docker compose ps --format '{{.Ports}}'` —— 部分 Docker Compose
# 版本不支持该 Go template 语法，会报 "format value could not be parsed"。
# 用 docker inspect 更可靠，它在所有版本上行为一致。
docker inspect unirate-gateway \
  --format '{{range $p, $conf := .NetworkSettings.Ports}}{{$p}} -> {{range $conf}}{{.HostIp}}:{{.HostPort}} {{end}}
{{end}}' | grep 9090
```

从**另一台机器**验证（最可靠）：

```bash
# 在其它主机执行，期望超时或 connection refused
curl -m 5 http://<部署机IP>:29090/admin/bizs
```

### 2.7 Tier 1 配置读写通

```bash
# 期望：items 含 7 项，每项有 value/default/env_value/source/overridden_by_env
curl -s $ADMIN/admin/policy -H "Authorization: Bearer $ADMIN_TOKEN"

# 期望 400（下限 100ms），证明校验在写入前生效
curl -s -o /dev/null -w '%{http_code}\n' -X POST $ADMIN/admin/policy/validate \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"values":{"token_flush_interval":"50ms"}}'
```

### 2.8 热更新真的生效（不是只写进了库）

用 `expose_rule_name` 验证，因为它的效果**直接可观测**（429 响应头的有无），
不需要看日志。

```bash
AUTH="Authorization: Bearer $ADMIN_TOKEN"
JSON='Content-Type: application/json'

# 关掉规则名暴露
curl -s -X PUT $ADMIN/admin/policy -H "$AUTH" -H "$JSON" \
  -H 'X-Operator: deploy-verify' \
  -d '{"values":{"expose_rule_name":false}}' >/dev/null
sleep 1

# 打到 429，统计 X-RateLimit-Rule 出现次数。期望：0
for i in $(seq 1 22); do
  curl -s -o /dev/null -D- $PROXY/demo/status/200 \
    -H 'Authorization: Bearer sk-verify-hot-a' 2>/dev/null
done | tr -d '\r' | grep -ci '^X-RateLimit-Rule:'

# 开回来
curl -s -X PUT $ADMIN/admin/policy -H "$AUTH" -H "$JSON" \
  -H 'X-Operator: deploy-verify' \
  -d '{"values":{"expose_rule_name":true}}' >/dev/null
sleep 1

# 期望：> 0。全程未重启网关。
for i in $(seq 1 22); do
  curl -s -o /dev/null -D- $PROXY/demo/status/200 \
    -H 'Authorization: Bearer sk-verify-hot-b' 2>/dev/null
done | tr -d '\r' | grep -ci '^X-RateLimit-Rule:'
```

判定：第一次必须是 `0`，第二次必须 `> 0`。两次都相同说明改动只落库、未生效。

```bash
# 变更已广播（每次生效会打一行）
docker compose logs --tail=100 gateway | grep -c 'runtime policy updated'

# 期望 > 0：审计日志记录了本次操作者
curl -s $ADMIN/admin/audit -H "$AUTH" | grep -c deploy-verify
```

> 不要用 `log_level=debug` 来验证热更新 —— 当前代码里没有任何 `log.Debug()`
> 调用，改成 debug 后日志输出不会有可见变化，看不到 DEBUG 行属正常，
> 不能据此判断热更新失败。要确认 `log_level` 已生效，看
> `runtime policy updated` 日志行里的 `log_level` 字段。

---

## 3. 生产环境检查清单

### 3.1 必须做

| 项 | 动作 | 为什么 |
|----|------|--------|
| 凭证 | `make init` 生成，不手写 | 手写密码普遍低熵；占位值等于无鉴权 |
| `ADMIN_ADDR` | 保持 `127.0.0.1:9090` | 见 §3.2 |
| Admin 端口映射 | 保持 `127.0.0.1:` 前缀 | 去掉即对全网暴露管理面 |
| `ADMIN_ALLOW_CIDRS` | 收窄到实际运维网段 | 令牌泄露后的第二重防线 |
| `EXPOSE_RULE_NAME` | 外网部署设 `false` | 429 响应会泄露内部规则名与维度 |
| `TZ_OFFSET_SECONDS` | 与业务时区一致 | 决定 1d/1w 窗口边界，改动会错乱计数器 |
| `INSTANCES` | 与真实副本数一致 | 见 §3.3 |
| `.env` 权限 | 600，不入版本库 | 已在 `.gitignore`，勿用 `git add -f` |
| 备份 | 定期 dump `biz_config` / `audit_log` | 配置与问责记录 |

### 3.2 不要放宽 `ADMIN_ADDR` 默认值

默认 `127.0.0.1:9090` 是安全设计，不是保守配置。它防的是「一键部署直接把
管理面暴露到公网」—— 管理面能改写全局限流规则，等同于网关的 root 权限。

compose 里 gateway 服务设了 `ADMIN_ADDR: ":9090"`，那是为了让 e2e 从宿主经
端口映射访问，安全性由 `ports` 的 `127.0.0.1:` 绑定 + CIDR 白名单兜住。
**生产 K8s / 裸机部署不要照抄这一行**，保持默认值或只在内网 overlay 暴露。

需要远程访问管理面时，走跳板机端口转发，不要放开监听：

```bash
ssh -L 29090:127.0.0.1:29090 user@gateway-host
```

### 3.3 `INSTANCES` 误配的后果

这项只在 Redis 故障降级时起作用：每实例保守配额 = 总配额 ÷ 实例数。

- 设得**小于**真实副本数 → 降级时各实例配额之和超过总配额，出现**超卖**
- 设得**大于**真实副本数 → 降级时过度拒绝

扩缩容后必须同步更新。它属 Tier 1，可在管理页面改，无需重启。

### 3.4 端口职责与暴露策略

| 端口 | 用途 | 暴露范围 | 理由 |
|------|------|----------|------|
| `8080` proxy | 业务流量 | **可公开** | 唯一对外服务面；无鉴权，鉴权由上游业务负责 |
| `9091` obs | `/metrics` `/live` `/ready` `/health` | **仅监控网** | 无鉴权，暴露配置版本、熔断状态、业务域数量等内部信息 |
| `9090` admin | 配置读写 | **绝不公开** | 可改写全局限流规则；仅回环 + Token + CIDR 白名单 |
| `6379` redis | 计数器 | 不映射宿主 | 仅容器网络内可达，且强制密码 |

配置 SoT 是进程内嵌的 SQLite（`sqlite-data` 卷内的 `/var/lib/unirate/unirate.db`），
**没有监听端口**，因此不在上表 —— 它的访问控制退化为卷的文件权限。

obs 端口不能和业务端口合并：`/metrics` 会泄露业务域列表与流量特征。
e2e 有一条断言专门守这个（业务端口访问 `/metrics` 必须非 200）。

**obs 端口的宿主映射需按环境收紧（部署期动作，代码不强制）。**
`docker-compose.yml` 当前把 obs 映射为 `"${OBS_PORT:-29091}:9091"`，
即宿主全部网卡可达且无鉴权 —— 这在本机开发可接受，公网机器上不可接受。

要点：Prometheus 抓取**不经过这个映射**。`deploy/prometheus/prometheus.yml`
的目标是 `gateway:9091`，走 compose 内网 DNS，因此删除或收紧宿主映射
不影响抓取（可用 `docker run --network unirate_unirate curlimages/curl
-s http://gateway:9091/metrics` 自行验证）。

生产上按 admin 的写法加回环前缀即可：

```yaml
- "127.0.0.1:${OBS_PORT:-29091}:9091"
```

但改之前要同步这四处从宿主 curl 该端口的地方，否则探活与验收会静默失败：
`Makefile`（`/ready` 等待）、`.github/workflows/ci.yml`（同上）、
`test/e2e/run.sh`（`OBS_URL` 默认值）、`scripts/init-env.sh`（`OBS_PORT`）。
它们都支持环境变量覆盖，走跳板机端口转发时无需改代码。

需要人读指标时不要走这个端口：控制台看板用的是 admin 端口的
`GET /admin/metrics`（同源 + Bearer + CIDR，输出与 obs 端口逐字节一致）。

---

## 4. 配置分层

三层，判据是「能否安全热更新」+「误配的爆炸半径」，不是「方便不方便」。

### 4.1 Tier 0 — 必须环境变量，且无默认值

`ADMIN_TOKEN` `REDIS_PASSWORD` `GRAFANA_PASSWORD`

缺失即拒绝启动。由 `make init` 生成。

`LOGFIRE_TOKEN` 属同类凭证，但**刻意不用 `${VAR:?}` 强制语法**：它只服务于
可选的 `logfire` profile，而 compose 的变量插值发生在 profile 过滤之前 ——
用强制语法会让一个没启用的可选组件卡死所有 compose 命令。校验因此下移到
`deploy/otel/collector.yaml`，由 Collector 自身在启动时拒绝空值。

### 4.2 Tier 0 — 环境变量，可有默认值

改动需重建监听或连接池，因此无法热更新：

`PROXY_ADDR` `OBS_ADDR` `ADMIN_ADDR` `REDIS_ADDRS` `REDIS_DB` `REDIS_POOL_SIZE`
`REDIS_TIMEOUT` `STORE_DSN` `TZ_OFFSET_SECONDS` `SHUTDOWN_GRACE`

`STORE_DSN` 指定 SQLite 库文件路径（见 `internal/store/store.go` 的 `Open`）：

| 取值 | 含义 |
|----------|------|
| 空 | 落 `./data/unirate.db`（`SQLITE_PATH` 可覆盖） |
| `/abs/path.db`、`./rel.db` | 指定绝对或相对路径 |

目录不存在时自动创建（0750）。WAL 与其余 pragma 由 `store.sqliteTarget`
统一注入、不接受外部覆盖 —— 关掉 WAL 会让管理面的审计查询阻塞配置写入。

compose 里设为 `/var/lib/unirate/unirate.db`（对应 `sqlite-data` 卷）。

连接池参数是正确性要求而非调优：SQLite 在 WAL 下允许多读单写，
并发写会撞 `SQLITE_BUSY`，因此写连接限制为 1，由 `database/sql` 排队，
而不是依赖 `busy_timeout` 自旋重试。

以下技术上能热更新，但**刻意留在环境变量**：

| 变量 | 不搬进页面的原因 |
|------|------------------|
| `ADMIN_ALLOW_CIDRS` | 若可从页面改，攻击者拿到令牌后能自行放开来源限制，等于自毁第二重防线 |
| `ALLOW_HEADER_UPSTREAM` | SSRF 开关。从页面可开启等于把 SSRF 防线交给一个令牌 |
| `UPSTREAM_ALLOWLIST` | 同上，SSRF 白名单 |
| `TRUSTED_PROXY_HOPS` | 影响 IP 提取，误配导致所有请求被识别为同一 IP，限流维度整体错乱 |
| `REAL_IP_HEADER` | 同上 |
| `TOKEN_HEADERS` | 影响 token 维度提取，误配使配额统计失效 |

### 4.3 Tier 1 — 可在管理页面热改

`GET /admin/policy` 读，`PUT /admin/policy` 写，`POST /admin/policy/validate` 试算。

| 键 | 默认 | 范围 | 备注 |
|----|------|------|------|
| `expose_rule_name` | `true` | bool | 外网建议 `false` |
| `upstream_timeout` | `60s` | `1s ~ 600s` | SSE 不受此约束 |
| `token_flush_interval` | `1s` | `100ms ~ 10s` | 直接决定 Token 超卖窗口宽度 |
| `max_request_body_mb` | `32` | `1 ~ 256` | 上限是防内存耗尽的硬护栏 |
| `config_poll_interval` | `15s` | `5s ~ 300s` | Pub/Sub 的兜底轮询 |
| `log_level` | `info` | `debug/info/warn/error` | debug 在高流量下日志量激增 |
| `instances` | `1` | `1 ~ 1024` | 见 §3.3 |

写入前完成校验，越界返回 400 且不落库。每次变更写 `audit_log`。

### 4.4 优先级：页面 > 环境变量 > 内置默认

页面优先，因为它是**运行期决策**，环境变量是**部署期决策**。

`GET /admin/policy` 每项返回：

| 字段 | 含义 |
|------|------|
| `value` | 当前生效值 |
| `default` | 内置默认值 |
| `env_value` | 环境变量层的值（未显式设置时等于 `default`） |
| `source` | 生效值来自 `page` / `env` / `default` |
| `overridden_by_env` | 该项是否被环境变量**显式设置** |
| `page_value` | 页面覆盖的原始值，未覆盖为 `null` |

`overridden_by_env: true` 不代表页面改动无效 —— 页面优先级更高。
它的意思是「本项在部署侧也被固定过」，用途是提示：一旦你用 `reset` 清除页面覆盖，
生效值会回落到那个 env 值，**而不是**回落到内置默认值。

清除覆盖：

```bash
curl -X PUT $ADMIN/admin/policy -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d '{"reset":["log_level"]}'
```

### 4.5 Tier 2 — 业务限流规则

`/admin/bizs` 读写，SQLite 为 SoT，Pub/Sub 秒级生效。校验见 `/admin/rules/validate`。

---

## 5. 升级与回滚

### 5.1 升级

```bash
# 1. 备份（biz_config / audit_log / runtime_config 都在 SQLite 卷里）
#    必须用 .backup，见下方说明
docker run --rm -v unirate_sqlite-data:/d -v "$PWD:/out" alpine:3.20 sh -c \
  'apk add --no-cache sqlite >/dev/null && \
   sqlite3 /d/unirate.db ".backup /out/backup-$(date +%Y%m%d-%H%M).db"'

# 2. 记录当前版本，回滚时要用
docker compose images gateway
git rev-parse HEAD

# 3. 拉取新代码
git pull

# 4. 滚动重建（建表由进程启动时幂等完成，无需单独的迁移步骤）
docker compose up -d --build gateway

# 5. 验证（必须做，不能只看容器状态）
make e2e
```

**运行中不要用 `docker cp` 或 `cp` 直接拷 `unirate.db`。** 库跑在 WAL 模式下，
最近写入停留在 `unirate.db-wal` 侧车文件里还没合并进主库。

实测：写入一个业务域后立刻用三种方式备份，再从备份里查 `biz_config`。

| 方式 | 时机 | 产物 | 查 `biz_config` |
|------|------|------|-----------------|
| `cp unirate.db` | 运行中 | 36 KB | **0 行** |
| `sqlite3 ... ".backup"` | 运行中 | 36 KB | 1 行，数据完整 |
| `cp unirate.db` | `down` 之后 | 36 KB | 1 行，数据完整 |

热备那份的失效形态比"文件损坏"隐蔽得多：**大小正常、表结构齐全、
`.schema` 看不出任何问题，只是数据是空的**。运维拿到一份 36 KB 的备份，
没有任何可疑信号，直到真要恢复那天才发现里面什么都没有。

第三行解释了为什么这个坑能长期存活：`docker compose down` 干净关停时
SQLite 会做检查点合并，WAL 清空、数据全部落进主库，所以**停机后**拷单文件
确实拿得到完整数据。同一条 `cp` 命令在停机时可用、热备时静默产出空库 ——
成功过一次，就更不会去怀疑它。

结论：统一用 `.backup`，它自行处理 WAL 检查点，两种状态下都正确。
要坚持文件级备份就必须三个文件一起拷（`.db` `.db-wal` `.db-shm`）。

原先的 `make migrate` 步骤已移除：DDL 内嵌在 `internal/store/schema.go`，
每次启动幂等执行，`runtime_config` 表不会再缺失。老部署升级也无需人工介入。

### 5.2 回滚

```bash
git checkout <上一个可用 commit>
docker compose up -d --build gateway
make e2e
```

数据层通常不需要回滚：`runtime_config` 表存在但代码不认识它，只是被忽略。
确需清除（会丢失全部页面侧配置改动）：

```bash
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 sh -c \
  'apk add --no-cache sqlite >/dev/null && \
   sqlite3 /d/unirate.db "DROP TABLE runtime_config;"'
docker compose restart gateway   # 重启时会重新幂等建表
```

凭证不要在回滚时重新生成 —— 新的 `REDIS_PASSWORD` 与运行中的 Redis 不匹配，
计数器读写会全部失败。

### 5.3 零中断说明

单实例 `docker compose up -d` 会有秒级中断。要求不中断则：

- 多实例部署，SLB 按 `/ready` 摘流（网关退出前先让 `/ready` 转 503，再等在途请求结束，
  由 `SHUTDOWN_GRACE` 控制，默认 30s）
- Redis 不要与网关同批重启

SQLite 随网关进程共生，不构成独立的重启依赖，也就少掉一个重启协调环节。

**多实例部署必须把管理面写入固定到单一实例。** 配置读取路径本身没问题：
所有实例都从 Redis 快照读，不查 SoT，Pub/Sub 秒级同步（见 §6.4）。
问题在写入侧 —— SQLite 是进程本地文件，各实例各持一份：

- 管理面写请求被 SLB 打到哪个实例，配置就只落进那个实例的库
- 该实例的库成为事实上的唯一 SoT，其余实例的库是永不更新的死数据
- 审计日志按请求落点散落各实例，问责记录不完整
- 任一实例重启并从本地库 bootstrap 时，会用自己那份陈旧数据覆盖 Redis 快照

可行做法（按推荐程度）：

1. **指定一个写实例**：只有它映射 admin 端口（9090），其余实例的 admin
   端口不对外暴露。其余实例只读 Redis 快照，本地库仅作为空壳。
2. **共享库文件卷**：把 `sqlite-data` 挂到同一网络存储上。仅在存储提供
   完整 POSIX 文件锁语义时才成立；NFS 常见配置下锁不可靠，会出现库损坏，
   除非已确认存储行为，否则不要用这条。

单实例部署不受影响，这也是默认形态。

---

## 6. 故障排查

### 6.1 Redis 挂了会怎样

**不会整体不可用**，按 metric 分治降级（见 `internal/limiter/limiter.go`）：

| 规则类型 | 降级行为 | 理由 |
|----------|----------|------|
| `metric=request` | Fail-Open 放行 | 保业务可用性 |
| `metric=token` | 本地保守配额（总量÷实例数） | Token 预算关系真金白银，Fail-Open 等于烧钱开关 |
| `type=concurrency` | 本地保守配额 | 同上，防打爆下游 |

配置也会保留最后有效快照继续服务，不会因配置中心不可达而停摆。

关键细节：**只有熔断器确认 Redis 真故障时才降级放行**。健康状态下的偶发超时
一律 Fail-Close —— 否则高并发抖动会让限流静默失效（实测 500 并发打 limit=50
因超时放行导致通过 134 个）。

排查：

```bash
# 期望 0。为 1 说明熔断器已打开，限流正在降级
curl -s $OBS/metrics | grep unirate_redis_breaker_open

# 降级决策计数（注意指标名是 degraded_decisions_total，不是 degraded_total —
# 写错会 grep 到空输出，从而误判「没发生降级」）
curl -s $OBS/metrics | grep unirate_degraded_decisions_total

docker compose logs --tail=100 redis
docker compose ps redis
```

`unirate_redis_breaker_open` 必须纳入告警。它区分「限流正在精确生效」与
「限流已静默失效」—— 后者不会有任何用户可见症状。

### 6.2 SoT（SQLite）不可用会怎样

SQLite 是进程内库，不存在「数据库服务挂了」这种独立故障。
剩下的真实形态是**卷不可写**（权限错误、磁盘满）：

- **业务流量不受影响**：网关读配置走 Redis 快照，不查 SoT
- **管理面写入失败**：`/admin/bizs` PUT、`/admin/policy` PUT 返回 5xx
- **管理面读取仍可用**：`GET /admin/policy`、`/admin/snapshot` 读本地快照
- 网关重启时若 SoT 打不开，退到 Redis 快照启动，日志出现
  `load from sot failed, falling back to redis`

```bash
# 卷内文件与归属（应为 10001:10001）
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 ls -ln /d

# 磁盘水位
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 df -h /d

# 启动时选定的后端
docker compose logs gateway | grep -E 'store connected|loaded from sot'
```

归属不对是最常见的原因：卷首次挂载到镜像中不存在的路径时会以 `root:root`
创建，而容器以 UID 10001 运行。Dockerfile 已预建 `/var/lib/unirate` 并
chown，空卷会继承这个归属；若是从旧版本升级上来的既有卷，需手工修：

```bash
docker compose down
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 chown -R 10001:10001 /d
docker compose up -d
```

`GET /admin/policy` 刻意不要求 SoT：故障期间恰恰是最需要看配置的时候。

### 6.3 `/ready` 一直 503

```bash
curl -s $OBS/health
```

看 `components.config.version`：

- `0` → 配置从未加载成功。检查 `STORE_DSN` 与卷可写性（§6.2）
- `> 0` 但 `biz_count = 0` → 表是空的，说明还没通过 `/admin/bizs` 配过业务域。
  建表是自动的（启动时幂等执行内嵌 DDL），空表属于正常初始状态，不是故障

网关镜像是 distroless 风格，**容器内没有 `sqlite3`**，查表要用一次性容器
挂同一个卷（注意：`docker compose down` 后再查更可靠，避免与运行中的
写连接争锁）：

```bash
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 sh -c \
  'apk add --no-cache sqlite >/dev/null && \
   sqlite3 /d/unirate.db "SELECT biz, enabled FROM biz_config;"'
```

### 6.4 页面改了配置不生效

按顺序排查，每步都有明确判据：

```bash
# 1. SoT 里有没有？没有 → 写入没成功，看 PUT 的响应码
docker run --rm -v unirate_sqlite-data:/d alpine:3.20 sh -c \
  'apk add --no-cache sqlite >/dev/null && \
   sqlite3 /d/unirate.db "SELECT * FROM runtime_config;"'
```

表不存在已不再是常见原因 —— DDL 内嵌在 `internal/store/schema.go`，
每次启动幂等建表。若真的缺表，说明建表阶段就失败了，
查 `docker compose logs gateway | grep -i migrate`，通常是卷权限问题（§6.2）。

需要留意的是代码把缺表当作「无覆盖项」处理：这让老部署升级不会起不来，
代价是 PUT 会失败而 GET 一切正常 —— 症状不指向根因。

```bash
# 2. 生效值是什么？source 字段说明它来自哪一层
curl -s $ADMIN/admin/policy -H "Authorization: Bearer $ADMIN_TOKEN"
```

- `source: page` 但行为没变 → 该项未接入热更新路径，是 bug
- `source: env` → 页面覆盖没写进去，回到第 1 步
- 值不是你填的那个 → 越界被拒后回退了，看网关日志的
  `invalid runtime policy override` 行

特例：`log_level` 改成 `debug` 后**看不到 DEBUG 日志是正常的** ——
当前代码没有任何 `log.Debug()` 调用。判断它是否生效要看
`runtime policy updated` 日志行里的 `log_level` 字段，不要靠日志量变化。

```bash
# 3. 变更有没有被广播出去？
docker compose logs --tail=100 gateway | grep -E 'policy updated|hot-reloaded'

# 4. 多实例部署时，其它实例是否已同步
curl -s $OBS/health | grep -o '"version":[0-9]*'
```

Pub/Sub 不保证投递，兜底轮询按 `config_poll_interval`（默认 15s）。
若某实例长期落后，检查它到 Redis 的连通性。

### 6.5 网关起不来

```bash
docker compose logs gateway | tail -30
```

| 日志关键字 | 原因 | 处理 |
|-----------|------|------|
| `admin token is required` | `ADMIN_TOKEN` 为空 | `make init` |
| `well-known placeholder` | 用了仓库占位值 | `make init`，不要手写 |
| `admin token too short` | 不足 32 字符 | `openssl rand -base64 24` |
| `single repeated character` | 类似 `aaaa...` | 同上 |
| `required variable ... is missing` | compose 缺凭证 | `make init` |
| `invalid admin allow cidr` | CIDR 格式错 | 检查 `ADMIN_ALLOW_CIDRS` |

### 6.6 429 过多

```bash
# 看哪条规则、哪个维度在拒
curl -s $OBS/metrics | grep unirate_rejected_total

# 确认不是降级导致的过度拒绝
curl -s $OBS/metrics | grep -E 'unirate_degraded_decisions_total|breaker_open'
```

若 `breaker_open=1`，说明 Redis 故障期间走了本地保守配额，此时 429 偏多是
预期行为 —— 先修 Redis，不要急着放宽规则。

排障时可临时开规则名暴露定位具体规则（外网记得改回）：

```bash
curl -X PUT $ADMIN/admin/policy -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d '{"values":{"expose_rule_name":true}}'
```

---

## 7. 日常运维命令

```bash
make help          # 全部可用目标
make init          # 生成 .env（已存在则拒绝）
make up            # 启动生产编排（redis + gateway），不含测试夹具
make up-test       # 启动 + 测试夹具（mock-upstream / demo 种子），e2e 用
make down          # 停止并清理数据卷（含测试夹具，会丢数据）
make logs          # 跟踪网关日志
make ps            # 服务状态（含测试夹具）
make e2e           # 端到端验收（自动重建干净栈）
make test          # 全量测试（含 race + 真实 Redis）
make verify        # 提交前完整校验（vet + test）
```
