# unirate 管理控制台 · DESIGN.md

> 设计师：颜好看 | 寄存器：**Product**（设计服务产品，不是产品本身）
> 三轴刻度：Variance **3** / Motion **2** / Density **8**
> 交付物：`design-tokens.css` · `design-tokens.json` · `icon-sprite.svg` · 本文档

Density 拉到 8、Variance 压到 3、Motion 压到 2，是因为使用者是在故障中的 SRE。
非对称布局和动效在这个场景全是负资产——他们要的是「每次打开位置都一样，一屏看完，读到真值」。

---

## 1. 视觉主题与对标

### 1.1 对标品牌（选 3 个 + 各取一件事）

| 对标 | 取什么 | 不取什么 |
|---|---|---|
| **Grafana** | 状态语义色的克制用法：绿/琥珀/红只出现在阈值判定处，图表主体是中性描线；面板 = 1px 边框的矩形，不是带阴影的卡片 | 面板可拖拽自定义布局（本控制台是固定 4 模块，不做画布） |
| **Vercel（Geist）** | 数字全走 mono + tabular-nums；状态色只出现在 ≤10px 的点/徽章上，绝不做大面积填充；层级靠 spacing 和字重而非颜色 | 纯黑 `#000` 画布（对比过强，长时间盯屏疲劳）与 `#0070f3`（太标志性，抄了就是 Vercel 克隆） |
| **Linear** | 受限字号阶梯（正文 13px，全站 7 级封顶）+ 80-120ms 的极短过渡 | 主强调色 `#5E6AD2`（紫向，撞 P0-2 红线气味）与大量键盘驱动的隐藏交互 |

补充参考 **Kong Manager** 的一条信息架构决策：把「配置实体管理」和「Vitals 监控」放在同级导航而不是把监控塞进详情页——出问题时人是先看监控再跳规则，两者都得是一级入口。

### 1.2 主题氛围

关键词：**克制、可核对、无歧义**。

界面读起来应该像一份排版良好的运行报表，不像一个产品。判据很直接——如果某个视觉元素不能回答「哪条规则拒了流量」「配额还剩多少」「配置生效了没」，它就不该存在。

### 1.3 明暗决策：**双主题，默认跟随系统**

IDE 主题是 light，但控制台自身独立决定。结论是**两套都做**，理由：

1. **场景是分裂的。** 白天调配额（办公室、亮环境、可能投屏对齐需求）用 light；半夜 oncall 查拒绝率（暗环境）用 dark。只给一个必然有一半时间是错的，而 SRE 恰恰是最容易夜间使用的人群。
2. **成本几乎为零。** 语义 token 已经把两套值一一对齐（`--c-*` 名称完全相同），组件 CSS 零分支。多出的代价只是 tokens.css 里一段 dark 覆盖块。
3. **默认值给系统偏好**，而非硬编码某一个。实现上主题由 `html[data-theme]` 单一来源决定（`"light"|"dark"`），`<head>` 内联脚本在首绘前同步写入：初值读 `localStorage.unirate_theme`，缺失时回落 `matchMedia('(prefers-color-scheme: dark)')`。tokens.css 里**不写** `@media (prefers-color-scheme)` 分支——否则 dark 的值要维护两份，迟早漂移。
4. 不选纯黑：dark 画布用 `#0e1116`（带蓝相的近黑），面板 `#161a20`。纯黑 + 亮字的高对比在长时间监控下会产生光晕疲劳，Grafana 和 GitHub 都是这个取向。

### 1.4 配色理由

主强调色 **`#1957c8`（light）/ `#4c8dff`（dark）** —— 钢蓝偏深。

- 为什么是蓝：基础设施工具的既有语言就是蓝（可信、非情绪化），且蓝不与任何语义色（绿/琥珀/红/青）冲突，能安全地承担「可交互」这一唯一职责。
- 为什么不是 `#6366f1`：Tailwind 默认 indigo，一眼 AI 生成，且紫向色相在企业运维语境里显轻。
- 为什么不是 `#0070f3`：Vercel 的签名色，用了就是复刻。
- 降级态单独给了**青色 `#0d6b78`**，不复用琥珀。因为「Redis 挂了但本地兜底放行」和「配额接近上限」是完全不同的两件事，混用同一个颜色会让人在故障中做出错判——这是这套配色里最重要的一个决定。

强调色配额：**每屏 ≤2 处**。实际落点是「主操作按钮」+「当前选中的导航项标记」。表格里的链接不算（用 `--c-fg` + hover 下划线）。

