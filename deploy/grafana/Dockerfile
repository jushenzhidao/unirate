# Grafana 镜像：把数据源、面板 provider 与面板 JSON 内置进镜像。
#
# 关键陷阱 —— 面板 JSON 不能放 /var/lib/grafana/dashboards：
#   该路径被命名卷 grafana-data 覆盖。命名卷仅在首次为空时从镜像播种，
#   之后镜像里更新的面板不会再生效，表现为"改了 dashboard.json 却不更新"。
#   因此面板放 /etc/grafana/dashboards，并让 dashboard-provider.yml 指向它。
#
# 构建上下文必须是 deploy/grafana/ 而非仓库根目录：
# 根目录 .dockerignore 排除了 deploy/grafana，用根上下文 COPY 会失败。
FROM grafana/grafana:11.1.0

COPY datasource.yml /etc/grafana/provisioning/datasources/ds.yml
COPY dashboard-provider.yml /etc/grafana/provisioning/dashboards/dp.yml
COPY dashboard.json /etc/grafana/dashboards/unirate.json
