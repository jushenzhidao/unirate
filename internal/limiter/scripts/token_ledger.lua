--[[
token_ledger.lua — Token 消耗账本（预扣 / 增量刷盘 / 核销退差）

对应评审 P0-6 修正。原设计缺陷：
  「估算值 × 1.2 后扣减」+「检测到 usage 用精确值覆盖」，但覆盖的补偿算法未定义。
  若精确值 847 < 已扣 1016，多扣的 169 不退 → 全局 Token 预算被系统性提前 20% 耗尽。
  且「本地累加、SSE 结束一次性刷 Redis」使长 SSE 期间的消耗对其他实例不可见，
  超卖窗口 = 整个 SSE 时长（可达数分钟）。

本账本的语义约定（Spec 未定义，此处定案）：
  Token 属于「事后计量」，不能在请求准入时精确判断。因此拆成三个时点：
    1. admit   : 准入时只读检查余额是否已耗尽（超额则拒绝新请求，不影响在途请求）
    2. reserve : 请求期间按估算量增量预扣（SSE 每秒刷一次，压缩超卖窗口至 1s）
    3. settle  : 响应结束用上游精确 usage 核销，退回多扣部分（差额可为负）
  「先扣后核销」保证并发场景下不会超卖；「退差」保证不会系统性虚高。

ARGV[1] = op : admit | reserve | settle
KEYS[1]      : 账本 Key（窗口计数）

op=admit   : ARGV[2]=limit                 → 返回 {ok, used, pttl}
op=reserve : ARGV[2]=delta, ARGV[3]=expire → 返回 {1, used, 0}
op=settle  : ARGV[2]=reserved_total, ARGV[3]=actual_total, ARGV[4]=expire
             → 按 (actual - reserved) 修正，返回 {1, used, adjust}
]]

local op = ARGV[1]
local key = KEYS[1]

if op == 'admit' then
    local limit = tonumber(ARGV[2])
    local used = tonumber(redis.call('GET', key)) or 0
    local pttl = redis.call('PTTL', key)
    if pttl < 0 then pttl = 0 end
    if used >= limit then
        return { 0, used, pttl }
    end
    return { 1, used, pttl }

elseif op == 'reserve' then
    local delta = tonumber(ARGV[2])
    local expire = tonumber(ARGV[3])
    if delta <= 0 then
        return { 1, tonumber(redis.call('GET', key)) or 0, 0 }
    end
    local used = redis.call('INCRBY', key, delta)
    -- 仅在 Key 新建时设置过期，避免每次刷盘都续期导致窗口永不结束
    if used == delta then
        redis.call('EXPIRE', key, expire)
    end
    return { 1, used, 0 }

elseif op == 'settle' then
    local reserved = tonumber(ARGV[2])
    local actual = tonumber(ARGV[3])
    local expire = tonumber(ARGV[4])
    local adjust = actual - reserved

    if adjust == 0 then
        return { 1, tonumber(redis.call('GET', key)) or 0, 0 }
    end

    -- Key 已随窗口过期时不再补记：跨窗口回补会污染新窗口配额
    if redis.call('EXISTS', key) == 0 then
        if adjust > 0 then
            redis.call('INCRBY', key, adjust)
            redis.call('EXPIRE', key, expire)
            return { 1, adjust, adjust }
        end
        return { 1, 0, 0 }
    end

    local used = redis.call('INCRBY', key, adjust)
    -- 防御性归零：并发退差理论上不应为负，若为负说明存在重复 settle，归零并由调用方打点告警
    if used < 0 then
        redis.call('SET', key, 0, 'KEEPTTL')
        used = 0
    end
    return { 1, used, adjust }
end

return redis.error_reply('unknown op: ' .. tostring(op))
