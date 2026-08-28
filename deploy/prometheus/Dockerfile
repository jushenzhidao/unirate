# Prometheus 镜像：把抓取配置与告警规则内置进镜像，不再依赖 bind mount。
#
# 理由：bind mount 要求宿主上存在仓库目录，用 registry 镜像部署时通常没有
# deploy/ 这份文件，挂载失败只会得到一个空配置的 Prometheus
#（看似启动成功，实际什么都没抓）—— 这是典型的静默失效，必须避免。
#
# 构建上下文必须是 deploy/prometheus/ 而非仓库根目录：
# 根目录的 .dockerignore 显式排除了 deploy/prometheus，
# 用根上下文时 COPY 会因文件被忽略而失败。
FROM prom/prometheus:v2.53.0

COPY prometheus.yml /etc/prometheus/prometheus.yml
COPY rules.yml /etc/prometheus/rules.yml