---

## 2. 色彩与语义

完整值见 `design-tokens.css` / `design-tokens.json`。这里只写规则。

### 2.1 五态映射表（前端唯一查表处）

| 状态 | 语义 token | 图标 | 点形状 | 文案 | 触发条件 |
|---|---|---|---|---|---|
| healthy | `--c-ok` | `i-status-ok` | 圆 | 正常 | breaker=0、无降级计数增长 |
| warning | `--c-warn` | `i-status-warn` | 三角 | 接近上限 | `rule_watermark_ratio_percent` ≥ 80 |
| degraded | `--c-degraded` | `i-status-degraded` | 菱形 | 降级 | `degraded_decisions_total` 增长中 或 snapshot.degraded=true |
| failed | `--c-danger` | `i-status-fail` | 方 | 故障 | `redis_breaker_open`=1 或 admin 接口 5xx |
| disabled | `--c-disabled` | `i-status-off` | 空心环 | 已停用 | biz.enabled=false / rule.enabled=false |

**颜色永不单独承载语义。** 每个状态渲染必须同时输出 图标形状 + 文字标签 + 颜色三者。色盲用户靠形状和文字，一样能读。

### 2.2 边框有两个 token，别用错

实测 `--c-border` 只有 **1.38:1**（light）/ **1.31:1**（dark）。这对分隔线是对的（装饰性，WCAG 1.4.11 不适用，也不该抢注意力），但对**靠边框才能被识别的控件**不合格。

- `--c-border` → 表格分隔线、面板轮廓、区块分割
- `--c-border-field`（3.11:1 / 3.63:1）→ 输入框、下拉、次级按钮、开关、复选框

同理 `--c-meta`（3.11:1 / 3.63:1）只能用于 placeholder 和已停用态标签；任何用户必须读到的元数据用 `--c-muted`（≥5.2:1）。审计日志的时间、IP、操作者都属于"必须读到"，用 `--c-muted`。

### 2.3 四层配比

中性色 88% / 强调色 6% / 语义色 5% / 效果色 1%。语义色只出现在：状态徽章、图表拒绝序列、表单错误、危险按钮描边。不做大面积语义色填充（`*-soft` 底色仅用于 18px 高的徽章和错误提示条）。

---

## 3. 排版

### 3.1 字体决策：放弃品牌字形，锁定受控系统栈

零外链约束下不能 `@import` 网络字体，也不能内嵌 base64 字体（会把单文件撑到几百 KB，违背 embed 进二进制的初衷）。所以这里**明确不假装有品牌字体**，改为：

- `--font-sans`：`ui-sans-serif` → `-apple-system` → `Segoe UI Variable Text` → `PingFang SC` / `Microsoft YaHei UI`，中英文回退链完整（内网大概率是 Windows + Chrome，`Segoe UI Variable Text` 必须在链上）。
- `--font-mono`：`ui-monospace` → `SF Mono` → `JetBrains Mono` → `Cascadia Mono` → `Consolas`。

工艺由**排版契约**保证而非字形：7 级字号封顶、三级字重（400/510/590）、显式字距规则、数字强制 `tabular-nums`。

### 3.2 数字必须等宽 —— 本项目最容易被忽略的一条

QPS、拒绝率、P99、Token 数、config version、时间戳、biz 名、path 全部走 `--font-mono` + `font-variant-numeric: tabular-nums`。

原因具体到场景：看板每 5 秒轮询刷新，如果数字用比例字体，`1` 和 `8` 宽度不同，整列会在每次刷新时左右抖动，人眼没法比较相邻两行的量级。这是 Stripe/Vercel 表格做对的核心细节。

### 3.3 字号阶梯（7 级 + 1 个 KPI 例外）

| token | 值 | 用途 | 字重 | 字距 |
|---|---|---|---|---|
| `--fs-micro` | 11px | 表头 ALL CAPS、徽章 | 510 | **0.08em** |
| `--fs-cap` | 12px | 元数据、helper text | 400 | 0.01em |
| `--fs-body` | 13px | 正文、表格单元、输入框 | 400 | 0 |
| `--fs-lg` | 14px | 强调正文、卡片标题 | 510 | 0 |
| `--fs-title` | 16px | 面板标题 | 590 | -0.01em |
| `--fs-sec` | 20px | 页面标题 | 590 | -0.01em |
| `--fs-kpi` | 28px | KPI 数字（仅 mono） | 590 | -0.02em |

12px 元数据的对比度**不放宽到 3:1**——12px 不算大字号，`--c-muted` 在两套主题下都做到了 ≥5.5:1。

