--[[
release_conc.lua — 并发计数精确释放

对应评审 P0-4 修正第 3 项：
  原设计用 DECR 释放，存在两个静默错误：
    a) key 级 TTL 过期后，旧请求的 DECR 会误减新窗口的计数
    b) 重复释放 / 乱序释放会把计数减成负数，导致并发限流彻底失效
  改为 ZREM(request_id)：幂等、精确、不可能产生负值。
  即使请求在网关崩溃后从未释放，成员也会被 batch_check 的 deadline 清扫回收。

KEYS[1..N] : 该请求持有的所有并发 Key
ARGV[1]    : 全局唯一请求 ID
返回       : 实际移除的成员数（用于观测泄漏率）
]]

local removed = 0
for i = 1, #KEYS do
    removed = removed + redis.call('ZREM', KEYS[i], ARGV[1])
end
return removed
