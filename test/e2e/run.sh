#!/usr/bin/env bash
# unirate 端到端验收
#
# 覆盖评审要求的四条链路 + 安全回归：
#   A. 成功流       正常代理、路径剥离、头透传
#   B. 429 流       超限拒绝、Retry-After、计数器不被污染
#   C. Redis 故障流 降级行为符合 metric 分治策略
#   D. SSE 计量流   零缓冲透传 + Token 预扣核销
#   E. 安全回归     Admin 鉴权、SSRF 防护、XFF 防伪造
#
# 退出码非 0 即验收失败。

set -uo pipefail

PROXY="${PROXY_URL:-http://127.0.0.1:28080}"
OBS="${OBS_URL:-http://127.0.0.1:29091}"
ADMIN="${ADMIN_URL:-http://127.0.0.1:29090}"
COMPOSE="${COMPOSE_CMD:-docker compose}"

# ADMIN_TOKEN 从环境或 .env 读取，**不再有内置默认值**。
#
# 这里曾硬编码回退值 change-me-admin-token-32chars-min，与 compose 的同名默认值
# 互相印证成了"一切正常"的假象 —— 而那个令牌公开写在仓库里，
# 等于验收脚本在确认一个无鉴权的管理面可以被访问。
# 现在取不到真实令牌就直接失败：拿不到真令牌的鉴权验收本身是无效的。
ENV_FILE="${ENV_FILE:-$(cd "$(dirname "$0")/../.." && pwd)/.env}"
if [ -z "${ADMIN_TOKEN:-}" ] && [ -f "$ENV_FILE" ]; then
  ADMIN_TOKEN=$(grep -E '^ADMIN_TOKEN=' "$ENV_FILE" | head -1 | cut -d= -f2-)
fi
TOKEN="${ADMIN_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  printf '\033[31m未找到 ADMIN_TOKEN\033[0m —— 请先执行 make init 生成 .env，\n'
  printf '或显式传入：ADMIN_TOKEN=xxx %s\n' "$0"
  exit 1
fi

PASS=0
FAIL=0
declare -a FAILURES=()

c_g=$'\033[32m'; c_r=$'\033[31m'; c_y=$'\033[33m'; c_b=$'\033[36m'; c_0=$'\033[0m'

