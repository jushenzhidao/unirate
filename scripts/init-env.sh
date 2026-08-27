#!/usr/bin/env bash
# 生成含强随机凭证的 .env
#
# 为什么需要这个脚本：compose 里所有凭证都是必填（${VAR:?...}），
# 没有任何默认值。这不是为了折磨部署者，而是因为「公开在仓库里的默认凭证
# 等于没有凭证」—— 本项目曾用 change-me-admin-token-32chars-min 作为
# ADMIN_TOKEN 默认值，实测可直接访问管理面。
#
# 降低门槛的正确做法是一键生成强随机值，而不是给一个人人都知道的默认值。
#
# 幂等性：已存在 .env 时**拒绝覆盖**并退出非 0。
# 覆盖已有 .env 会让正在运行的 MySQL 数据卷与新密码不匹配，
# 表现为重启后 MySQL 认证失败 —— 这种故障排查成本很高，宁可让脚本失败。

set -euo pipefail

ENV_FILE="${ENV_FILE:-.env}"
EXAMPLE_FILE=".env.example"

c_g=$'\033[32m'; c_r=$'\033[31m'; c_y=$'\033[33m'; c_0=$'\033[0m'

if [ -e "$ENV_FILE" ]; then
  printf '%s%s 已存在，拒绝覆盖。%s\n\n' "$c_r" "$ENV_FILE" "$c_0"
  printf '  覆盖已有凭证会与既有 MySQL 数据卷不匹配，导致重启后认证失败。\n\n'
  printf '  确实要重新生成，请显式操作：\n'
  printf '    mv %s %s.bak && make init\n\n' "$ENV_FILE" "$ENV_FILE"
  printf '  若同时要重建数据卷（%s会清空配置与审计日志%s）：\n' "$c_y" "$c_0"
  printf '    docker compose down -v && mv %s %s.bak && make init\n' "$ENV_FILE" "$ENV_FILE"
  exit 1
fi

if ! command -v openssl >/dev/null 2>&1; then
  printf '%s缺少 openssl%s，无法生成强随机凭证。\n' "$c_r" "$c_0"
  printf '  macOS/Linux 通常自带；容器环境请先安装 openssl 包。\n'
  exit 1
fi

# base64 24 字节 → 32 字符，正好满足 ADMIN_TOKEN 的 >=32 要求。
# 去掉 base64 里的 / + = ：它们在 MySQL DSN 与 shell 引用中都需要转义，
# 一个没转义的 / 会让 DSN 解析出错，而报错信息完全指不到密码上。
gen() {
  local n="${1:-24}"
  openssl rand -base64 $((n * 2)) | tr -d '/+=\n' | cut -c1-$((n + 8))
}

ADMIN_TOKEN="$(gen 24)"
REDIS_PASSWORD="$(gen 18)"
MYSQL_PASSWORD="$(gen 18)"
MYSQL_ROOT_PASSWORD="$(gen 18)"
GRAFANA_PASSWORD="$(gen 18)"

# 生成后自检：ADMIN_TOKEN 必须真的够长。
# 若上面的 tr 过滤过于激进导致长度不足，宁可在这里失败，
# 也不要写出一个会让网关启动失败、而部署者不知道为什么的 .env。
if [ "${#ADMIN_TOKEN}" -lt 32 ]; then
  printf '%s生成的 ADMIN_TOKEN 长度不足（%d < 32）%s，请重试。\n' \
    "$c_r" "${#ADMIN_TOKEN}" "$c_0"
  exit 1
fi

# 端口沿用模板里的值，避免与模板漂移
read_port() {
  local key="$1" def="$2"
  if [ -f "$EXAMPLE_FILE" ]; then
    local v
    v=$(grep -E "^${key}=" "$EXAMPLE_FILE" 2>/dev/null | head -1 | cut -d= -f2)
    [ -n "$v" ] && { printf '%s' "$v"; return; }
  fi
  printf '%s' "$def"
}

umask 077
cat > "$ENV_FILE" <<EOF
# 由 make init 于 $(date '+%Y-%m-%d %H:%M:%S%z') 生成。
#
# 本文件含真实凭证，已被 .gitignore 排除，不要提交、不要贴进工单。
# 重新生成前请先备份：mv .env .env.bak

# ---- 端口映射（宿主侧）----
PROXY_PORT=$(read_port PROXY_PORT 28080)
OBS_PORT=$(read_port OBS_PORT 29091)
# Admin 端口只绑 127.0.0.1，不对外暴露。不要改成 0.0.0.0。
ADMIN_PORT=$(read_port ADMIN_PORT 29090)
PROM_PORT=$(read_port PROM_PORT 29092)
GRAFANA_PORT=$(read_port GRAFANA_PORT 29093)

# ---- 凭证（强随机生成，勿手改为弱值）----
ADMIN_TOKEN=${ADMIN_TOKEN}
REDIS_PASSWORD=${REDIS_PASSWORD}
MYSQL_PASSWORD=${MYSQL_PASSWORD}
MYSQL_ROOT_PASSWORD=${MYSQL_ROOT_PASSWORD}
GRAFANA_PASSWORD=${GRAFANA_PASSWORD}

# Admin 来源白名单（CIDR，逗号分隔）。刻意留在环境变量、不可从页面改 ——
# 否则攻击者拿到令牌后能自行放开来源限制。
ADMIN_ALLOW_CIDRS=172.16.0.0/12,10.0.0.0/8,127.0.0.0/8,192.168.0.0/16

# ---- 运行参数 ----
# 这两项属 Tier 1，也可在管理页面热改（页面值优先于此处）。
LOG_LEVEL=info
VERSION=dev

# ---- 镜像来源 ----
# 默认本地构建（up -d --build）。想用 CI 发布到 GHCR 的预构建镜像，
# 取消下面两行注释，再用 docker compose pull && docker compose up -d 启动
# —— 此模式下不要带 --build，否则本地构建会覆盖刚拉下来的 tag。
# 注意 GHCR 的语义化 tag 不带 v 前缀，写成 v0.1.4 会 404。
# GATEWAY_IMAGE=ghcr.io/jushenzhidao/unirate:latest
# GATEWAY_PULL_POLICY=always
EOF

chmod 600 "$ENV_FILE"

printf '%s已生成 %s%s（权限 600，已被 .gitignore 排除）\n\n' "$c_g" "$ENV_FILE" "$c_0"
printf '  ADMIN_TOKEN         %d 字符\n' "${#ADMIN_TOKEN}"
printf '  REDIS_PASSWORD      %d 字符\n' "${#REDIS_PASSWORD}"
printf '  MYSQL_PASSWORD      %d 字符\n' "${#MYSQL_PASSWORD}"
printf '  MYSQL_ROOT_PASSWORD %d 字符\n' "${#MYSQL_ROOT_PASSWORD}"
printf '  GRAFANA_PASSWORD    %d 字符\n\n' "${#GRAFANA_PASSWORD}"
printf '下一步：\n'
printf '  docker compose up -d --build\n'
printf '  ./test/e2e/run.sh\n\n'
printf '%s凭证已写入 %s，请勿明文分发。查看令牌：grep ADMIN_TOKEN %s%s\n' \
  "$c_y" "$ENV_FILE" "$ENV_FILE" "$c_0"
