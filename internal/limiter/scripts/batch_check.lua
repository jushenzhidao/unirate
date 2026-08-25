--[[
batch_check.lua — 多规则原子限流求值（两阶段提交）

设计要点（对应评审 P0-4 / P0-5 修正）：
  评审建议的做法是「INCR 后发现超限再 DECR 回滚」。该方案在多规则串行检查时依然脆弱：
  回滚自身可能失败、跨规则回滚顺序难以维护、且拒绝路径存在瞬时计数污染窗口。

  本脚本改为两阶段求值，从根本上消除中间态：
    Phase 1 (dry-run)  : 所有规则只读试算，不产生任何配额变更
    Phase 2 (commit)   : 当且仅当全部规则通过，才统一写入
  任一规则不通过 → 直接返回，配额计数器保持原样，无需任何回滚。
  由此天然满足：被拒请求不占用任何配额（杜绝 DoS 放大 + 水位虚高）。

  Phase 1 中唯一允许的写操作是「清理已过期成员」(ZREMRANGEBYSCORE)，
  该操作幂等且在任何情况下都是正确的，不影响上述不变量。

时间源：统一使用 redis.call('TIME')，避免多网关实例时钟漂移污染滑动窗口/令牌桶。
        （Redis 5.0+ 默认 effect-based replication，脚本内调用 TIME 是安全的）

入参：
  KEYS[1..N] : 各规则对应的 Redis Key
  ARGV[1]    : cjson 编码的规则数组
  ARGV[2]    : 全局唯一请求 ID（用于滑动窗口 member 与并发持有者标识）

规则对象字段：
  k        : 该规则使用的 KEYS 下标（1-based）
  t        : 规则类型 fixed | sliding | tb | conc | tkadmit
  limit    : 配额上限（conc 为 max_concurrent）
  cost     : 本次消耗量（request 计量恒为 1，token 计量为 token 数）
  expire   : Key 过期秒数（fixed）
  window   : 窗口毫秒数（sliding）
  rate     : 每秒填充速率（tb）
  burst    : 桶容量（tb）
  ttl      : 并发持有超时毫秒数（conc）

tkadmit（Token 预算准入，对应 ADR-003 合并往返）：
  语义与 token_ledger.lua 的 op='admit' 完全一致 —— 只读检查
  「当前窗口预算是否已耗尽」，used >= limit 即拒绝。
  它在 Phase 2 中**不做任何写入**（Token 属事后计量，
  准入期无法预知消耗，实际扣减由 TokenReserve/Settle 在响应期完成）。
  因此该类型天然不破坏「全通过才提交」的不变量。

  合并动机：原先 TokenAdmit 与 Check 是两次串行 Redis 往返，
  且 TokenAdmit 对每条 token 规则各发一次。实测双往返使吞吐腰斩。

返回：
  { ok, failed_rule_index, retry_after_ms }
  ok = 1 表示全部通过并已提交；ok = 0 表示被 failed_rule_index 号规则拒绝（未提交任何变更）
]]

local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)

local rules = cjson.decode(ARGV[1])
local req_id = ARGV[2]

-- Phase 1: 只读试算，收集提交所需的中间状态
--
-- plans 必须保持「无空洞」：Phase 2 用 ipairs 遍历，而 ipairs 遇到第一个 nil 即停止。
-- 若某条规则不产生 plan 就留下 nil 空洞，其后所有规则的提交会被静默跳过 ——
-- 表现为「判定通过但配额没扣」，即 P0-1 配额放大的复现，且不报任何错。
-- 因此 tkadmit 这类「只读不写」的规则也必须占位（t='noop'），不得留空。
local plans = {}

