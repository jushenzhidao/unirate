#!/usr/bin/env bash
# unirate 压测驱动（ADR-008 的可执行入口）
#
# 职责分工：
#   本脚本（宿主侧）—— 采集容器 CPU（容器内拿不到 docker socket）、编排轮次、落盘产物
#   loadgen（容器内）—— 施压、采集客户端分位数与 /metrics、Redis commandstats、判定
#
# 为何 CPU 必须由宿主采：压测程序运行在容器内，无 docker socket，
# 无法调 `docker stats`。而双侧 CPU 是 ADR-008 的强制归因依据
# （首版把「简化脚本的 88k 平台期」误读为「Redis 饱和」，根因就是缺这份数据）。
#
# 用法：
#   bash test/perf/run.sh A                    # 场景 A
#   bash test/perf/run.sh B                    # 场景 B（拒绝路径精度）
#   bash test/perf/run.sh all                  # 四场景全跑
#   LABEL=after-opt bash test/perf/run.sh A    # 打标签便于前后对比
#
# 环境变量：
#   ROUNDS/DURATION/WARMUP/CONC  覆盖默认参数（默认遵循 ADR-008：5 轮 / 20s / 30s）
#   LABEL                        产物标签（如 before-opt / after-opt）
#   QUICK=1                      快速模式（1 轮 / 5s / 5s 预热），仅用于验证程序本身

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

NET="${NET:-unirate_unirate}"
IMAGE="${IMAGE:-golang:1.22-alpine}"
GOMODCACHE_VOL="${GOMODCACHE_VOL:-unirate-gomod}"
BASELINE_DIR="test/perf/baseline"
TS="$(date +%Y%m%d-%H%M%S)"
LABEL="${LABEL:-baseline}"

ROUNDS="${ROUNDS:-5}"
DURATION="${DURATION:-20s}"
WARMUP="${WARMUP:-30s}"

if [ "${QUICK:-0}" = "1" ]; then
  ROUNDS=1; DURATION="${DURATION_QUICK:-5s}"; WARMUP="${WARMUP_QUICK:-5s}"
  echo "[QUICK] 快速模式：${ROUNDS} 轮 / ${DURATION} / 预热 ${WARMUP} —— 仅用于验证程序，不可作基线"
fi

# 凭证解析：优先环境变量 → .env → **运行中容器的实际配置**。
#
# 最后一级是必须的：项目已改为 `make init` 生成强随机凭证，
# 仓库里不再有可用的默认值。硬编码默认密码会让压测在
# 健康检查阶段以 WRONGPASS 失败（实测踩过）。
# 从运行中的容器取值，保证压测总是对着「当前真实在跑的那套」。
env_of() { # env_of <container> <KEY>
  docker inspect "$1" --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | grep -E "^$2=" | head -1 | cut -d= -f2- || true
}
from_env_file() {
  [ -f .env ] || return 0
  grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2- || true
}

REDIS_PASSWORD="${REDIS_PASSWORD:-$(from_env_file REDIS_PASSWORD)}"
[ -n "$REDIS_PASSWORD" ] || REDIS_PASSWORD="$(env_of unirate-gateway REDIS_PASSWORD)"

ADMIN_TOKEN="${ADMIN_TOKEN:-$(from_env_file ADMIN_TOKEN)}"
[ -n "$ADMIN_TOKEN" ] || ADMIN_TOKEN="$(env_of unirate-gateway ADMIN_TOKEN)"

if [ -z "$REDIS_PASSWORD" ]; then
  echo "警告：未解析到 REDIS_PASSWORD，健康检查可能以 WRONGPASS 失败" >&2
fi
if [ -z "$ADMIN_TOKEN" ]; then
  echo "警告：未解析到 ADMIN_TOKEN，场景 B 无法创建专属业务域" >&2
fi

mkdir -p "$BASELINE_DIR"

# 兜底清理：任何异常退出都不留下孤儿采样进程
SAMPLER_PIDS=""
cleanup_samplers() {
  local p
  for p in ${SAMPLER_PIDS:-}; do
    kill "$p" 2>/dev/null || true
  done
}
trap cleanup_samplers EXIT INT TERM

# --- 前置检查：容器在跑且网络存在 ---------------------------------------------
if ! docker network inspect "$NET" >/dev/null 2>&1; then
  echo "错误：docker 网络 $NET 不存在。请先 docker compose up -d" >&2
  exit 1
fi
for c in unirate-gateway unirate-redis unirate-mock-upstream; do
  if ! docker ps --format '{{.Names}}' | grep -qx "$c"; then
    echo "错误：容器 $c 未运行。请先 docker compose up -d" >&2
    exit 1
  fi