---

## 4. 布局骨架

```
┌──────────────────────────────────────────────────────────────────────┐
│ topbar 48px  unirate ▸ 监控   [健康胶囊] [v#号] [自动刷新] [主题] [登出]│
├────────────┬─────────────────────────────────────────────────────────┤
│ sidebar    │  content  padding 24px   max-width 1440px               │
│ 216px      │  ┌─ 页面标题行（标题 + 主操作）────────────────────────┐ │
│            │  ├─ 工具栏（筛选 / 搜索 / 时间范围）──────────────────┤ │
│ 监控看板    │  └─ 面板区 / 表格区 ────────────────────────────────┘ │
│ 限流规则    │                                                         │
│ 审计日志    │                                                         │
│ 配置快照    │                                                         │
│            │                                                         │
│ ─────────  │                                                         │
│ 主题 · 登出 │                                                         │
└────────────┴─────────────────────────────────────────────────────────┘
```

**topbar 常驻健康胶囊**是这个布局的关键决定：不管在哪个模块，Redis 熔断状态 + 当前 config version 永远可见。SRE 在改规则时必须能同时看到「集群是否处于降级」——否则会在 Redis 挂着的时候误判配额没生效。

- 侧栏导航项 4 个（认知负荷 ≤4，符合工作记忆上限），选中态 = 左侧 2px `--c-accent` 竖标记 + `--c-surface-3` 底 + `--c-fg` 文字。
- 面板 = `--c-surface` + 1px `--c-border` + `--r-lg`，**默认无阴影**（避免幽灵卡片：1px 边框与 blur≥16px 阴影不共存）。只有 dropdown / drawer / modal 用 `--sh-md` / `--sh-lg`。
- 表格行高 36px（密集）。行内操作按钮 hover 才显形，但**必须 focus-visible 时也显形**，否则键盘用户永远够不到。

### 4.1 响应式

| 断点 | 行为 |
|---|---|
| ≥1600px | KPI 6 列；QPS 图与拒绝图并排双栏 |
| 1200-1599px | KPI 3 列；图表单栏堆叠；侧栏展开 216px |
| 900-1199px | KPI 2 列；侧栏折叠为 56px 图标条（图标 20px + `title` + `aria-label`） |
| <900px | 侧栏转抽屉（`i-menu` 触发）；**表格转卡片列表**（每行变一张 key-value 卡）；触摸目标提到 44px |

<900px 时表格必须转卡片，不做横向滚动。规则表有 8 列，手机横滚等于不可用。移动端的现实用途是「路上收到告警，先看清是哪个 biz 哪条规则」，卡片列表正好。

---

## 5. 五个页面规格

### 5.1 登录页 `#/login`

不是营销页，**不做 Hero**。单面板居中偏上（`margin-top: 12vh`，不垂直居中）。

```
        [i-shield 24px]
        unirate 管理控制台
        需要 Admin Bearer Token 才能访问

        Admin Token
        [i-key][ ················· ][i-eye]
        令牌由部署时的 UNIRATE_ADMIN_TOKEN 提供，仅存于本页会话

        [        进入控制台        ]
```

- 输入框 `type=password` + `i-eye`/`i-eye-off` 切换明文，`autocomplete="off"`，`spellcheck="false"`。
- 提交动作：以该 token 调 `GET /admin/snapshot` 试探。200 → 存 `sessionStorage.unirate_admin_token` 并跳 `#/monitor`。
- **状态**：
  - loading：按钮内 `i-spinner` 旋转 + 文案「验证中」+ 按钮 disabled（不做全屏遮罩）。
  - 401 → 字段下方错误条：「令牌无效。请核对部署配置中的 UNIRATE_ADMIN_TOKEN」。字段描边转 `--field-border-error`，焦点环转 `--sh-focus-danger`。
  - 403 → 「当前来源 IP 不在 Admin 白名单内（admin.allow_cidrs）」。这是 CIDR 拦截，和令牌错是两回事，必须分开说。
  - 503 → 「管理面已启动，但配置存储未就绪。令牌可能正确，稍后重试」。
  - 网络失败 → 「无法连接管理面（{addr}）。确认 admin 端口可达」+ 重试按钮。
- 底部 meta 行：`unirate` + 版本号 + 「文档」外链（`i-external`）。
- 安全提示放在页面底部而非隐藏：「Token 仅保存在当前标签页的 sessionStorage，关闭标签页即失效」。

