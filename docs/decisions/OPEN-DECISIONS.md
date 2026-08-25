# 悬而未决登记册（OPEN-DECISIONS）

> 只追加 + 就地关闭。每个 Phase 开始时复现未决项到工作上下文最前面，逐条判断能否关闭。
> 已关闭项可升格为 ADR。

**汇总**：3 未决 + 7 已决

---

## 未决项

| Date | Source | Open Item | Related Constraints | Current Leaning | Blocked By | Resolves When | Status |
|------|--------|-----------|---------------------|-----------------|------------|---------------|--------|
| 2026-08-23 | Phase 0 | MySQL 连接池 MaxOpenConns=16 是否偏保守 | 配置读取为低频操作（Bootstrap + Admin 写入 + 轮询兜底），但 Admin 管理页面上线后查询频次会上升 | 维持 16，先补 SetConnMaxIdleTime + 暴露 db.Stats() 指标；**判据已由 ADR-004 定案：WaitCount > 0 才上调** | 等 db.Stats().WaitCount 实测数据 | 压测显示 WaitCount > 0 时上调 | OPEN |
| 2026-08-23 | Phase 1 | store.Rules(biz) 每请求 append 合并全局+biz 规则是否值得预计算 | 该路径每请求调用（handler.go:147），但 atomic.Pointer 读本身无锁，分配量取决于规则数（demo 仅 4 条）；**当前无该路径基准数据** | 倾向不做——在未测量处优化属 gc-guide 明确警告的反模式 | 需 CPU profile 证明其为热点 | profile 显示占比 > 5% 则实施，否则关闭为「不做」 | OPEN |
| 2026-08-23 | Phase 3 | 剩余 4 个既有生产文件超「单文件 ≤300 行」规范：handler.go(561) / limiter.go(458) / store.go(365) / main.go(324)。**已消除 2 个**：`admin/server.go` 528→292（拆出 biz.go / audit.go）、`obs/metrics.go` 327→244（拆出 registry.go），两次拆分均因本轮要在这两个文件内新增指标端点，属「改到即拆」而非独立重构，全量 race 9 包 + e2e 50/50 已验证行为未变 | 贝洛奇已把自己新增的 policy.go 从 308 行拆成 3 个文件，但既有文件未动。这些正是本轮多人同时改动的核心：handler.go 被架构师 POC 与前端静态挂载同时触碰，server.go 被前端加路由，此刻重构必然冲突 | **本轮不拆，登记为技术债**。理由：①重构核心文件与进行中的 POC/前端工作直接冲突，风险收益不成比例 ②拆分不改变行为，无法用测试证明「拆对了」，只能靠 review，而 review 成本此刻最高 ③测试文件（limiter_test 581 / server_test 388）不适用同一标准，测试用例线性增长属正常 | 等本轮 POC 与前端落地收口 | 所有 in-flight 改动完成后，单独一轮重构并逐文件跑回归 | OPEN |

---

## 已决项

