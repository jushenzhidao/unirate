/* ============================================================================
   两级表格的内层：某个 biz 展开后的规则表
   ----------------------------------------------------------------------------
   从 page-rules.js 拆出。分页状态由调用方持有（page-rules.js 的 state），
   本模块只负责渲染 —— 状态放这里会让「折叠再展开」丢失页码。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U;
  var INNER_PAGE_SIZE = 20;

  var ALGO_LABEL = {
    fixed_window: '固定窗口', sliding_window: '滑动窗口', token_bucket: '令牌桶'
  };
  var METRIC_LABEL = { request: '请求数', token: 'Token 数' };

  function ruleEnabled(r) { return r.enabled === null || r.enabled === undefined || r.enabled === true; }

  function head() {
    return U.el('thead', null, U.el('tr', null, [
      U.el('th', { style: 'width:28px' }),
      U.el('th', { text: '规则名' }),
      U.el('th', { text: '类型' }),
      U.el('th', { text: '计量' }),
      U.el('th', { text: '维度组合' }),
      U.el('th', { text: '窗口' }),
      U.el('th', { class: 'num', text: '限额' }),
      U.el('th', { text: '算法' }),
      U.el('th', { text: '状态' }),
      U.el('th', { style: 'width:64px' })
    ]));
  }

  function ruleRow(biz, r, onEdit) {
    var isConc = r.type === 'concurrency';
    return U.el('tr', null, [
      U.el('td', null, U.icon('i-grip', { class: 'cell-meta', label: '规则顺序即优先级' })),
      U.el('td', null, U.el('span', { class: 'mono', text: r.name || '—' })),
      U.el('td', null, U.el('span', { class: 'badge' }, [
        U.icon(isConc ? 'i-rule-concurrency' : 'i-rule-rate'),
        U.el('span', { text: isConc ? '并发' : '速率' })
      ])),
      // 并发规则没有「计量对象」这一维度，显示 — 而不是留空
      U.el('td', { class: 'cell-meta', text: isConc ? '—' : (METRIC_LABEL[r.metric] || r.metric || '—') }),
      U.el('td', null, U.el('div', { class: 'dim-list' },
        (r.dimensions || []).map(function (d) {
          return U.el('span', { class: 'badge badge--mono', text: d });
        }))),
      U.el('td', null, U.el('span', { class: 'mono', text: isConc ? '—' : (r.window || '—') })),
      U.el('td', { class: 'num' }, U.el('span', {
        class: 'mono', text: isConc ? String(r.max_concurrent || 0) : String(r.limit || 0)
      })),
      U.el('td', { class: 'cell-meta', text: isConc ? '—' : (ALGO_LABEL[r.algorithm] || r.algorithm || '—') }),
      U.el('td', null, U.statusBadge(ruleEnabled(r) ? 'healthy' : 'disabled',
        ruleEnabled(r) ? '启用' : '已停用')),
      U.el('td', null, U.el('div', { class: 'row-actions' },
        U.el('button', {
          class: 'btn btn--ghost btn--icon', type: 'button',
          'aria-label': '编辑规则 ' + (r.name || ''),
          onclick: function () { onEdit(biz, r); }
        }, U.icon('i-edit'))))
    ]);
  }

  /* render(biz, opts)
     opts.page      当前页码（由调用方持有）
     opts.onPage    翻页回调
     opts.onEdit    编辑规则
     opts.onCreate  新增规则 */
  function render(biz, opts) {
    var rules = biz.rules || [];
    if (rules.length === 0) {
      return U.el('div', { class: 'sub-wrap' }, U.el('div', { class: 'note' }, [
        U.icon('i-info'),
        U.el('span', { text: '该业务域没有规则，其流量会落到 * 兜底域的规则上。' })
      ]));
    }

    var page = opts.page || 0;
    var pages = Math.ceil(rules.length / INNER_PAGE_SIZE) || 1;
    var slice = rules.length > INNER_PAGE_SIZE
      ? rules.slice(page * INNER_PAGE_SIZE, (page + 1) * INNER_PAGE_SIZE)
      : rules;

    var body = U.el('tbody');
    slice.forEach(function (r) { body.appendChild(ruleRow(biz, r, opts.onEdit)); });

    var wrap = U.el('div', { class: 'sub-wrap' }, [
      U.el('div', { class: 'row', style: 'margin-bottom: var(--sp-2)' }, [
        U.el('span', { class: 'caps', text: '限流规则 ' + rules.length + ' 条' }),
        U.el('span', { class: 'spacer' }),
        U.el('button', {
          class: 'btn', type: 'button', onclick: function () { opts.onCreate(biz); }
        }, [U.icon('i-plus'), U.el('span', { text: '新增规则' })])
      ]),
      U.el('div', { class: 'table-wrap' },
        U.el('table', { class: 'tbl tbl--inner' }, [head(), body]))
    ]);

    // 规则数 >20 时内层分页：一个 biz 挂几十条规则时，展开会把外层列表挤出屏幕
    if (rules.length > INNER_PAGE_SIZE) {
      wrap.appendChild(U.el('div', { class: 'row', style: 'margin-top: var(--sp-2)' }, [
        U.el('span', {
          class: 'helper',
          text: '第 ' + (page + 1) + ' / ' + pages + ' 页，共 ' + rules.length + ' 条'
        }),
        U.el('span', { class: 'spacer' }),
        U.el('button', {
          class: 'btn btn--icon', type: 'button', 'aria-label': '上一页', disabled: page === 0,
          onclick: function () { opts.onPage(page - 1); }
        }, U.icon('i-chevron-left')),
        U.el('button', {
          class: 'btn btn--icon', type: 'button', 'aria-label': '下一页', disabled: page >= pages - 1,
          onclick: function () { opts.onPage(page + 1); }
        }, U.icon('i-chevron-right'))
      ]));
    }
    return wrap;
  }

  w.RulesInnerTable = { render: render, ruleEnabled: ruleEnabled };
})(window);