会话失效处理：任何 API 返回 401 → 清 sessionStorage + 跳登录页 + 顶部提示「会话已失效，请重新输入令牌」。**不要静默跳转**，否则用户以为自己点错了。

### 5.2 实时监控看板 `#/monitor`

数据源 `GET /metrics`（Prometheus 文本），前端解析。轮询默认 5s，可切 5s/15s/60s/暂停（`i-pause`/`i-play`），状态存 localStorage。

**布局：三段**

1. **KPI 条**（6 个数字卡，1 行）

| 卡 | 指标来源 | 单位 | 阈值着色 |
|---|---|---|---|
| QPS | `unirate_requests_total` 一阶差分 ÷ 间隔 | req/s | 中性（无阈值） |
| 拒绝率 | `rejected_total` 差分 ÷ `requests_total` 差分 | % | ≥1% warn，≥5% danger |
| P99 延迟 | `request_duration_seconds_bucket` 直方图插值 | ms | ≥1000 warn，≥3000 danger |
| Token 消耗 | `tokens_settled_total` 差分 | tok/s | 中性 |
| 当前并发 | `concurrency_in_flight` 求和 | 个 | 中性 |
| 熔断器 | `redis_breaker_open` | 状态徽章 | 1 → failed |

每张卡结构：`caps 标签` / `28px mono 数值 + 12px 单位` / `sparkline（40px 高）+ 环比 delta`。
delta 用 `i-sort-asc`/`i-sort-desc` 表示方向 + 数值，**不用红绿判断好坏**——QPS 涨是好事，拒绝率涨是坏事，方向和好坏不是一回事。只有明确有阈值的指标（拒绝率、P99）才着色。

2. **主图区**（2 栏，<1600px 堆叠）
   - 左：**放行 vs 拒绝 QPS 折线**（双序列，`--c-chart-1` / `--c-chart-2`），60 个采样点滚动窗口。
   - 右：**延迟分布柱状**（直方图 bucket 直出，横轴 log 刻度标签）。

3. **明细区**（2 栏）
   - 左：**按 biz 的拒绝 Top 10 横向条形**。每行：biz 名（mono）+ 条 + 计数 + 命中规则名。**点击行直接跳 `#/rules?biz=xxx`** —— 这是整个控制台最关键的一条动线：从「拒绝率高」到「哪条规则拒的」必须一次点击到位。
   - 右：**规则水位表**（`rule_watermark_ratio_percent`）。列：biz / rule / 水位条 / 百分比。≥80% 行左侧出 warn 三角。默认按水位降序。

**状态**：
- loading（首帧）：KPI 卡骨架（`--c-surface-2` 矩形，透明度 1.2s 呼吸；reduced-motion 下静态），图表区显示网格线不显示数据。
- empty（`/metrics` 解析出 0 个样本）：`i-empty` 24px + 「网关尚未处理任何请求。指标会在首个请求到达后出现」。
- error（`/metrics` 不可达）：整页顶部错误条 + 保留上一帧数据并置灰，标注「数据停留在 14:32:07，最近 3 次拉取失败」。**不要清空画面**——故障中最后一帧数据往往是最有价值的。
- 降级（`degraded_decisions_total` 在增长）：topbar 健康胶囊转 degraded 青色 + 看板顶部横条「Redis 不可达，限流决策由本地兜底，配额可能不准确」。

### 5.3 业务域与限流规则 `#/rules`

**主视图：两级表格。** 外层是 biz 列表，展开行显示该 biz 的规则表。不用「列表页 + 详情页」跳转——SRE 常需要横向比较多个 biz 的规则，跳转会丢上下文。

外层列：状态点 / biz（mono）/ base_url / 规则数 / 剥离前缀 / Token 计量 / 更新时间 / 操作（`i-edit` `i-trash` hover 显形）。
展开后内层列：拖拽柄 `i-grip` / 规则名 / 类型徽章 / 计量 / 维度组合 chips / 窗口 / 限额 / 算法 / 启用开关。

工具栏：搜索（biz / 规则名 / base_url，`i-search`）、状态筛选（全部/启用/停用）、`+ 新增业务域`（主按钮，唯一强调色实体按钮）。

**规则表单（右侧抽屉，宽 520px，不用 modal）**

抽屉而非弹窗，因为编辑规则时需要对照左侧列表里其他规则的限额。

字段渐进披露，按类型分支：