ok()   { PASS=$((PASS+1)); printf '  %s✓%s %s\n' "$c_g" "$c_0" "$1"; }
# 注意不要写成 `[ $# -gt 1 ] && printf ...`：该复合语句在条件为假时
# 返回非零退出码，会成为函数返回值，在 set -e 语境下或被调用方判断时引发误判。
bad() {
  FAIL=$((FAIL+1))
  FAILURES+=("$1")
  printf '  %s✗%s %s\n' "$c_r" "$c_0" "$1"
  if [ $# -gt 1 ]; then
    printf '      %s\n' "$2"
  fi
  return 0
}
info() { printf '  %s·%s %s\n' "$c_y" "$c_0" "$1"; }
sect() { printf '\n%s=== %s ===%s\n' "$c_b" "$1" "$c_0"; }

# assert_eq <期望> <实际> <描述>
assert_eq() {
  if [ "$1" = "$2" ]; then ok "$3"; else bad "$3" "期望 '$1'，实际 '$2'"; fi
}

metric() {
  curl -fsS "$OBS/metrics" 2>/dev/null | grep -E "^$1" | awk '{s+=$NF} END {printf "%d", s+0}'
}

# ---------------------------------------------------------------------------
sect "前置检查"

if ! curl -fsS "$OBS/live" >/dev/null 2>&1; then
  printf '%s网关未运行%s，请先执行: docker compose up -d --build\n' "$c_r" "$c_0"
  exit 1
fi
ok "网关存活 (/live)"

ready=$(curl -s -o /dev/null -w '%{http_code}' "$OBS/ready")
assert_eq "200" "$ready" "网关就绪 (/ready)"

# /health 里有两个 version 字段：顶层 "version":"dev"（构建版本，字符串）与
# components.config.version（配置版本，数字）。必须精确定位后者 ——
# 早先用 grep -o '"version":[0-9]*' | head -1 会先命中字符串那个并匹配到空值。
cfg_ver=$(curl -fsS "$OBS/health" \
  | grep -o '"config":{[^}]*}' \
  | grep -o '"version":[0-9]\+' \
  | cut -d: -f2)
if [ -n "$cfg_ver" ] && [ "$cfg_ver" -gt 0 ] 2>/dev/null; then
  ok "配置已从 SoT 加载（version=${cfg_ver}）"
else
  bad "配置版本异常" "解析结果: '${cfg_ver}' —— 配置可能未成功加载"
fi

biz_count=$(curl -fsS "$OBS/health" \
  | grep -o '"biz_count":[0-9]\+' | cut -d: -f2)
info "已加载业务域数量: ${biz_count:-0}"

# ---------------------------------------------------------------------------
sect "A. 成功流"

resp=$(curl -s -w '\n%{http_code}' "$PROXY/demo/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-e2e-success' \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}')
code=$(printf '%s' "$resp" | tail -1)
body=$(printf '%s' "$resp" | sed '$d')
assert_eq "200" "$code" "正常请求返回 200"

if printf '%s' "$body" | grep -q '"total_tokens":20'; then
  ok "上游响应完整透传（含 usage）"
else
  bad "上游响应透传" "响应体: $(printf '%s' "$body" | head -c 200)"
fi

# 路径剥离：网关应把 /demo/v1/... 转成 /v1/...
echo_body=$(curl -fsS "$PROXY/demo/echo?a=1" -H 'Authorization: Bearer sk-e2e-path')
if printf '%s' "$echo_body" | grep -q '"path":"/echo"'; then
  ok "path_strip_prefix 生效（/demo/echo → /echo）"
else
  bad "path_strip_prefix" "$(printf '%s' "$echo_body" | head -c 200)"
fi
if printf '%s' "$echo_body" | grep -q '"query":"a=1"'; then
  ok "查询串保留"
else
  bad "查询串保留"
fi
# 逐跳头必须剥离，Via 必须追加
if printf '%s' "$echo_body" | grep -qi '"Via"'; then
  ok "Via 头已追加"
else
  bad "Via 头缺失"
fi

# 请求 ID 必须回传，便于全链路排查
rid=$(curl -fsS -D- -o /dev/null "$PROXY/demo/echo" -H 'X-Request-Id: e2e-fixed-id' 2>/dev/null \
      | tr -d '\r' | grep -i '^X-Request-Id:' | awk '{print $2}')
assert_eq "e2e-fixed-id" "$rid" "X-Request-Id 原样回传"

# 未配置的业务域必须拒绝而非透传
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/nonexistent-biz/x")
if [ "$code" = "502" ] || [ "$code" = "403" ]; then
  ok "未知业务域被拒绝 (HTTP $code)"
else
  bad "未知业务域处理" "得到 HTTP $code，期望 502/403"
fi

# 非法 biz 格式
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/bad_biz/x")
assert_eq "400" "$code" "非法 biz 字符集被拒绝"

# ---------------------------------------------------------------------------
sect "B. 429 流（超限拒绝 + 计数器不污染）"

# demo-ip-qps: 1s 窗口内 10 次。
#
# 必须并发发起：串行 curl 的总耗时会跨越 1s 窗口边界，
# 使统计结果混合两个窗口的配额，无法对通过数做精确断言。
info "并发发送 25 个请求（规则 limit=10/1s）..."
burst_dir=$(mktemp -d)
sleep 1.1   # 让上一批请求的窗口过期，尽量让整批落在同一窗口
for i in $(seq 1 25); do
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PROXY/demo/status/200" \
    -H 'Authorization: Bearer sk-e2e-burst' > "$burst_dir/$i" 2>/dev/null &
done
wait
codes=$(cat "$burst_dir"/* 2>/dev/null)
n200=$(printf '%s' "$codes" | grep -o '200' | wc -l | tr -d ' ')
n429=$(printf '%s' "$codes" | grep -o '429' | wc -l | tr -d ' ')
rm -rf "$burst_dir"
info "结果: 200×${n200}, 429×${n429}"

if [ "$n429" -gt 0 ]; then
  ok "超限请求被拒绝（429×${n429}）"
else
  bad "限流未生效" "25 个请求全部通过"
fi
# 关键断言：通过数必须收敛到配额附近。
# 上界验证 P0-1（集群语义未被放大）；下界验证 P0-5（计数器未被超限请求污染，
# 若污染则会过度拒绝，通过数显著低于 limit）。
# 区间 [8,13] 容忍窗口边界处的并发抖动。
if [ "$n200" -le 13 ] && [ "$n200" -ge 8 ]; then
  ok "通过数 ${n200} 收敛于配额 10（区间 [8,13]）"
else
  bad "配额精度" "通过 ${n200} 个，期望 8~13（limit=10）"
fi

# 429 响应必须带 Retry-After，客户端才知道何时重试
hdrs=$(for i in $(seq 1 20); do
  curl -s -D- -o /dev/null "$PROXY/demo/status/200" -H 'Authorization: Bearer sk-e2e-retry' 2>/dev/null
done | tr -d '\r')
if printf '%s' "$hdrs" | grep -qi '^Retry-After:'; then
  ok "429 响应包含 Retry-After"
else
  bad "Retry-After 缺失"
fi
if printf '%s' "$hdrs" | grep -qi '^X-RateLimit-Rule:'; then
  ok "429 响应标注命中规则名"
else
  info "X-RateLimit-Rule 未暴露（EXPOSE_RULE_NAME 可能已关闭）"
fi

# 窗口恢复：等待窗口过期后必须重新放行，证明计数器正常过期而非永久污染
info "等待 1.5s 让窗口滚动..."
sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/demo/status/200" \
  -H 'Authorization: Bearer sk-e2e-recover')
assert_eq "200" "$code" "窗口滚动后恢复放行（计数器未被永久污染）"

rejected=$(metric 'unirate_rejected_total')
if [ "${rejected:-0}" -gt 0 ]; then
  ok "拒绝指标已上报 (unirate_rejected_total=${rejected})"
else
  bad "拒绝指标未上报"
fi

# ---------------------------------------------------------------------------
sect "D. SSE 计量流（零缓冲 + Token 核销）"

tok_before=$(metric 'unirate_tokens_reserved_total')

# 用 --no-buffer 让 curl 立即输出，通过时间戳观察帧到达节奏
sse_out=$(mktemp)
timeout 40 curl -sN --no-buffer \
  "$PROXY/demo/v1/chat/completions?stream=1&chunks=8&delay_ms=150" \
  -H 'Accept: text/event-stream' \
  -H 'Authorization: Bearer sk-e2e-sse' \
  > "$sse_out" 2>/dev/null

frames=$(grep -c '^data:' "$sse_out" 2>/dev/null || echo 0)
info "收到 ${frames} 个 data 帧"
if [ "$frames" -ge 9 ]; then
  ok "SSE 帧完整（8 内容帧 + usage 帧 + DONE）"
else
  bad "SSE 帧数异常" "仅 ${frames} 帧，内容: $(head -c 200 "$sse_out")"
fi
if grep -q '\[DONE\]' "$sse_out"; then
  ok "SSE 正常收到 [DONE]"
else
  bad "SSE 未收到 [DONE]"
fi
if grep -q '"total_tokens"' "$sse_out"; then
  ok "上游精确 usage 帧已透传"
else
  bad "usage 帧丢失"
fi

# 零缓冲验证：8 帧 × 150ms ≈ 1.2s。若网关缓冲，首帧会延迟到流结束才出现。
first_byte_ms=$(timeout 40 curl -sN --no-buffer -o /dev/null \
  -w '%{time_starttransfer}' \
  "$PROXY/demo/v1/chat/completions?stream=1&chunks=10&delay_ms=200" \
  -H 'Accept: text/event-stream' -H 'Authorization: Bearer sk-e2e-ttfb' 2>/dev/null \
  | awk '{printf "%d", $1*1000}')
info "首字节时间: ${first_byte_ms}ms（总流时长约 2000ms）"
if [ "${first_byte_ms:-99999}" -lt 1000 ]; then
  ok "SSE 零缓冲：首字节远早于流结束"
else
  bad "SSE 疑似被缓冲" "首字节 ${first_byte_ms}ms，接近或超过总流时长"
fi

sleep 2
tok_after=$(metric 'unirate_tokens_reserved_total')
settled=$(metric 'unirate_tokens_settled_total')
info "Token 预扣: ${tok_before} → ${tok_after}，已核销: ${settled}"
if [ "${tok_after:-0}" -gt "${tok_before:-0}" ]; then
  ok "SSE 期间 Token 增量预扣生效"
else
  bad "Token 预扣未发生"
fi
if [ "${settled:-0}" -gt 0 ]; then
  ok "Token 核销退差生效（评审 P0-6 修复验证）"
else
  bad "Token 未核销" "上游返回了精确 usage 但未触发 settle"
fi

sse_active=$(metric 'unirate_sse_streams_active')
assert_eq "0" "${sse_active:-x}" "SSE 流结束后活跃计数归零"
rm -f "$sse_out"

# 并发额度必须归零，否则是泄漏（评审 P0-4）
inflight=$(metric 'unirate_concurrency_in_flight')
assert_eq "0" "${inflight:-x}" "并发计数器归零（无泄漏）"

# ---------------------------------------------------------------------------
sect "E. 安全回归"

# Admin 鉴权：无 token 必须 401
code=$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/admin/bizs")
assert_eq "401" "$code" "Admin 无凭证访问被拒（评审 P0-3）"

code=$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/admin/bizs" \
  -H 'Authorization: Bearer wrong-token-xxxxxxxxxxxxx')
assert_eq "401" "$code" "Admin 错误凭证被拒"

code=$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/admin/bizs" \
  -H "Authorization: Bearer $TOKEN")
assert_eq "200" "$code" "Admin 正确凭证可访问"

# 业务端口绝不能触达 Admin 路由 —— 这是原 Spec 的核心漏洞
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/admin/bizs" \
  -H "Authorization: Bearer $TOKEN" -d '{"biz":"pwned","base_url":"http://evil"}')
if [ "$code" = "400" ] || [ "$code" = "502" ] || [ "$code" = "403" ]; then
  ok "业务端口无法访问 Admin 接口 (HTTP $code)"
else
  bad "Admin 接口暴露在业务端口" "HTTP $code —— 这是提权漏洞"
fi

# SSRF：请求头指定上游默认关闭
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/demo/echo" \
  -H 'X-Upstream-Base-URL: http://169.254.169.254' \
  -H 'Authorization: Bearer sk-e2e-ssrf')
if [ "$code" = "200" ]; then
  # 200 说明请求头被忽略，走了配置的正常上游 —— 这是期望行为
  ok "X-Upstream-Base-URL 被忽略（未启用覆盖）"
else
  info "上游覆盖头处理返回 HTTP $code"
fi

# 规则校验接口：令牌桶 + metric=token 必须被拒（评审 P0-2）
resp=$(curl -s -w '\n%{http_code}' -X POST "$ADMIN/admin/rules/validate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '[{"name":"bad","type":"rate","metric":"token","dimensions":["biz"],"window":"1h","limit":1000,"algorithm":"token_bucket"}]')
code=$(printf '%s' "$resp" | tail -1)
assert_eq "400" "$code" "token_bucket + metric=token 组合被拒绝（评审 P0-2）"

# 非法维度组合
resp=$(curl -s -w '\n%{http_code}' -X POST "$ADMIN/admin/rules/validate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '[{"name":"bad2","type":"rate","dimensions":["global","biz"],"window":"1s","limit":10}]')
code=$(printf '%s' "$resp" | tail -1)
assert_eq "400" "$code" "global 与其他维度组合被拒绝"

# 合法规则必须通过
resp=$(curl -s -w '\n%{http_code}' -X POST "$ADMIN/admin/rules/validate" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '[{"name":"good","type":"rate","metric":"request","dimensions":["biz","ip"],"window":"1m","limit":600,"algorithm":"sliding_window"}]')
code=$(printf '%s' "$resp" | tail -1)
assert_eq "200" "$code" "合法规则校验通过"

# 审计日志必须记录变更
audit=$(curl -fsS "$ADMIN/admin/audit" -H "Authorization: Bearer $TOKEN")
if printf '%s' "$audit" | grep -q '"count"'; then
  ok "审计日志接口可用"
else
  bad "审计日志接口异常"
fi

# ---------------------------------------------------------------------------
sect "F. 配置热更新"

new_limit=777
resp=$(curl -s -w '\n%{http_code}' -X POST "$ADMIN/admin/bizs" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H 'X-Operator: e2e-runner' \
  -d "{\"biz\":\"hotreload\",\"base_url\":\"http://mock-upstream:9000\",\"path_strip_prefix\":true,\"enabled\":true,\"rules\":[{\"id\":900,\"name\":\"hot-rule\",\"type\":\"rate\",\"metric\":\"request\",\"dimensions\":[\"biz\"],\"window\":\"1s\",\"limit\":$new_limit,\"algorithm\":\"fixed_window\"}]}")
code=$(printf '%s' "$resp" | tail -1)
assert_eq "200" "$code" "Admin 写入新业务域"

# Pub/Sub 应在秒级生效
sleep 3
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/hotreload/echo" \
  -H 'Authorization: Bearer sk-e2e-hot')
assert_eq "200" "$code" "新业务域热更新后立即可路由（无需重启）"

# 审计记录必须包含本次操作者
audit=$(curl -fsS "$ADMIN/admin/audit" -H "Authorization: Bearer $TOKEN")
if printf '%s' "$audit" | grep -q 'e2e-runner'; then
  ok "变更已记入审计日志（operator=e2e-runner）"
else
  bad "审计日志未记录本次变更"
fi

curl -s -o /dev/null -X DELETE "$ADMIN/admin/bizs/hotreload" -H "Authorization: Bearer $TOKEN"

# ---------------------------------------------------------------------------
sect "C. Redis 故障流（降级策略）"

if [ "${SKIP_REDIS_FAULT:-0}" = "1" ]; then
  info "已跳过（SKIP_REDIS_FAULT=1）"
else
  info "暂停 Redis 容器..."
  if $COMPOSE pause redis >/dev/null 2>&1; then
    sleep 1
    # 触发足够多的失败以打开熔断器
    for i in $(seq 1 12); do
      curl -s -o /dev/null --max-time 3 "$PROXY/demo/status/200" \
        -H 'Authorization: Bearer sk-e2e-degrade' 2>/dev/null
    done

    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$PROXY/demo/status/200" \
      -H 'Authorization: Bearer sk-e2e-degrade')
    # metric=request 规则允许 Fail-Open，因此 200 是符合设计的
    if [ "$code" = "200" ]; then
      ok "Redis 故障期间 request 类限流 Fail-Open（服务不中断）"
    elif [ "$code" = "429" ]; then
      ok "Redis 故障期间保守拒绝（Fail-Close，同样是可接受策略）"
    else
      bad "Redis 故障期间响应异常" "HTTP $code"
    fi

    # /live 绝不能因 Redis 故障而失败 —— 否则会触发无意义的容器重启
    live=$(curl -s -o /dev/null -w '%{http_code}' "$OBS/live")
    assert_eq "200" "$live" "Redis 故障时 /live 仍为 200（不触发误重启，评审 P1-11）"

    # /ready 同样应保持 200：网关仍能代理，摘流反而造成全站不可用
    ready=$(curl -s -o /dev/null -w '%{http_code}' "$OBS/ready")
    assert_eq "200" "$ready" "Redis 故障时 /ready 仍为 200（不误摘流）"

    # /health 必须诚实反映降级
    health=$(curl -fsS "$OBS/health")
    if printf '%s' "$health" | grep -q '"status":"degraded"'; then
      ok "/health 正确上报 degraded 状态"
    else
      bad "/health 未反映降级" "$(printf '%s' "$health" | head -c 200)"
    fi

    info "恢复 Redis..."
    $COMPOSE unpause redis >/dev/null 2>&1
    sleep 3

    # 熔断器必须能自愈
    code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/demo/status/200" \
      -H 'Authorization: Bearer sk-e2e-recovered')
    if [ "$code" = "200" ] || [ "$code" = "429" ]; then
      ok "Redis 恢复后限流重新生效（熔断器自愈）"
    else
      bad "Redis 恢复后异常" "HTTP $code"
    fi
  else
    info "无法暂停 Redis 容器，跳过故障注入"
  fi
fi

# ---------------------------------------------------------------------------
sect "G. 指标与探针完整性"

for m in unirate_requests_total unirate_rejected_total \
         unirate_request_duration_seconds_bucket unirate_config_version \
         unirate_redis_breaker_open unirate_concurrency_in_flight; do
  if curl -fsS "$OBS/metrics" | grep -q "^${m}"; then
    ok "指标存在: ${m}"
  else
    bad "指标缺失: ${m}"
  fi
done

# Prometheus 抓取格式合法性：不得出现空标签名或格式错误
if curl -fsS "$OBS/metrics" | grep -qE '^\s*[a-zA-Z_][a-zA-Z0-9_]*(\{[^}]*\})?\s+[-0-9]'; then
  ok "metrics 输出符合 Prometheus 文本格式"
else
  bad "metrics 格式异常"
fi

# 业务端口不得暴露指标与探针（信息泄露）
code=$(curl -s -o /dev/null -w '%{http_code}' "$PROXY/metrics")
if [ "$code" != "200" ]; then
  ok "业务端口不暴露 /metrics (HTTP $code)"
else
  bad "业务端口暴露了 /metrics" "存在信息泄露"
fi

# ---------------------------------------------------------------------------
# admin 端口的受鉴权指标端点（看板数据源）
#
# 为什么这几条必须放在 e2e 而不是单测：Metrics 的注入发生在
# cmd/gateway/main.go 的装配代码里，而 internal/admin 的单测用
# newTestServer 自行装配 Options —— 注入那一行被误删时，
# 所有 admin 单测仍会全绿，端点却从 401 变成 503。
# cmd/gateway 覆盖率为 0，这条链路只有 e2e 能守。
# 本轮该端点确实出现过「代码写好但镜像未重建 → 404」的状态。

code=$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/admin/metrics")
assert_eq "401" "$code" "admin 指标端点无凭证被拒"

code=$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN/admin/metrics" \
  -H "Authorization: Bearer $TOKEN")
assert_eq "200" "$code" "admin 指标端点带凭证可访问（证明 main.go 已注入 Metrics）"

# 503 是「注入缺失」的特征码，单独断言以便一眼定位病因
if [ "$code" = "503" ]; then
  bad "Metrics 未注入" "main.go 的 admin.Options{Metrics: ...} 可能被删除"
fi

# 与 obs 端口逐行一致：两处渲染分叉会让看板与 Prometheus
# 对同一时刻给出不同结论，这种不一致极难排查。
# 排除 uptime 与 histogram 的 _sum（连续量，两次取样必然不同）。
obs_body=$(curl -fsS "$OBS/metrics" | grep -vE 'uptime|_sum' | sort)
adm_body=$(curl -fsS "$ADMIN/admin/metrics" -H "Authorization: Bearer $TOKEN" \
  | grep -vE 'uptime|_sum' | sort)
if [ "$obs_body" = "$adm_body" ]; then
  ok "admin 与 obs 两端口指标输出一致"
else
  bad "两端口指标输出分叉" "看板与 Prometheus 会对同一时刻给出不同结论"
fi

# 指标含内部拓扑（biz 名/规则名），不得被中间层缓存或内容嗅探改写
hdrs=$(curl -fsS -D- -o /dev/null "$ADMIN/admin/metrics" \
  -H "Authorization: Bearer $TOKEN" | tr -d '\r')
if printf '%s' "$hdrs" | grep -qi '^Cache-Control:.*no-store'; then
  ok "admin 指标端点禁用缓存（no-store）"
else
  bad "缺 Cache-Control: no-store" "中间层缓存会让看板显示过期数据却无提示"
fi
if printf '%s' "$hdrs" | grep -qi '^X-Content-Type-Options:.*nosniff'; then
  ok "admin 指标端点禁用内容嗅探（nosniff）"
else
  bad "缺 X-Content-Type-Options: nosniff"
fi

# ---------------------------------------------------------------------------
printf '\n%s========================================%s\n' "$c_b" "$c_0"
printf '  验收结果: %s通过 %d%s / %s失败 %d%s\n' "$c_g" "$PASS" "$c_0" "$c_r" "$FAIL" "$c_0"
printf '%s========================================%s\n' "$c_b" "$c_0"

if [ "$FAIL" -gt 0 ]; then
  printf '\n%s失败项:%s\n' "$c_r" "$c_0"
  for f in "${FAILURES[@]}"; do printf '  - %s\n' "$f"; done
  exit 1
fi

printf '\n%s全部验收通过%s\n' "$c_g" "$c_0"
exit 0
