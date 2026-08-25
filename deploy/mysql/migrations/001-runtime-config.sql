-- 迁移 001：新增 Tier 1 运行策略表
--
-- 为什么需要独立的迁移文件：
-- docker-entrypoint-initdb.d 下的 init.sql **只在数据卷为空时执行一次**。
-- 已在运行的部署（mysql-data 卷已有数据）不会重新跑 init.sql，
-- 因此升级时必须手工执行本文件。
--
-- 应用方式：
--   docker compose exec -T mysql mysql -u root -p"$MYSQL_ROOT_PASSWORD" unirate \
--     < deploy/mysql/migrations/001-runtime-config.sql
--
-- 幂等：CREATE TABLE IF NOT EXISTS，重复执行安全。
--
-- 回滚：
--   DROP TABLE runtime_config;
-- 回滚是安全的 —— 网关代码把「表不存在」当作「无覆盖项」处理，
-- 会回落到环境变量与内置默认值继续服务，不会启动失败。
-- 但请注意：回滚会丢失所有页面侧配置改动。

SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS runtime_config (
  cfg_key    VARCHAR(64)  NOT NULL COMMENT 'Tier 1 配置键，见 config.PolicyKeys',
  cfg_value  VARCHAR(255) NOT NULL COMMENT '原始字符串值，由应用层按 spec 解析',
  operator   VARCHAR(128) NOT NULL DEFAULT 'unknown' COMMENT '最后修改人',
  created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (cfg_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Tier 1 运行策略覆盖项（SoT）';