```
规则名              [ text ]                       必填
类型                [ 速率 rate | 并发 concurrency ]  分段控件

  ── 类型 = rate 时 ──────────────────────────────
  计量维度          [ 请求数 request | Token 数 token ]
  时间窗口          [ 5m ]  预设 1s/1m/5m/1h/1d/1w + 自定义
  限额              [ 10000 ]  mono 输入，右侧显示换算「≈ 33.3 req/s」
  算法              [ 固定窗口 | 滑动窗口 | 令牌桶 ]
     令牌桶 + Token 计量 → 该组合直接标红禁用，helper 说明
     「令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定/滑动窗口」
  突发容量 burst    [ 0 ]  仅令牌桶时出现

  ── 类型 = concurrency 时 ───────────────────────
  最大并发          [ 50 ]
  持有超时（秒）     [ 120 ]  helper「留空取默认 120s」

限流维度（多选 chips）  [global] [biz] [ip] [token] [path] [method]
  选中 global 时其余 chip 立即禁用并置灰，helper
  「global 表示不分维度的全局限流，不能与具体维度组合」
水位告警阈值 %      [ 80 ]
启用               [ 开关 ]
```

**实时校验对接 `POST /admin/rules/validate`**：

- 触发：字段 blur + 值变化后 400ms debounce。发送当前整份规则数组（后端签名收 `[]*Rule`）。
- 200 `{valid:true}` → 抽屉底部状态条转 ok：`i-check` +「校验通过：{n} 条规则」。
- 400 `{problems:[{index,name,error}]}` → 按 `index` 把 `error` 映射回对应规则的字段级错误。后端返回的是英文技术文案，前端需要一张**错误映射表**把已知 error 前缀翻成中文（`"limit must be > 0"` → 「限额必须大于 0」），未命中的原文兜底显示，不吞。
- 校验未通过时保存按钮 disabled，但**不禁止用户继续编辑**（不做输入拦截）。
- 保存成功后回填 `config_version`，抽屉关闭并在表格对应行闪一次 `--c-accent-soft` 底（120ms，reduced-motion 下不闪）。
- 202 `saved_but_publish_failed` → **必须显式提示**：「已写入数据库，但配置发布失败。网关会在下次轮询（≤30s）拉取。如需立即生效，去配置快照页手动重载」+ 直达按钮。这是后端真实存在的分支，不能当成功处理。

**删除**：二次确认。`i-trash` → 确认弹窗要求**手动输入 biz 名**才能激活删除按钮（这是删除整个业务域的全部限流规则，误删代价是流量瞬间失控）。文案：「删除后 {biz} 的全部 {n} 条限流规则立即失效，该域流量将不再受限。输入 {biz} 确认」。

**状态**：
- loading：表格 6 行骨架。
- empty（无任何 biz）：`i-empty` +「尚未配置任何业务域。网关会以 `*` 兜底规则处理所有流量」+ `新增业务域` 按钮 + 一行提示「路径首段即业务域标识，例如 `POST /openai/v1/chat/completions` 命中 biz `openai`」。
- error 503（DB 不可达）：「配置数据库不可达，无法读写业务域。当前网关仍在用最后一份有效配置服务」+ 重试。这句后半段很重要——运维需要知道「管理面挂了 ≠ 网关挂了」。
- edge：规则数 >20 的 biz 展开时内层表分页；base_url 超长中间截断（`text-overflow` 不够，用 `path/…/tail` 保留尾部）；维度 chips 超 3 个折行不横滚。

### 5.4 审计日志 `#/audit`

数据源 `GET /admin/audit`（当前后端固定返回最近 100 条，倒序）。

工具栏：动作筛选（全部 / upsert_biz / delete_biz）、biz 筛选、操作者搜索、导出 CSV（`i-download`，前端拼串，不需要后端）。

表格列：时间（mono，绝对时间 + hover 显相对）/ 动作徽章 / biz（mono）/ 操作者（`i-user` + 名，`unknown` 用 `--c-meta` 斜体）/ 来源 IP（mono）/ detail（截断，点击展开）。

detail 是规则 JSON（后端截断在 4000 字符）。展开用行内展开区而非弹窗，`--c-surface-2` 底、mono、`white-space: pre-wrap`。**若 detail 恰好 4000 字符**，尾部追加 `--c-warn` 标注「detail 已被截断至 4000 字符」——否则用户会以为 JSON 坏了。

筛选是**前端筛选**（数据量固定 100 条）。工具栏必须明示这一点：「显示最近 100 条变更记录，筛选在本地进行」。不能让人误以为筛的是全量。

**状态**：loading 骨架 8 行 / empty「暂无配置变更记录。所有通过管理面的写操作都会记录在此」/ error 503 同上。