for i, r in ipairs(rules) do
    local key = KEYS[r.k]
    local cost = tonumber(r.cost) or 1
    local limit = tonumber(r.limit)

    if r.t == 'fixed' then
        -- 固定窗口：先判后增。避免「先 INCR 再判断」导致被拒请求污染计数
        local cur = tonumber(redis.call('GET', key)) or 0
        if cur + cost > limit then
            local ttl = redis.call('PTTL', key)
            if ttl < 0 then ttl = tonumber(r.expire) * 1000 end
            return { 0, i, ttl }
        end
        plans[i] = { t = 'fixed', key = key, cost = cost, expire = tonumber(r.expire), fresh = (cur == 0) }

    elseif r.t == 'sliding' then
        -- 滑动窗口：ZSet + 全局唯一 member，杜绝同毫秒同值覆盖导致的漏计（评审 P0-5 附带项）
        local window = tonumber(r.window)
        local min_score = now_ms - window
        redis.call('ZREMRANGEBYSCORE', key, 0, min_score)  -- 幂等清理，安全
        local cnt = tonumber(redis.call('ZCARD', key)) or 0
        if cnt + cost > limit then
            -- retry_after = 最早成员滑出窗口所需时间
            local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
            local wait = window
            if oldest and oldest[2] then
                wait = (tonumber(oldest[2]) + window) - now_ms
                if wait < 0 then wait = 0 end
            end
            return { 0, i, wait }
        end
        plans[i] = { t = 'sliding', key = key, cost = cost, window = window }

    elseif r.t == 'tb' then
        -- 令牌桶：Hash 持久桶（tokens/last_refill），Key 不含窗口 boundary
        -- 修正评审 P0-2：原设计 Key 带 window_boundary 会令桶每窗口重建，退化为固定窗口
        local rate = tonumber(r.rate)
        local burst = tonumber(r.burst)
        local st = redis.call('HMGET', key, 'tokens', 'ts')
        local tokens = tonumber(st[1])
        local last = tonumber(st[2])
        if tokens == nil or last == nil then
            tokens = burst
            last = now_ms
        else
            local delta = now_ms - last
            if delta > 0 then
                tokens = tokens + (delta / 1000.0) * rate
                if tokens > burst then tokens = burst end
                last = now_ms
            end
        end
        if tokens < cost then
            local need = cost - tokens
            local wait = math.ceil((need / rate) * 1000)
            return { 0, i, wait }
        end
        plans[i] = { t = 'tb', key = key, cost = cost, tokens = tokens, ts = last, rate = rate, burst = burst }

    elseif r.t == 'conc' then
        -- 并发控制：ZSet(member=request_id, score=deadline_ms)
        -- 修正评审 P0-4：
        --   1) 拒绝路径不 ZADD → 被拒请求不占并发额度（原设计 INCR 后拒绝会永久泄漏）
        --   2) 释放用 ZREM(request_id) 精确移除 → 不会误减他人计数、不会出现负值
        --      （原设计 key 级 TTL 过期后旧请求 DECR 会误伤新窗口，甚至减成负数使限流失效）
        --   3) 过期成员由 ZREMRANGEBYSCORE 按 deadline 清扫 → 进程崩溃也能自愈
        local ttl = tonumber(r.ttl)
        redis.call('ZREMRANGEBYSCORE', key, 0, now_ms)  -- 清扫已超时的持有者
        local cnt = tonumber(redis.call('ZCARD', key)) or 0
        if cnt + 1 > limit then
            return { 0, i, ttl }
        end
        plans[i] = { t = 'conc', key = key, deadline = now_ms + ttl, ttl = ttl }

    elseif r.t == 'tkadmit' then
        -- Token 预算准入：只读检查余额是否已耗尽（ADR-003）
        --
        -- 与 token_ledger.lua op='admit' 逐位等价：
        --   used = GET(key) or 0；used >= limit 则拒绝，retry_after = PTTL。
        -- 刻意不用 used + cost > limit：准入期不知道本次会消耗多少 token，
        -- 语义是「窗口预算已用完则不再放新请求进来」，在途请求不受影响。
        local used = tonumber(redis.call('GET', key)) or 0
        if used >= limit then
            local pttl = redis.call('PTTL', key)
            if pttl < 0 then pttl = 0 end
            return { 0, i, pttl }
        end
        -- 占位，防止 plans 出现空洞（见上方注释）。Phase 2 对其不做任何写入。
        plans[i] = { t = 'noop' }

    else
        -- 未知规则类型：必须显式占位，否则同样会造成 plans 空洞。
        -- 保持与原实现一致的宽容语义（未知类型不参与限流），但不破坏提交完整性。
        plans[i] = { t = 'noop' }
    end
end

-- Phase 2: 全部通过，统一提交
for i, p in ipairs(plans) do
    if p.t == 'fixed' then
        redis.call('INCRBY', p.key, p.cost)
        if p.fresh then
            redis.call('EXPIRE', p.key, p.expire)
        end
    elseif p.t == 'sliding' then
        for n = 1, p.cost do
            redis.call('ZADD', p.key, now_ms, req_id .. ':' .. n)
        end
        redis.call('PEXPIRE', p.key, p.window + 1000)
    elseif p.t == 'tb' then
        redis.call('HSET', p.key, 'tokens', p.tokens - p.cost, 'ts', p.ts)
        -- 桶从空到满所需时间 + 冗余，闲置后自动回收，避免无限增长
        redis.call('PEXPIRE', p.key, math.ceil((p.burst / p.rate) * 1000) + 60000)
    elseif p.t == 'conc' then
        redis.call('ZADD', p.key, p.deadline, req_id)
        redis.call('PEXPIRE', p.key, p.ttl + 60000)
    end
end

return { 1, 0, 0 }
