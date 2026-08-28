-- 测试专用种子数据 —— 演示业务域 demo。
--
-- 仅由 docker-compose.test.yml 挂载到 gateway 的 SEED_SQL_DIR 下加载，
-- 生产编排不挂载本目录，因此不会进入生产库。
--
-- 迁移说明：本文件替代原 deploy/mysql/testdata/02-demo.sql。
-- MySQL 版用 JSON_ARRAY/JSON_OBJECT 构造，SQLite 无这些函数，
-- 直接写 JSON 文本 —— 应用层读出后仍是 json.Unmarshal，语义等价。
--
-- e2e（test/e2e/run.sh）大量用例依赖此业务域及其规则 id/window/limit，
-- 修改任一数值会直接使断言失效，调整前请同步核对 run.sh。
--
-- 规则设计意图（与评审结论对应，勿随意改算法字段）：
--   id=10 固定窗口：先判后增，超限不污染计数器（P0-5）
--   id=11 滑动窗口：ZSet + 唯一成员，避免同毫秒请求互相覆盖
--   id=12 并发控制：ZSet + deadline 清扫，请求结束精确释放（P0-4）
--   id=13 Token 预算：必须用窗口语义，令牌桶在此处会被 Validate 拒绝（P0-2）

INSERT INTO biz_config (biz, base_url, path_strip_prefix, enabled, rules_json, metering_json)
VALUES (
  'demo',
  'http://mock-upstream:9000',
  1,
  1,
  '[
    {"id":10,"name":"demo-ip-qps","type":"rate","metric":"request",
     "dimensions":["biz","ip"],"window":"1s","limit":10,
     "algorithm":"fixed_window","enabled":true},
    {"id":11,"name":"demo-token-sliding","type":"rate","metric":"request",
     "dimensions":["biz","token"],"window":"10s","limit":30,
     "algorithm":"sliding_window","enabled":true},
    {"id":12,"name":"demo-concurrency","type":"concurrency",
     "dimensions":["biz"],"max_concurrent":50,"timeout":120,
     "enabled":true},
    {"id":13,"name":"demo-token-budget","type":"rate","metric":"token",
     "dimensions":["biz","token"],"window":"1h","limit":100000,
     "algorithm":"fixed_window","watermark":80,"enabled":true}
  ]',
  '{"mode":"auto","json_path":"usage.total_tokens",
    "header_name":"X-Usage-Tokens","estimate_ratio":0.4,"safety_buffer":1.2}'
)
ON CONFLICT(biz) DO UPDATE SET
  base_url = excluded.base_url,
  path_strip_prefix = excluded.path_strip_prefix,
  enabled = excluded.enabled,
  rules_json = excluded.rules_json,
  metering_json = excluded.metering_json