### 5.5 配置快照与热更新 `#/config`

数据源 `GET /admin/snapshot`，操作 `POST /admin/reload`。

**布局：左右两栏**

左栏 4 个状态字段（不做成 KPI 卡，做成定义列表，密度更高）：

| 字段 | 来源 | 渲染 |
|---|---|---|
| 配置版本 | `snapshot.version` | 20px mono + `i-copy` 复制 |
| 加载时间 | `snapshot.loaded_at` | mono 绝对时间 + 「距今 4 分 12 秒」 |
| 降级状态 | store.degraded | 五态徽章（degraded / healthy） |
| 业务域数量 | `len(snapshot.bizs)` | mono 数字 + 「其中 {n} 个已启用」 |

降级为 true 时，左栏顶部出 `--c-degraded-soft` 提示条：「配置中心不可达，当前使用最后一份有效配置（版本 {v}，加载于 {t}）。网关仍在正常限流，但新的配置变更不会生效」。

**手动重载按钮**：主按钮 `i-refresh` +「重新加载配置」。因为这是有副作用的全局操作，需要确认（普通确认弹窗，不需要输入确认）：「将从配置库重新读取全部业务域配置并发布到网关。当前版本 {v}」。
成功 → Toast「配置已重载：版本 {old} → {new}，{n} 个业务域」+ 左栏字段更新（版本号闪一次 accent 底）。若版本号未变，明确说「配置无变化，版本仍为 {v}」——不要让人以为没生效。
失败 → Toast danger + 保留原值 + 展开错误详情。

右栏：**当前生效配置的只读 JSON 视图**。`--c-surface-2` 底、mono、12px、行号、可折叠 biz 节点、顶部搜索框（高亮匹配）。这是排障时最实用的东西——「我改的配置到底有没有在内存里」只有这里能回答。

顶部另有一行 meta：「快照来自网关内存，不是数据库。如需对比数据库真值，重载后再看」。

**状态**：loading 骨架 / error 503「配置存储未就绪，网关可能仍在 bootstrap」/ reload 进行中按钮转 `i-spinner` + disabled + 「重载中」。

---

## 6. 无库手绘图表方案

全部纯 SVG，声明式渲染，无 Canvas（Canvas 拿不到 DOM 可访问性，且高分屏需手动处理 dpr）。三种图表足够覆盖全部需求。

### 6.1 折线 / 面积图（QPS 时序）

**核心技巧：viewBox 用数据坐标系，不做手动像素换算。**

```html
<svg class="chart-line" viewBox="0 0 600 160" preserveAspectRatio="none"
     role="img" aria-label="放行与拒绝 QPS 时序，最近 5 分钟">
  <!-- 网格：4 条横线，y 由 max 值等分 -->
  <g stroke="var(--c-chart-grid)" stroke-width="1">
    <line x1="0" y1="40"  x2="600" y2="40"/>
    <line x1="0" y1="80"  x2="600" y2="80"/>
    <line x1="0" y1="120" x2="600" y2="120"/>
    <line x1="0" y1="160" x2="600" y2="160"/>
  </g>
  <!-- 面积（放行） -->
  <path d="M0,150 L10,142 … L600,60 L600,160 L0,160 Z" fill="var(--c-chart-area)" stroke="none"/>
  <!-- 描线（放行） -->
  <path d="M0,150 L10,142 … L600,60" stroke="var(--c-chart-1)" stroke-width="1.5"/>
  <!-- 描线（拒绝） -->
  <path d="M0,158 L10,157 … L600,140" stroke="var(--c-chart-2)" stroke-width="1.5"/>
  <!-- hover 游标：JS 只改这一个 x -->
  <line class="cursor" x1="0" y1="0" x2="0" y2="160" stroke="var(--c-border-strong)" hidden/>
</svg>
```

三个必须注意的点：

1. `preserveAspectRatio="none"` 让 SVG 随容器宽度拉伸，但会**把描线也横向拉粗/拉细**。解法是 `vector-effect: non-scaling-stroke`（已写进 tokens.css 的 `.chart-line path`）。这是纯 SVG 折线图最常见的踩坑点。
2. 轴标签**不放在拉伸的 SVG 里**（会变形），用 HTML 绝对定位在 SVG 外层容器上。
3. 点生成：`x = i * (600 / (n-1))`，`y = 160 - (v / max) * 150`。max 取窗口内最大值向上取整到「好看的刻度」（1/2/5 × 10^n），否则纵轴数字会是 `7823.4` 这种读不了的值。

