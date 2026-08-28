/* ============================================================================
   监控看板 #/monitor —— DESIGN.md §5.2
   ----------------------------------------------------------------------------
   四段：KPI 条 / 主图区（放行·拒绝双序列 + 延迟直方图）/ 明细区（biz 拒绝 Top10 + 规则水位）
   / 流量区（biz 的 RPM·TPM Top10）
   数据层走 Metrics.fetchMetrics()（可插拔，当前端点待接入）。
   错误时不清空画面 —— 故障中最后一帧数据往往最有价值。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, C = w.Charts, M = w.Metrics;

  var store = M.createStore();
  var failStreak = 0;
  var lastOkAt = null;
  var lastError = null;
  var timer = null;
  var INTERVALS = [5, 15, 60];

  function interval() {
    var v = null;
    try { v = localStorage.getItem('unirate_poll'); } catch (e) {}
    var n = parseInt(v, 10);
    return INTERVALS.indexOf(n) >= 0 || n === 0 ? n : 5;
  }
  function setInterval_(n) {
    try { localStorage.setItem('unirate_poll', String(n)); } catch (e) {}
  }

  function mainCharts() {
    var has = store.series.pass.length > 0;
    var line = U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' }, [
        U.el('span', { class: 'panel-title', text: '放行 vs 拒绝 QPS' }),
        U.el('div', { class: 'chart-legend' }, [
          U.el('span', null, [U.el('i', { style: 'background: var(--c-chart-1)' }), '放行']),
          U.el('span', null, [U.el('i', { style: 'background: var(--c-chart-2)' }), '拒绝'])
        ])
      ]),
      U.el('div', { class: 'panel-body' }, has ? C.lineChart({
        ariaLabel: '放行与拒绝 QPS 时序，最近 ' + store.series.pass.length + ' 个采样点',
        labels: store.series.labels,
        series: [
          { values: store.series.pass, color: 'var(--c-chart-1)', label: '放行', unit: ' req/s', area: true },
          { values: store.series.reject, color: 'var(--c-chart-2)', label: '拒绝', unit: ' req/s' }
        ]
      }) : C.lineChart({ series: [{ values: [], color: 'var(--c-chart-1)', label: '放行' }] }))
    ]);
    if (!has) {
      line.lastChild.appendChild(U.el('p', { class: 'helper', text: '等待首个采样点' }));
    }

    var buckets = store.buckets;
    var total = buckets ? buckets.reduce(function (a, b) { return a + b; }, 0) : 0;
    var cum = 0;
    var cumArr = (buckets || []).map(function (v) { cum += v; return cum; });
    var p99Idx = -1;
    if (total > 0) {
      for (var i = 0; i < cumArr.length; i++) {
        if (cumArr[i] >= total * 0.99) { p99Idx = i; break; }
      }
    }
    var bars = U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' }, [
        U.el('span', { class: 'panel-title', text: '请求延迟分布' }),
        U.el('span', { class: 'spacer' }),
        U.el('span', { class: 'helper', text: total > 0 ? '窗口内 ' + total + ' 次请求' : '' })
      ]),
      U.el('div', { class: 'panel-body' }, total > 0 ? C.barChart({
        ariaLabel: '请求延迟分布直方图，横轴为延迟上界（秒）',
        markIndex: p99Idx,
        bars: M.BUCKETS.map(function (b, i) {
          return { label: b < 1 ? (b * 1000) + 'ms' : b + 's', value: buckets[i] };
        })
      }) : U.emptyBlock([U.el('span', { text: '窗口内还没有完成的请求，直方图会在首批请求结束后出现' })]))
    ]);
    return U.el('div', { class: 'grid-2' }, [line, bars]);
  }

  function detailPanels() {
    var raw = store.raw;
    var top = U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' }, [
        U.el('span', { class: 'panel-title', text: '按业务域的拒绝 Top 10' }),
        U.el('span', { class: 'spacer' }),
        U.el('span', { class: 'helper', text: '点击跳该域规则' })
      ])
    ]);
    var list = U.el('div', { class: 'bars' });
    var entries = [];
    if (raw) {
      Object.keys(raw.rejectByBiz).forEach(function (b) {
        entries.push({ biz: b, count: raw.rejectByBiz[b], rule: (raw.rejectRuleByBiz[b] || {}).rule || '—' });
      });
      entries.sort(function (a, b) { return b.count - a.count; });
      entries = entries.slice(0, 10);
    }
    if (entries.length === 0) {
      top.appendChild(U.emptyBlock([U.el('span', { text: '窗口内没有任何请求被规则拒绝' })]));
    } else {
      var peak = entries[0].count || 1;
      entries.forEach(function (e) {
        // 从「拒绝率高」到「哪条规则拒的」必须一次点击到位（本控制台最关键动线）
        list.appendChild(U.el('button', {
          class: 'bar-row', type: 'button',
          'aria-label': '查看业务域 ' + e.biz + ' 的限流规则',
          onclick: function () { location.hash = '#/rules?biz=' + encodeURIComponent(e.biz); }
        }, [
          U.el('span', { class: 'bar-name', text: e.biz, title: e.biz }),
          C.hBar((e.count / peak) * 100),
          U.el('span', { class: 'bar-num', text: U.compact(e.count) }),
          U.el('span', { class: 'bar-rule', text: '命中规则 ' + e.rule })
        ]));
      });
      top.appendChild(list);
    }

    var marks = raw ? raw.watermarks.slice().sort(function (a, b) { return b.pct - a.pct; }) : [];
    var wm = U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' },
        U.el('span', { class: 'panel-title', text: '规则水位' }))
    ]);
    if (marks.length === 0) {
      wm.appendChild(U.emptyBlock([U.el('span', { text: '暂无规则水位样本。水位在规则首次命中后上报' })]));
    } else {
      var body = U.el('div', { class: 'bars' });
      marks.forEach(function (m) {
        var st = m.pct >= 100 ? 'failed' : m.pct >= 80 ? 'warning' : 'healthy';
        body.appendChild(U.el('div', { class: 'bar-row', style: 'cursor:default' }, [
          U.el('span', { class: 'bar-name' }, [
            st === 'warning' || st === 'failed' ? U.icon(U.STATUS[st].icon, { label: U.STATUS[st].label }) : null,
            U.el('span', { text: m.biz, title: m.biz })
          ]),
          C.hBar(m.pct, st),
          U.el('span', { class: 'bar-num', text: U.num(m.pct, 1) + '%' }),
          U.el('span', { class: 'bar-rule', text: '规则 ' + m.rule })
        ]));
      });
      wm.appendChild(body);
    }
    return U.el('div', { class: 'grid-2' }, [top, wm]);
  }

  /* 按业务域的 RPM/TPM。RPM 与 TPM 量级差两三个数量级，同轴无法比较，
     故按 RPM 排序、条形只表达 RPM 占比，TPM 以数值并列展示。 */
  function trafficPanel() {
    var k = store.kpi;
    var panel = U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' }, [
        U.el('span', { class: 'panel-title', text: '按业务域的流量 Top 10' }),
        U.el('span', { class: 'spacer' }),
        U.el('span', { class: 'helper', text: '后端滚动窗口值，非采样差分' })
      ])
    ]);
    var rows = [];
    if (k && k.rpmByBiz) {
      Object.keys(k.rpmByBiz).forEach(function (b) {
        rows.push({ biz: b, rpm: k.rpmByBiz[b] || 0, tpm: (k.tpmByBiz || {})[b] || 0 });
      });
      rows.sort(function (a, b) { return b.rpm - a.rpm; });
      rows = rows.slice(0, 10);
    }
    if (rows.length === 0) {
      panel.appendChild(U.emptyBlock([U.el('span', { text: '窗口内暂无业务域流量样本' })]));
      return panel;
    }
    var peak = rows[0].rpm || 1;
    var body = U.el('div', { class: 'bars' });
    rows.forEach(function (e) {
      body.appendChild(U.el('button', {
        class: 'bar-row', type: 'button',
        'aria-label': '查看业务域 ' + e.biz + ' 的限流规则',
        onclick: function () { location.hash = '#/rules?biz=' + encodeURIComponent(e.biz); }
      }, [
        U.el('span', { class: 'bar-name', text: e.biz, title: e.biz }),
        C.hBar((e.rpm / peak) * 100),
        U.el('span', { class: 'bar-num', text: U.compact(e.rpm) + ' req/min' }),
        U.el('span', { class: 'bar-rule', text: U.compact(e.tpm) + ' tok/min' })
      ]));
    });
    panel.appendChild(body);
    return panel;
  }

  function banner() {
    var out = [];
    if (!M.endpointReady()) {
      out.push(U.el('div', { class: 'note note--warn' }, [
        U.icon('i-info'),
        U.el('div', null, [
          U.el('div', { text: '指标端点待接入：admin 端口尚未提供 ' + M.endpointPath() + '，KPI 与图表暂无数据。' }),
          U.el('div', {
            class: 'helper',
            text: '解析、差分、直方图插值与渲染均已就位，后端补完该端点即可出图。' +
              'obs 端口（29091）无鉴权且全网暴露，跨端口取数已否决。'
          })
        ])
      ]));
    }
    if (store.restarted) {
      out.push(U.el('div', { class: 'note note--warn' }, [
        U.icon('i-status-warn'),
        U.el('span', { text: '网关已重启，指标基线已重置。本次采样已丢弃，速率将在下一次采样后恢复。' })
      ]));
    }
    if (store.kpi && store.kpi.degradedGrowing) {
      out.push(U.el('div', { class: 'note note--degraded' }, [
        U.icon('i-status-degraded'),
        U.el('span', { text: 'Redis 不可达，限流决策由本地兜底，配额可能不准确。' })
      ]));
    }
    if (failStreak > 0 && M.endpointReady()) {
      out.push(U.el('div', { class: 'note note--danger' }, [
        U.icon('i-status-fail'),
        U.el('div', null, [
          U.el('div', {
            text: lastOkAt
              ? '数据停留在 ' + U.absTime(lastOkAt) + '，最近 ' + failStreak + ' 次拉取失败。'
              : '指标拉取失败 ' + failStreak + ' 次，尚未取到任何数据。'
          }),
          lastError ? U.el('div', { class: 'helper', text: lastError }) : null
        ])
      ]));
    }
    return out;
  }

  function toolbar() {
    var cur = interval();
    var seg = U.el('div', { class: 'seg', role: 'group', 'aria-label': '自动刷新间隔' });
    INTERVALS.forEach(function (n) {
      seg.appendChild(U.el('button', {
        type: 'button', text: n + 's', 'aria-pressed': cur === n ? 'true' : 'false',
        onclick: function () { setInterval_(n); schedule(); render(); }
      }));
    });
    return U.el('div', { class: 'toolbar' }, [
      U.el('span', { class: 'caps', text: '自动刷新' }),
      seg,
      U.el('button', {
        class: 'btn', type: 'button',
        'aria-pressed': cur === 0 ? 'true' : 'false',
        onclick: function () { setInterval_(cur === 0 ? 5 : 0); schedule(); render(); }
      }, [U.icon(cur === 0 ? 'i-play' : 'i-pause'), U.el('span', { text: cur === 0 ? '继续' : '暂停' })]),
      U.el('span', { class: 'spacer' }),
      U.el('span', {
        class: 'helper',
        text: lastOkAt ? '上次更新 ' + U.clockTime(lastOkAt) : '尚未取到采样'
      }),
      U.el('button', {
        class: 'btn', type: 'button', onclick: function () { poll(); }
      }, [U.icon('i-refresh'), U.el('span', { text: '立即刷新' })])
    ]);
  }

  var root = null;

  function render() {
    if (!root) return;
    U.clear(root);
    U.append(root, U.el('div', { class: 'page' }, [
      U.el('div', { class: 'page-head' }, [
        U.el('h1', { class: 'page-title', text: '监控看板' }),
        U.el('span', { class: 'page-sub', text: '网关运行指标，' + (interval() === 0 ? '自动刷新已暂停' : '每 ' + interval() + ' 秒刷新') })
      ]),
      toolbar(),
      U.el('div', { class: 'stack' }, banner().concat([
        w.MonitorKPI.render(store, failStreak), mainCharts(), detailPanels(), trafficPanel()
      ]))
    ]));
  }

  function poll() {
    return M.fetchMetrics().then(function (text) {
      M.ingest(store, text);
      failStreak = 0;
      lastError = null;
      lastOkAt = new Date();
      if (store.kpi) {
        w.App.setHealth(store.kpi.breaker >= 1 ? 'failed'
          : store.kpi.degradedGrowing ? 'degraded' : 'healthy', store.kpi.version);
      }
      render();
    }, function (err) {
      // 错误时保留上一帧并置灰，不清空画面
      failStreak++;
      lastError = err && err.message ? err.message : null;
      render();
    });
  }

  function schedule() {
    if (timer) { w.clearInterval(timer); timer = null; }
    var n = interval();
    if (n > 0) timer = w.setInterval(function () {
      // document.hidden 时不打指标端点，避免后台标签页持续轮询
      if (!document.hidden) poll();
    }, n * 1000);
  }

  w.PageMonitor = {
    mount: function (outlet) {
      root = outlet;
      render();
      poll();
      schedule();
    },
    unmount: function () {
      if (timer) { w.clearInterval(timer); timer = null; }
      root = null;
    }
  };
})(window);