| Date | Source | Open Item | Resolution | Resolved At |
|------|--------|-----------|------------|-------------|
| 2026-08-23 | Phase 1 | obs 端口（29091）当前 `0.0.0.0` 全网暴露且无鉴权，`/metrics` 含 biz 名/规则名/拒绝分布等内部拓扑信息 | **本轮不改宿主端口绑定，改为在部署文档登记为部署期约束；信息泄露的实际入口已由 `/admin/metrics` 承接而收窄**。查证依据（贝洛奇实测）：①`deploy/prometheus/prometheus.yml` 抓取目标是 `gateway:9091`，走 compose 内网 DNS，**与宿主 `0.0.0.0:29091` 映射无关** —— 已用同网络容器验证 `http://gateway:9091/metrics → 200`，故「收紧会打断 Prometheus 抓取」这一顾虑不成立 ②但 Prometheus/Grafana 位于 `profiles: [obs]`，**默认不启动**，当前实际无任何组件依赖宿主映射做抓取 ③真正依赖宿主 29091 的是**探活与验收链路**：`Makefile:84`、`.github/workflows/ci.yml:219` 的 `/ready` 探活与 `test/e2e/run.sh:16` 的 50 项验收全部从宿主 curl 该端口。改绑定必须同步改这三处 + `scripts/init-env.sh`，属跨 CI/Makefile/e2e 的改动面，而本轮有三人并行改动 ④绑定收紧本身**不是纵深防御的正确落点**：`0.0.0.0` 在容器语境下指容器内网卡，暴露面由 compose 的 `ports:` 映射决定，把它改成内网网段是在错误的层解决问题，正确做法是删除宿主映射或用 `127.0.0.1:` 前缀（与 admin 一致） | Phase 3（贝洛奇实测抓取链路后关闭；遗留动作：给 `OBS_PORT` 映射加 `127.0.0.1:` 前缀并同步改 Makefile/CI/e2e/init-env 四处，单独一轮做） |
| 2026-08-23 | Phase 1 | 监控看板的指标数据源：obs 端口 `/metrics` 跨源取数 vs admin 端口同源新端点 | **admin 端口新增受鉴权的指标读取端点，看板同源取数**。理由：①看板是管理员功能，鉴权边界必须与其他三模块一致，不能同一界面里一半要令牌一半裸奔 ②跨端口即跨源需开 CORS，而在无鉴权端口开 CORS 等于让任意网页可读运行指标 ③obs 端口继续专供 Prometheus 抓取，职责不混淆。来源：用户指出「看板本质就是管理员的一部分」 | Phase 1 门禁（用户提出） |
| 2026-08-23 | Phase 1 | 监控看板的数据刷新机制：轮询 vs SSE 推送 | **定为轮询（默认 5s，可配），不做 SSE 推送**。理由：①指标读取开销实测可忽略——`/metrics` 单次响应 8043 B / 113 行，`curl -w` 三次测得 5.05ms / 3.90ms / 3.88ms（含 HTTP 往返与 curl 启动开销，实际序列化远低于此），5s 轮询的成本相对网关自身负载可忽略 ②新增推送端点会扩大攻击面，与 obs 端口收紧的方向相反 ③实现成本低一个量级。注：数据源仍按上一条已决项走 admin 同源端点 | ADR-006 §8（Phase 1 架构师实测） |
| 2026-08-23 | Phase 1 | 是否需显式设置 GOMAXPROCS/GOMEMLIMIT | **GOMEMLIMIT 设为容器内存 limit × 0.9 并同时给容器设 limit；GOGC 保持默认 100 不设 off；GOMAXPROCS 当前不设但须写入部署文档**。查证结论：①容器感知 GOMAXPROCS 是 Go 1.25 特性且要求 go.mod 声明 go 1.25+，本项目 go.mod 为 `go 1.22`，**该特性不生效** ②但 compose 当前也未设 CPU limit，故不存在 cgroup quota 与 NumCPU() 错配，**现状无问题**；一旦未来给容器设 CPU limit 而仍用 Go 1.22 构建，会触发 CFS throttling 与 p99 尖刺，届时须显式设 GOMAXPROCS 或引入 automaxprocs ③GOGC=off 被官方 gc-guide 明确警告会在 live heap 逼近 limit 时触发 GC 抖动（程序不 OOM 但无限期停滞，「比 OOM 更糟」），网关作为基础设施可用性优先，不接受该风险 ④GOMEMLIMIT 须留 5-10% 余量（官方建议）且必须配合容器 limit，单独设置无基准可依 | ADR-005 §二；来源 go.dev/doc/gc-guide + Go 1.25 release notes |
| 2026-08-23 | Phase 1 | 压测时是否给 gateway 容器显式限核 | **不限核，改用「同环境 + 预热 + 5 轮取中位数」控制噪声；若需权威数据则在专用环境复测**。理由：①限核会让被测对象偏离真实部署形态，而本轮优化目标（吞吐/并发）恰恰依赖真实可用核数，限核后测出的天花板不具参考价值 ②噪声问题可用统计手段解决——实测同配置 QPS 波动约 ±8%（PoolSize 128 vs 256 的 88,288 vs 87,804 即在噪声内），5 轮中位数足以区分 >10% 的真实提升 ③本机同时运行 20+ 无关容器，限核也无法消除邻居干扰，压测前记录 `docker stats` 全局快照更有效 ④预热是更关键的变量：EVALSHA 冷路径为百微秒级、热路径为个位数到二十几微秒级，**差一个数量级以上**，不预热的数据一律作废。（注：此处曾先后写作「9 倍」与「40 倍」，两者均不成立——样本来自不同脚本，`eval` 计数是三个正式脚本的首次加载，`evalsha` 累积值绝大部分是简化探针，分母口径不同不可直接相除。现改为数量级表述，足以支撑「必须预热」且不引入错误精度。） | ADR-008 §四（可比性保证） |
| 2026-08-23 | Phase 0 | 管理控制台采用暗色还是亮色主题 | **双主题，`:root[data-theme]` 单一来源，默认跟随系统偏好**。理由：使用场景本身分裂——白天调配额用 light，夜间 oncall 查拒绝率用 dark，只给一种必然有一半时间是错的，而 SRE 恰是最易夜间使用的人群。语义 token 两套值一一对齐，组件 CSS 零分支，增量成本接近零。tokens.css 刻意不写 `@media(prefers-color-scheme)` 分支，避免 dark 值维护两份导致漂移；改由 head 内联脚本首绘前写入 data-theme（含 try/catch 防隐私模式抛错）。dark 画布用 `#0e1116` 而非纯黑，减少长时间监控的光晕疲劳。**已由我独立复算对比度验证通过** | Phase 1 门禁 |
| 2026-08-23 | Phase 1 | TokenAdmit 与 Check 合并为单次往返的真实净收益 | **POC 达标并已实施**。实测 QPS +32.4%~+35.9%（阈值 ≥20%），p50 −22.7%~−27.3%，p99 −16.0%~−18.3%；Redis CPU 全档 0.999 核证明其仍为瓶颈、归因成立。验收全通过：race 全绿、限流内核 29 项零跳过、e2e 50/50、场景 B 五轮零误差。实施中发现 `ipairs` 空洞会使 Phase 2 静默跳过后续提交（等同 P0-1 复现），已用 noop 占位防住并经变异验证 | Phase 1（ADR-003 已转 Accepted & Implemented） |