hover 交互：容器上一个 `mousemove`，按 `offsetX / clientWidth * n` 反查索引，移动游标线 + 显示 HTML tooltip。**不给每个数据点挂事件**（60 点 × 2 序列 = 120 个监听器，纯浪费）。

**Sparkline（KPI 卡内）**：同样结构去掉网格/面积/交互，`viewBox="0 0 120 32"`，只保留一条 `polyline` + 末点一个 `circle r=2`。

### 6.2 柱状图（延迟直方图 / biz 拒绝 Top N）

**纵向柱（延迟分布）**：13 个 bucket 直接对应 13 个 `<rect>`。`viewBox="0 0 600 160"`，`preserveAspectRatio` 默认（不拉伸，避免柱宽变形）。

```html
<svg viewBox="0 0 600 160" role="img" aria-label="请求延迟分布直方图">
  <rect x="4"  y="120" width="38" height="40"  rx="2" fill="var(--c-chart-1)"/>
  <rect x="48" y="40"  width="38" height="120" rx="2" fill="var(--c-chart-1)"/>
  <!-- … -->
</svg>
```
柱宽 `(600 - gap*(n+1)) / n`，`rx="2"` 与 `--r-sm` 对齐。P99 位置用一条 `--c-chart-4` 竖虚线标出（`stroke-dasharray="3 3"`）。

**横向条（biz 拒绝 Top 10）**：不用 SVG，用 HTML div + `width: {pct}%` + `background: var(--c-chart-2)`。理由：需要在条上叠中文 biz 名和数字，HTML 的文本排版和 `text-overflow` 比 SVG `<text>` 可靠得多，而且天然可点击、可 focus、可读屏。

**规则水位条**同理，用 HTML：底槽 `--c-surface-2`，填充按阈值切色（<80% accent / ≥80% warn / ≥100% danger），高 6px，`--r-pill`。

### 6.3 KPI 数字卡

纯 HTML，无 SVG（除内嵌 sparkline）。

```html
<article class="kpi" aria-labelledby="kpi-rej-l">
  <header>
    <span class="caps" id="kpi-rej-l">拒绝率</span>
    <svg class="icon" aria-hidden="true"><use href="#i-status-warn"/></svg>
  </header>
  <p class="kpi-value" data-state="warning">2.41<small>%</small></p>
  <footer>
    <svg class="spark" viewBox="0 0 120 32" aria-hidden="true">
      <polyline points="0,28 12,26 …" stroke="var(--c-chart-2)" stroke-width="1.5"/>
    </svg>
    <span class="delta num">
      <svg class="icon" aria-hidden="true"><use href="#i-sort-asc"/></svg>+0.8pt
    </span>
  </footer>
</article>
```

`data-state` 驱动数值颜色（`healthy` → `--c-fg`，`warning` → `--c-warn`，`failed` → `--c-danger`）。轮询刷新时**直接替换 textContent，不做 count-up 动画**——SRE 要读真值，过渡中的假数字是干扰。

### 6.4 Prometheus 文本解析要点（交给前端时一并说明）

`/metrics` 是 `text/plain; version=0.0.4`。解析需注意：

- 跳过 `#` 开头行；行格式 `name{k="v",...} value`（`unirate_uptime_seconds` 无标签）。
- Counter 是单调累计值，**必须做一阶差分再除以采样间隔**才是速率。首帧只能存基线不能出图（首帧 KPI 显示 `—` 而非 `0`，否则会误报「零流量」）。
- 直方图 P99：`request_duration_seconds_bucket{le="..."}` 累积桶，先差分再线性插值。桶边界已知（0.001…30 共 13 档）。
- 计数器重启（网关重启）会导致差分为负 → 判负则丢弃该次采样并标注「网关已重启，指标基线已重置」。这个分支必须处理，否则重启后 QPS 会出现巨大负值尖刺。

---

## 7. 组件五态矩阵

