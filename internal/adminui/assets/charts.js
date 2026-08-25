/* ============================================================================
   纯 SVG 手绘图表（无 Canvas、无图表库）—— DESIGN.md §6
   ----------------------------------------------------------------------------
   两个必踩的坑已在此处理：
   1. preserveAspectRatio="none" 会把描线横向拉粗/拉细 → 靠 CSS 的
      vector-effect: non-scaling-stroke（见 base.css .chart-line path）。
   2. 轴标签不放进被拉伸的 SVG（会变形），用 HTML 绝对定位在外层容器。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U;
  var VW = 600, VH = 160;

  /* niceMax：把纵轴上限取到 1/2/5 × 10^n，否则刻度会是 7823.4 这种读不了的值 */
  function niceMax(v) {
    if (!isFinite(v) || v <= 0) return 1;
    var exp = Math.floor(Math.log(v) / Math.LN10);
    var pow = Math.pow(10, exp);
    var f = v / pow;
    var step = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return step * pow;
  }

  function points(series, max) {
    var n = series.length;
    if (n === 0) return [];
    var stepX = n === 1 ? 0 : VW / (n - 1);
    return series.map(function (v, i) {
      var y = VH - (Math.max(0, v) / max) * (VH - 10);
      return { x: n === 1 ? VW / 2 : i * stepX, y: Math.max(0, Math.min(VH, y)) };
    });
  }

  function pathOf(pts) {
    if (pts.length === 0) return '';
    if (pts.length === 1) {
      // 单点时画横线，否则 SVG path 只有一个 M 什么都看不到（五态矩阵 Edge 列）
      return 'M0,' + pts[0].y.toFixed(1) + ' L' + VW + ',' + pts[0].y.toFixed(1);
    }
    return pts.map(function (p, i) {
      return (i === 0 ? 'M' : 'L') + p.x.toFixed(1) + ',' + p.y.toFixed(1);
    }).join(' ');
  }

  /* lineChart(opts)
     opts.series [{ values:[], color:'var(--c-chart-1)', label:'放行', area:bool }]
     opts.labels 采样时刻文本，用于 hover tooltip 与 x 轴 */
  function lineChart(opts) {
    var series = (opts.series || []).filter(function (s) { return s && s.values; });
    var all = [];
    series.forEach(function (s) { all = all.concat(s.values); });
    var peak = all.length ? Math.max.apply(null, all) : 0;
    // 全 0 时 y 轴固定 0-1，否则除零得 NaN，整条线消失
    var max = niceMax(peak > 0 ? peak : 1);

    var grid = U.svg('g', { stroke: 'var(--c-chart-grid)', 'stroke-width': '1' },
      [0.25, 0.5, 0.75, 1].map(function (f) {
        var y = (VH * f).toFixed(1);
        return U.svg('line', { x1: 0, y1: y, x2: VW, y2: y });
      }));

    var kids = [grid];
    series.forEach(function (s) {
      var pts = points(s.values, max);
      if (pts.length === 0) return;
      if (s.area && pts.length > 1) {
        kids.push(U.svg('path', {
          d: pathOf(pts) + ' L' + VW + ',' + VH + ' L0,' + VH + ' Z',
          fill: s.areaColor || 'var(--c-chart-area)', stroke: 'none'
        }));
      }
      kids.push(U.svg('path', { d: pathOf(pts), stroke: s.color, 'stroke-width': '1.5', fill: 'none' }));
    });

    var cursor = U.svg('line', {
      class: 'cursor', x1: 0, y1: 0, x2: 0, y2: VH,
      stroke: 'var(--c-border-strong)', 'stroke-width': '1'
    });
    cursor.setAttribute('hidden', '');
    kids.push(cursor);

    var node = U.svg('svg', {
      class: 'chart-line', viewBox: '0 0 ' + VW + ' ' + VH,
      preserveAspectRatio: 'none', role: 'img',
      'aria-label': opts.ariaLabel || '时序折线图'
    }, kids);

    var box = U.el('div', { class: 'chart-box chart-pad' });
    // y 轴刻度用 HTML，不进被拉伸的 SVG
    box.appendChild(U.el('div', { class: 'chart-axis-y', style: 'left:0' }, [
      U.el('span', { text: U.compact(max) }),
      U.el('span', { text: U.compact(max * 0.5) }),
      U.el('span', { text: '0' })
    ]));
    box.appendChild(node);

    var labels = opts.labels || [];
    box.appendChild(U.el('div', { class: 'chart-axis-x' }, [
      U.el('span', { text: labels.length ? labels[0] : '' }),
      U.el('span', { text: labels.length ? labels[labels.length - 1] : '' })
    ]));

    // 容器上一个 mousemove 反查索引；不给每个点挂监听器（60 点 × 2 序列 = 120 个，纯浪费）
    var tip = U.el('div', { class: 'chart-tip' });
    tip.hidden = true;
    box.appendChild(tip);
    var n = series.length ? series[0].values.length : 0;
    if (n > 0) {
      box.addEventListener('mousemove', function (ev) {
        var rect = node.getBoundingClientRect();
        if (rect.width === 0) return;
        var ratio = (ev.clientX - rect.left) / rect.width;
        var i = Math.max(0, Math.min(n - 1, Math.round(ratio * (n - 1))));
        cursor.removeAttribute('hidden');
        var vx = (n === 1 ? VW / 2 : i * (VW / (n - 1))).toFixed(1);
        cursor.setAttribute('x1', vx);
        cursor.setAttribute('x2', vx);
        var lines = [labels[i] || ''];
        series.forEach(function (s) {
          lines.push(s.label + ' ' + U.compact(s.values[i]) + (s.unit || ''));
        });
        tip.textContent = lines.join(' · ');
        tip.hidden = false;
        var left = Math.min(rect.width - 10, Math.max(0, ratio * rect.width + 12));
        tip.setAttribute('style', 'left:' + left + 'px; top:4px');
      });
      box.addEventListener('mouseleave', function () {
        cursor.setAttribute('hidden', '');
        tip.hidden = true;
      });
    }
    return box;
  }

  /* barChart —— 延迟直方图。preserveAspectRatio 保持默认，避免柱宽变形。
     opts.bars [{label, value}]，opts.markIndex 处画 P99 竖虚线 */
  function barChart(opts) {
    var bars = opts.bars || [];
    var n = bars.length || 1;
    var gap = 4;
    var bw = (VW - gap * (n + 1)) / n;
    var peak = bars.reduce(function (m, b) { return Math.max(m, b.value || 0); }, 0);
    var max = niceMax(peak > 0 ? peak : 1);

    var kids = bars.map(function (b, i) {
      var h = Math.max(b.value > 0 ? 1 : 0, (Math.max(0, b.value || 0) / max) * (VH - 8));
      return U.svg('rect', {
        x: (gap + i * (bw + gap)).toFixed(1), y: (VH - h).toFixed(1),
        width: bw.toFixed(1), height: h.toFixed(1), rx: 2,
        fill: 'var(--c-chart-1)'
      });
    });
    if (opts.markIndex !== undefined && opts.markIndex !== null && opts.markIndex >= 0) {
      var mx = (gap + opts.markIndex * (bw + gap) + bw / 2).toFixed(1);
      kids.push(U.svg('line', {
        x1: mx, y1: 0, x2: mx, y2: VH,
        stroke: 'var(--c-chart-4)', 'stroke-width': '1', 'stroke-dasharray': '3 3'
      }));
    }

    var box = U.el('div', { class: 'chart-box chart-pad' });
    box.appendChild(U.el('div', { class: 'chart-axis-y', style: 'left:0' }, [
      U.el('span', { text: U.compact(max) }),
      U.el('span', { text: U.compact(max * 0.5) }),
      U.el('span', { text: '0' })
    ]));
    box.appendChild(U.svg('svg', {
      class: 'chart-bars', viewBox: '0 0 ' + VW + ' ' + VH,
      role: 'img', 'aria-label': opts.ariaLabel || '分布直方图'
    }, kids));
    box.appendChild(U.el('div', { class: 'chart-axis-x' },
      bars.map(function (b, i) {
        // 桶多时只标首/中/末，否则标签会挤成一团
        var show = n <= 7 || i === 0 || i === n - 1 || i === Math.floor(n / 2);
        return U.el('span', { text: show ? b.label : '' });
      })));
    return box;
  }

  /* sparkline —— KPI 卡内嵌，去掉网格/面积/交互，只留 polyline + 末点 circle */
  function sparkline(values, color) {
    var vals = (values || []).filter(function (v) { return isFinite(v); });
    var wv = 120, hv = 32;
    if (vals.length === 0) {
      return U.svg('svg', { class: 'spark', viewBox: '0 0 ' + wv + ' ' + hv, 'aria-hidden': 'true' }, []);
    }
    var max = niceMax(Math.max.apply(null, vals.concat([0])) || 1);
    var stepX = vals.length === 1 ? 0 : wv / (vals.length - 1);
    var pts = vals.map(function (v, i) {
      var y = hv - 2 - (Math.max(0, v) / max) * (hv - 6);
      return (vals.length === 1 ? wv / 2 : i * stepX).toFixed(1) + ',' + y.toFixed(1);
    });
    var last = pts[pts.length - 1].split(',');
    return U.svg('svg', {
      class: 'spark', viewBox: '0 0 ' + wv + ' ' + hv, preserveAspectRatio: 'none', 'aria-hidden': 'true'
    }, [
      U.svg('polyline', { points: pts.join(' '), stroke: color, 'stroke-width': '1.5', fill: 'none' }),
      U.svg('circle', { cx: last[0], cy: last[1], r: 2, fill: color, stroke: 'none' })
    ]);
  }

  /* hBar —— 横向条用 HTML div：条上要叠中文与数字，
     HTML 的 text-overflow 比 SVG <text> 可靠，且天然可点击/可 focus/可读屏 */
  function hBar(pct, state) {
    var p = Math.max(0, Math.min(100, isFinite(pct) ? pct : 0));
    return U.el('div', { class: 'bar-track' },
      U.el('div', {
        class: 'bar-fill', dataset: state ? { state: state } : null,
        style: 'width:' + p.toFixed(1) + '%'
      }));
  }

  w.Charts = {
    lineChart: lineChart, barChart: barChart, sparkline: sparkline,
    hBar: hBar, niceMax: niceMax
  };
})(window);
