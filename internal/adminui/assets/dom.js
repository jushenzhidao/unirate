/* ============================================================================
   DOM 构造工具 + 状态查表 + 格式化
   ----------------------------------------------------------------------------
   安全契约：本文件是唯一允许创建节点的地方，且只用 createElement / textContent。
   全局禁止 innerHTML 拼接后端数据 —— audit.detail 是用户可控 JSON，
   biz 名与 base_url 同样可控，是最明显的注入面（DESIGN.md §9.6）。
   ========================================================================== */
(function (w) {
  'use strict';

  /* el(tag, props, children) —— 唯一建节点入口。
     文本一律走 textContent；只有 SVG 的 <use href> 例外（无用户数据参与）。 */
  function el(tag, props, children) {
    var n = document.createElement(tag);
    if (props) {
      Object.keys(props).forEach(function (k) {
        var v = props[k];
        if (v === null || v === undefined || v === false) return;
        if (k === 'class') n.className = v;
        else if (k === 'text') n.textContent = String(v);
        else if (k === 'html') throw new Error('innerHTML is forbidden: use text');
        else if (k === 'dataset') Object.keys(v).forEach(function (d) { n.dataset[d] = v[d]; });
        else if (k === 'style') n.setAttribute('style', v);
        else if (k.slice(0, 2) === 'on') n.addEventListener(k.slice(2).toLowerCase(), v);
        else if (v === true) n.setAttribute(k, '');
        else n.setAttribute(k, String(v));
      });
    }
    append(n, children);
    return n;
  }

  function append(parent, children) {
    if (children === null || children === undefined || children === false) return parent;
    if (Array.isArray(children)) {
      children.forEach(function (c) { append(parent, c); });
      return parent;
    }
    if (children instanceof Node) parent.appendChild(children);
    else parent.appendChild(document.createTextNode(String(children)));
    return parent;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
    return node;
  }

  var SVG_NS = 'http://www.w3.org/2000/svg';

  /* svg(tag, attrs, children) —— SVG 元素必须用 createElementNS，
     否则浏览器按 HTML 命名空间解析，图形完全不渲染。 */
  function svg(tag, attrs, children) {
    var n = document.createElementNS(SVG_NS, tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        var v = attrs[k];
        if (v === null || v === undefined || v === false) return;
        if (k === 'text') { n.textContent = String(v); return; }
        if (k.slice(0, 2) === 'on') { n.addEventListener(k.slice(2).toLowerCase(), v); return; }
        n.setAttribute(k, String(v));
      });
    }
    if (Array.isArray(children)) children.forEach(function (c) { if (c) n.appendChild(c); });
    else if (children instanceof Node) n.appendChild(children);
    return n;
  }

  /* icon(id, opts) —— 图标只从 sprite 取，禁止 emoji 作功能图标（P0）。
     装饰性 aria-hidden；带语义时必须给 label。 */
  function icon(id, opts) {
    opts = opts || {};
    var cls = 'icon' + (opts.size ? ' icon--' + opts.size : '') + (opts.class ? ' ' + opts.class : '');
    var attrs = { class: cls };
    if (opts.label) { attrs.role = 'img'; attrs['aria-label'] = opts.label; }
    else attrs['aria-hidden'] = 'true';
    var use = document.createElementNS(SVG_NS, 'use');
    use.setAttribute('href', '#' + id);
    return svg('svg', attrs, use);
  }

  /* ---- 状态查表：design-tokens.json semantic.statusMapping 的唯一落点。
     颜色永不单独承载语义 —— 每次渲染同时输出 图标形状 + 文字 + 颜色。 */
  var STATUS = {
    healthy:  { label: '正常',     icon: 'i-status-ok' },
    warning:  { label: '接近上限', icon: 'i-status-warn' },
    degraded: { label: '降级',     icon: 'i-status-degraded' },
    failed:   { label: '故障',     icon: 'i-status-fail' },
    disabled: { label: '已停用',   icon: 'i-status-off' },
    unknown:  { label: '状态未知', icon: 'i-status-off' }
  };

  function statusBadge(state, labelOverride) {
    var s = STATUS[state] || STATUS.unknown;
    return el('span', { class: 'badge', dataset: { state: state } }, [
      icon(s.icon), el('span', { text: labelOverride || s.label })
    ]);
  }

  /* ---- 数值格式化：全部走 mono + tabular-nums，避免刷新时列宽跳动 ---- */
  function num(v, digits) {
    if (v === null || v === undefined || !isFinite(v)) return '—';
    var d = digits === undefined ? 0 : digits;
    return Number(v).toFixed(d);
  }

  /* compact 极大值缩写（12.4M），避免 KPI 卡溢出（五态矩阵 Edge 列） */
  function compact(v) {
    if (v === null || v === undefined || !isFinite(v)) return '—';
    var a = Math.abs(v);
    if (a >= 1e9) return (v / 1e9).toFixed(1) + 'G';
    if (a >= 1e6) return (v / 1e6).toFixed(1) + 'M';
    if (a >= 1e4) return (v / 1e3).toFixed(1) + 'k';
    if (a >= 100) return String(Math.round(v));
    if (a >= 10) return v.toFixed(1);
    return v.toFixed(2);
  }

  function pad(n) { return (n < 10 ? '0' : '') + n; }

  function absTime(v) {
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return '—';
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function clockTime(v) {
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return '—';
    return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function relTime(v) {
    var d = v instanceof Date ? v : new Date(v);
    if (isNaN(d.getTime())) return '—';
    var sec = Math.round((Date.now() - d.getTime()) / 1000);
    if (sec < 0) return '刚刚';
    if (sec < 60) return '距今 ' + sec + ' 秒';
    var m = Math.floor(sec / 60);
    if (m < 60) return '距今 ' + m + ' 分 ' + (sec % 60) + ' 秒';
    var h = Math.floor(m / 60);
    if (h < 24) return '距今 ' + h + ' 小时 ' + (m % 60) + ' 分';
    return '距今 ' + Math.floor(h / 24) + ' 天';
  }

  /* 中间截断：base_url 超长时保留尾部，text-overflow 会把关键路径尾巴吃掉 */
  function midTrunc(s, max) {
    s = String(s === null || s === undefined ? '' : s);
    var m = max || 42;
    if (s.length <= m) return s;
    var head = Math.ceil((m - 1) / 2);
    return s.slice(0, head) + '…/' + s.slice(-(m - head - 2));
  }

  function skeletonRows(cols, rows) {
    var tb = el('tbody');
    for (var r = 0; r < rows; r++) {
      var tr = el('tr');
      for (var c = 0; c < cols; c++) {
        tr.appendChild(el('td', null, el('span', { class: 'skel', style: 'width:' + (40 + ((r + c) % 4) * 15) + '%' })));
      }
      tb.appendChild(tr);
    }
    return tb;
  }

  function emptyBlock(parts) {
    return el('div', { class: 'empty' }, [icon('i-empty', { size: 'lg' })].concat(parts || []));
  }

  w.U = {
    el: el, append: append, clear: clear, svg: svg, icon: icon,
    STATUS: STATUS, statusBadge: statusBadge,
    num: num, compact: compact, absTime: absTime, clockTime: clockTime,
    relTime: relTime, midTrunc: midTrunc,
    skeletonRows: skeletonRows, emptyBlock: emptyBlock
  };
})(window);