| 组件 | Loading | Empty | Error | Populated | Edge |
|---|---|---|---|---|---|
| KPI 卡 | 骨架矩形 | `—` + 「暂无样本」 | 保留上一帧置灰 + 「数据停留在 {t}」 | 数值 + spark + delta | 极大值缩写（12.4M）；负差分标注重启 |
| 折线图 | 网格无数据 | 「等待首个采样点」 | 上一帧置灰 + 顶部错误条 | 双序列 + 游标 | 单点时画横线；全 0 时 y 轴固定 0-1 |
| 数据表 | 6 行骨架 | `i-empty` + 引导 + 主操作 | 错误条 + 重试，保留旧数据 | 行 + hover 操作 | 长文本尾部截断；>20 行分页 |
| 规则表单 | 字段 disabled + spinner | — | 字段级错误 + 底部汇总条 | 校验通过绿条 | 令牌桶+Token 组合硬禁用 |
| 重载按钮 | spinner + disabled | — | Toast danger + 详情 | 默认 | 版本未变时明示「无变化」 |
| 健康胶囊 | 骨架胶囊 | — | 「状态未知」灰徽章 | 五态之一 | 多态并存时取最严重 |
| JSON 视图 | 骨架块 | 「快照为空」 | 「无法读取快照」 | 折叠树 + 搜索 | >500KB 时默认全折叠 |

---

## 8. Do / Don't

**Do**
- 所有数字走 mono + tabular-nums。
- 状态 = 颜色 + 形状 + 文字，三者齐全。
- 错误文案说清「是什么坏了 + 影响范围 + 下一步」，区分「管理面故障」与「网关故障」。
- 面板用 1px 边框，浮层才用阴影。
- 控件边界用 `--c-border-field`，分隔线用 `--c-border`。
- 表格行操作 hover 显形，但 focus-visible 也必须显形。
- 危险操作（删 biz）要求输入名称确认。
- 每屏强调色 ≤2 处：主操作按钮 + 导航选中标记。

**Don't**
- 不用 emoji 作图标；图标只从 `icon-sprite.svg` 取，stroke 恒 1.5。
- 不做 Hero、不做营销文案、不做居中大标题。
- 不用渐变作主视觉（尤其禁 Indigo→Pink）；`*-soft` 底色是纯色低透明，不是渐变。
- 不用弹跳缓动；界面过渡不超过 180ms。
- 不做数字 count-up 动画。
- 不在 1px 边框元素上叠 blur≥16px 阴影。
- 卡片圆角不超过 12px。
- 不用红绿表示 delta 方向（方向 ≠ 好坏）。
- 不复用琥珀色表达「降级」——降级有独立的青色。
- 错误时不清空画面。
- 移动端不做表格横滚，转卡片。

---

## 9. 前端实现要点

1. **文件结构**：单 HTML。`<head>` 内联 `<style>`（直接粘 `design-tokens.css` 全文 + 组件样式），`<body>` 首行粘 sprite，尾部内联 `<script>`。总目标 < 120KB 未压缩。
2. **主题初始化必须在 `<head>` 内联脚本里同步执行**，否则一帧白闪，且 tokens.css 不含系统偏好分支时会退化为 light：
   ```html
   <script>
     (function () {
       var t = null;
       try { t = localStorage.getItem('unirate_theme'); } catch (e) {}
       if (t !== 'light' && t !== 'dark') {
         t = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
       }
       document.documentElement.dataset.theme = t;
     })();
   </script>
   ```
   `try/catch` 是必需的——隐私模式下 `localStorage` 取值会抛异常，不能让主题脚本把整页 JS 打断。
3. **路由**：`hashchange` + 一个 `switch`。无框架。每个模块一个渲染函数 `render(root, params)`。
4. **鉴权**：统一 `fetch` 封装注入 `Authorization: Bearer ${token}`；401 拦截 → 清 session + 跳登录。写操作额外带 `X-Operator` 头（值取 localStorage 里用户自填的操作者名，登录页可选填，用于审计日志可读性）。
5. **轮询**：`document.hidden` 时暂停（`visibilitychange`），避免后台标签页持续打 `/metrics`。
6. **XSS**：所有后端数据（biz 名、base_url、audit detail、错误 message）一律 `textContent` 或手写 `escapeHtml`，禁止 `innerHTML` 拼接。审计 detail 是用户可控的 JSON，是最明显的注入面。
7. **键盘可达**：表格行 `tabindex="0"`，Enter 展开；抽屉 Esc 关闭 + 焦点陷阱 + 关闭后焦点归还触发元素；`Cmd/Ctrl+K` 聚焦搜索。
8. **图标可访问性**：装饰性 `aria-hidden="true"`；仅图标的按钮必须 `aria-label`。
9. **已知坑**：`preserveAspectRatio="none"` 的 SVG 必须配 `vector-effect: non-scaling-stroke`；Counter 差分必须处理重启负值；`/metrics` 首帧只存基线不出图。

---

## 变更记录

| 日期 | 变更 | 原因 |
|---|---|---|
| 初版 | 建立 tokens / sprite / 5 页规格 / 图表方案 | Phase 1 设计调研交付 |