done

# --- 宿主侧 CPU 采样器 --------------------------------------------------------
# 每秒采一次 gateway 与 redis 的 CPU%，写入 <cpufile>。
# `docker stats --no-stream` 每次调用约耗时 1-2s，故实际间隔约 2s，够用。
start_cpu_sampler() {
  local out="$1"
  : > "$out"
  # 关键：子 shell 必须重定向 stdout，否则它会继承并持有命令替换 $(...) 的管道，
  # 使调用方永久阻塞（这里踩过一次：脚本卡死到 timeout 才发现）。
  #
  # 每行带 epoch 秒前缀：loadgen 只统计**正式采集窗口内**的样本，
  # 剔除预热与轮间重置期。否则 CPU 均值被空闲期稀释，
  # 归因会系统性低估负载侧 CPU —— 这正是首版 88k 归因失准的同类错误。
  (
    while :; do
      ts=$(date +%s)
      docker stats --no-stream --format '{{.Name}} {{.CPUPerc}}' \
        unirate-gateway unirate-redis 2>/dev/null \
        | while IFS= read -r line; do printf '%s %s\n' "$ts" "$line"; done >> "$out" || true
      # 不额外 sleep：docker stats --no-stream 本身约耗时 1-2s，
      # 已构成采样间隔。再 sleep 会让短轮次（如场景 B 的瞬时齐发）取不到样本。
    done
  ) >/dev/null 2>&1 &
  printf '%s' "$!"
}

run_scenario() {
  local sc="$1"; shift
  local cpufile="/tmp/unirate-perf-cpu-${sc}-${TS}.txt"
  local outfile="${BASELINE_DIR}/${TS}-scenario${sc}-${LABEL}.json"

  echo
  echo "############################################################"
  echo "# 场景 $sc   标签=$LABEL   轮数=$ROUNDS  单轮=$DURATION  预热=$WARMUP"
  echo "############################################################"

  local sampler_pid
  sampler_pid="$(start_cpu_sampler "$cpufile")"
  # 刻意不用 `trap ... RETURN` 清理采样器：它与函数内 local 变量作用域
  # 交互会破坏调用方的变量（实测触发过 "overall: unbound variable"）。
  # 采样器在本函数末尾显式 kill，异常退出时由下方 EXIT trap 兜底。
  SAMPLER_PIDS="${SAMPLER_PIDS:-} $sampler_pid"

  docker run --rm --network "$NET" \
    -v "$ROOT":/src -w /src/test/perf/loadgen \
    -v "$GOMODCACHE_VOL":/go/pkg/mod \
    -v "$cpufile":/tmp/cpu.txt:ro \
    -e GOFLAGS=-mod=mod -e GOPROXY=https://goproxy.cn,direct \
    -e REDIS_PASSWORD="$REDIS_PASSWORD" \
    -e ADMIN_TOKEN="$ADMIN_TOKEN" \
    -e PERF_CPUFILE=/tmp/cpu.txt \
    "$IMAGE" \
    go run . \
      -scenario="$sc" \
      -rounds="$ROUNDS" -duration="$DURATION" -warmup="$WARMUP" \
      -label="$LABEL" \
      -out="/src/${outfile}" \
      ${CONC:+-conc="$CONC"} \
      "$@"
  local rc=$?

  kill "$sampler_pid" 2>/dev/null || true
  echo "CPU 采样：$(wc -l < "$cpufile" 2>/dev/null || echo 0) 条 → $cpufile"
  return $rc
}

SCENARIO="${1:-A}"
overall=0
case "$SCENARIO" in
  all)
    for sc in A B C D; do
      run_scenario "$sc" || overall=$?
    done
    ;;
  A|B|C|D)
    run_scenario "$SCENARIO" || overall=$?
    ;;
  *)
    echo "用法：bash test/perf/run.sh [A|B|C|D|all]" >&2
    exit 1
    ;;
esac

echo
echo "产物目录：${BASELINE_DIR}"
ls -1 "$BASELINE_DIR" 2>/dev/null | tail -5
# 变量一律用 ${} 包裹：紧邻中文全角字符时，裸 $overall 会被解析成
# 含多字节尾巴的变量名，在 set -u 下报 "overall<乱码>: unbound variable"（实测踩过）。
if [ "${overall}" -ne 0 ]; then
  echo "存在未通过的判定项（退出码 ${overall}）——详见上方 FAIL 项" >&2
fi
exit "${overall}"
