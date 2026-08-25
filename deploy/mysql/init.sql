-- unirate 频控网关配置库
--
-- 设计说明：MySQL 是配置的唯一 Source of Truth（对应评审 P0-7）。
-- 规则以 JSON 列存储而非拆成规则表，理由：
--   1. 规则整体是「一个业务域的完整策略」，按 biz 原子读写最贴合使用方式；
--   2. 网关读取路径实际走 Redis 快照，MySQL 不承担高频查询，无需为查询优化建模；
--   3. 规则 schema 演进频繁，JSON 免去反复 DDL。
-- 校验由应用层 Rule.Validate() 承担，写入前强制执行。

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS biz_config (
  biz               VARCHAR(64)  NOT NULL COMMENT '业务域标识，* 表示全局规则',
  base_url          VARCHAR(512) NOT NULL DEFAULT '' COMMENT '上游基础地址',
  path_strip_prefix TINYINT(1)   NOT NULL DEFAULT 1 COMMENT '转发时是否剥离 /{biz} 前缀',
  enabled           TINYINT(1)   NOT NULL DEFAULT 1,
  rules_json        JSON         NULL COMMENT '限流规则数组',
  metering_json     JSON         NULL COMMENT 'Token 计量配置',
  created_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (biz)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='业务域配置（SoT）';

-- 审计日志：Spec 完全未要求，但管理面无审计等于没有问责能力。
-- 与配置变更同事务写入，保证「有变更必有记录」。
CREATE TABLE IF NOT EXISTS audit_log (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  action      VARCHAR(64)  NOT NULL,
  biz         VARCHAR(64)  NOT NULL DEFAULT '',
  operator    VARCHAR(128) NOT NULL DEFAULT 'unknown',
  remote_addr VARCHAR(64)  NOT NULL DEFAULT '',
  detail      TEXT         NULL,
  created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_biz_time (biz, created_at),
  KEY idx_action_time (action, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='管理面审计日志';

-- Tier 1 运行策略覆盖项（见 docs/decisions/CONFIG-TIERING.md）
--
-- 只存「页面显式改过的项」，不预填全量默认值。理由：
--   1. 表里有一行就代表「运维做过决策」，能与"从未碰过"区分开；
--   2. 预填默认值会把默认值固化进 SoT，将来调整代码内置默认值时，
--      老部署仍会读到旧值，且没人知道那一行是自动写的还是人工改的。
-- 未在此表出现的项按 环境变量 → 内置默认 回落。
--
-- 键名白名单与上下限由应用层 config.ValidatePolicyOverrides() 强制，
-- 写入前完成校验；DB 侧只做长度约束，不重复定义业务规则（避免两处漂移）。
CREATE TABLE IF NOT EXISTS runtime_config (
  cfg_key    VARCHAR(64)  NOT NULL COMMENT 'Tier 1 配置键，见 config.PolicyKeys',
  cfg_value  VARCHAR(255) NOT NULL COMMENT '原始字符串值，由应用层按 spec 解析',
  operator   VARCHAR(128) NOT NULL DEFAULT 'unknown' COMMENT '最后修改人',
  created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (cfg_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Tier 1 运行策略覆盖项（SoT）';

-- ---------------------------------------------------------------------------
-- 演示配置
-- ---------------------------------------------------------------------------

-- 全局兜底规则：所有业务域共同生效，防止单点打爆整个网关
INSERT INTO biz_config (biz, base_url, path_strip_prefix, enabled, rules_json, metering_json)
VALUES ('*', '', 0, 1, JSON_ARRAY(
  JSON_OBJECT(
    'id', 1, 'name', 'global-qps-guard', 'type', 'rate', 'metric', 'request',
    'dimensions', JSON_ARRAY('global'), 'window', '1s', 'limit', 5000,
    'algorithm', 'fixed_window', 'enabled', TRUE
  )
), NULL)
ON DUPLICATE KEY UPDATE rules_json = VALUES(rules_json);

-- 演示业务域：指向 compose 内置的 mock 上游
INSERT INTO biz_config (biz, base_url, path_strip_prefix, enabled, rules_json, metering_json)
VALUES ('demo', 'http://mock-upstream:9000', 1, 1, JSON_ARRAY(
  -- 固定窗口：先判后增，超限不污染计数器（评审 P0-5）
  JSON_OBJECT(
    'id', 10, 'name', 'demo-ip-qps', 'type', 'rate', 'metric', 'request',
    'dimensions', JSON_ARRAY('biz','ip'), 'window', '1s', 'limit', 10,
    'algorithm', 'fixed_window', 'enabled', TRUE
  ),
  -- 滑动窗口：ZSet + 唯一成员，避免同毫秒请求互相覆盖
  JSON_OBJECT(
    'id', 11, 'name', 'demo-token-sliding', 'type', 'rate', 'metric', 'request',
    'dimensions', JSON_ARRAY('biz','token'), 'window', '10s', 'limit', 30,
    'algorithm', 'sliding_window', 'enabled', TRUE
  ),
  -- 并发控制：ZSet + deadline 清扫，请求结束精确释放（评审 P0-4）
  JSON_OBJECT(
    'id', 12, 'name', 'demo-concurrency', 'type', 'concurrency',
    'dimensions', JSON_ARRAY('biz'), 'max_concurrent', 50, 'timeout', 120,
    'enabled', TRUE
  ),
  -- Token 预算：必须用窗口语义，令牌桶在此处会被 Validate 拒绝（评审 P0-2）
  JSON_OBJECT(
    'id', 13, 'name', 'demo-token-budget', 'type', 'rate', 'metric', 'token',
    'dimensions', JSON_ARRAY('biz','token'), 'window', '1h', 'limit', 100000,
    'algorithm', 'fixed_window', 'watermark', 80, 'enabled', TRUE
  )
), JSON_OBJECT(
  'mode', 'auto', 'json_path', 'usage.total_tokens',
  'header_name', 'X-Usage-Tokens', 'estimate_ratio', 0.4, 'safety_buffer', 1.2
))
ON DUPLICATE KEY UPDATE
  base_url = VALUES(base_url),
  rules_json = VALUES(rules_json),
  metering_json = VALUES(metering_json);
