/* ============================================================================
   监控看板的 KPI 条 —— DESIGN.md §5.2 / §6.3
   ----------------------------------------------------------------------------
   两条必须守住的语义：

   1. 首帧无速率时显示「—」而不是 0。counter 的绝对值不是速率，首帧只有基线，
      显示 0 会被读成「零流量」—— 在排障时这是完全错误的结论。

   2. delta 只表方向不表好坏，不用红绿。QPS 涨是好事、拒绝率涨是坏事，
      方向和好坏不是一回事。只有明确有阈值的指标（拒绝率、P99）才着色。

   刷新时直接替换 textContent，不做 count-up 动画 —— SRE 要读真值，
   过渡中的假数字是干扰。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, C = w.Charts;

  function card(cfg) {
    var val = cfg.value;
    var text = val === null || val === undefined ? '—' : val;
    return U.el('article', { class: 'kpi', dataset: cfg.stale ? { stale: '1' } : null }, [
      U.el('header', null, [
        U.el('span', { class: 'caps', text: cfg.label }),
        U.icon(cfg.icon)
      ]),
      U.el('p', {
        class: 'kpi-value', dataset: cfg.state ? { state: cfg.state } : null
      }, [text, cfg.unit && text !== '—' ? U.el('small', { text: cfg.unit }) : null]),
      // note 用于标注该数值的真实分辨率（目前只有 P99 用）。
      // 放在 value 与 footer 之间而不是 tooltip 里：精度限制属于数值本身的
      // 一部分，藏进悬浮提示等于默认没人看。
      cfg.note ? U.el('p', {
        class: 'helper', style: 'margin: 0; font-size: var(--fs-micro)', text: cfg.note
      }) : null,
      U.el('footer', null, [
        cfg.spark ? C.sparkline(cfg.spark, cfg.sparkColor || 'var(--c-chart-1)') : U.el('span'),
        cfg.delta ? U.el('span', { class: 'delta' }, [
          U.icon(cfg.delta.dir === 'up' ? 'i-sort-asc' : 'i-sort-desc'),
          U.el('span', { text: cfg.delta.text })
        ]) : null
      ])
    ]);
  }

  /* p99Text / p99Note —— 直方图分位数的诚实显示。
     
     分辨率上限由 bucket 边界决定：桶区间宽度占其上界的 50%-100%
     （例如 (1s, 2.5s] 这一档宽 1500ms）。线性插值出的 "2479.0 ms" 看起来像
     0.1ms 精度，实际不确定性达 ±1500ms —— 差四个数量级。SRE 会拿这个数字
     对 SLO，给假精度比给区间更危险。
     
     所以：有效数字按所在桶的宽度给（宽桶只给整数位），并在卡片上标出区间。 */
  function p99Text(k) {
    if (k.p99ms === null || k.p99ms === undefined) return null;
    var r = k.p99Range;
    if (!r) return U.num(k.p99ms, 1);
    if (r.exact) return U.num(k.p99ms, r.hi < 10 ? 1 : 0);
    if (!isFinite(r.hi)) return '>' + U.num(r.lo, 0);
    // 桶越宽，小数位越无意义
    var width = r.hi - r.lo;
    return U.num(k.p99ms, width >= 100 ? 0 : width >= 10 ? 0 : 1);
  }

  function p99Note(k) {
    var r = k.p99Range;
    if (!r || k.p99ms === null) return null;
    if (r.exact) return null;
    if (!isFinite(r.hi)) return '超出最大桶 ' + U.num(r.lo, 0) + 'ms';
    // 明示真实分辨率：这个值只能确定在哪个桶内
    return '桶区间 ' + U.num(r.lo, 0) + '–' + U.num(r.hi, 0) + 'ms';
  }

  function deltaOf(cur, prev, unit, digits) {
    if (cur === null || prev === null || cur === undefined || prev === undefined) return null;
    var d = cur - prev;
    // 抖动阈值：完全相等时不显示 delta，否则每帧都挂一个 +0.0 噪声
    if (Math.abs(d) < 1e-9) return null;
    return {
      dir: d > 0 ? 'up' : 'down',
      text: (d > 0 ? '+' : '') + U.num(d, digits === undefined ? 1 : digits) + (unit || '')
    };
  }

  function skeleton() {
    var grid = U.el('div', { class: 'grid-kpi' });
    for (var i = 0; i < 6; i++) {
      grid.appendChild(U.el('article', { class: 'kpi' }, [
        U.el('span', { class: 'skel', style: 'width:38%' }),
        U.el('span', { class: 'skel', style: 'width:60%; height:28px' }),
        U.el('span', { class: 'skel', style: 'width:100%' })
      ]));
    }
    return grid;
  }

  function breakerCard(k, stale) {
    var st = k.breaker >= 1 ? 'failed' : 'healthy';
    return U.el('article', { class: 'kpi', dataset: stale ? { stale: '1' } : null }, [
      U.el('header', null, [
        U.el('span', { class: 'caps', text: '熔断器' }),
        U.icon('i-breaker')
      ]),
      U.el('div', { style: 'padding: var(--sp-2) 0' },
        U.statusBadge(st, k.breaker >= 1 ? 'Redis 熔断开启' : 'Redis 正常')),
      U.el('footer', null, U.el('span', {
        class: 'helper',
        text: k.breaker >= 1 ? '限流决策已由本地兜底' : '决策走 Redis 集中计数'
      }))
    ]);
  }

  /* render(store, failStreak) —— store 为 Metrics 状态机 */
  function render(store, failStreak) {
    var k = store.kpi;
    if (!k) return skeleton();

    var grid = U.el('div', { class: 'grid-kpi' });
    var stale = failStreak > 0;
    // 阈值来自 DESIGN.md §5.2：拒绝率 ≥1% warn / ≥5% danger，P99 ≥1s warn / ≥3s danger
    var rejState = k.rejectRate >= 5 ? 'failed' : k.rejectRate >= 1 ? 'warning' : null;
    var p99State = k.p99ms === null ? null
      : k.p99ms >= 3000 ? 'failed' : k.p99ms >= 1000 ? 'warning' : null;
    var prev = k.prev;

    U.append(grid, [
      card({
        label: 'QPS', icon: 'i-nav-monitor', value: U.compact(k.qps), unit: ' req/s',
        spark: store.series.pass, stale: stale,
        delta: prev ? deltaOf(k.qps, prev.qps, '', 1) : null
      }),
      card({
        label: '拒绝率', icon: rejState ? U.STATUS[rejState].icon : 'i-status-ok',
        value: U.num(k.rejectRate, 2), unit: '%', state: rejState, stale: stale,
        spark: store.series.reject, sparkColor: 'var(--c-chart-2)',
        delta: prev ? deltaOf(k.rejectRate, prev.rejectRate, 'pt', 2) : null
      }),
      card({
        label: 'P99 延迟', icon: 'i-latency',
        // 有效数字按桶分辨率给，不给假精度（见 p99Text 说明）
        value: p99Text(k), unit: k.p99ms === null ? '' : ' ms',
        state: p99State, stale: stale,
        // delta 同样降精度：桶宽 50%-100% 时，"+9.0ms" 的变化量毫无意义
        delta: prev && prev.p99ms !== null && k.p99ms !== null
          ? deltaOf(k.p99ms, prev.p99ms, 'ms', 0) : null,
        note: p99Note(k)
      }),
      card({
        label: 'Token 消耗', icon: 'i-token',
        value: U.compact(k.tokensPerSec), unit: ' tok/s', stale: stale
      }),
      card({
        label: '当前并发', icon: 'i-rule-concurrency',
        value: U.compact(k.inFlight), unit: ' 个', stale: stale
      }),
      breakerCard(k, stale)
    ]);
    return grid;
  }

  w.MonitorKPI = { render: render };
})(window);
