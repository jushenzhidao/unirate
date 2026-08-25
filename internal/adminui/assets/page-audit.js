/* ============================================================================
   审计日志 #/audit —— DESIGN.md §5.4
   ----------------------------------------------------------------------------
   安全要点：detail 是用户可控 JSON（规则内容原样入库），是本控制台最明显的
   注入面。本文件对它只做 textContent 渲染，绝不 innerHTML、绝不 eval。
   导出 CSV 同理要防公式注入（=/+/-/@ 开头的单元格会被 Excel 当公式执行）。

   筛选是前端筛选（后端固定返回最近 100 条），工具栏必须明示这一点，
   否则用户会误以为筛的是全量。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;
  var DETAIL_LIMIT = 4000;

  var state = {
    items: null, loading: true, error: null,
    action: 'all', biz: 'all', operator: '', expanded: {}
  };
  var root = null;

  var ACTION_LABEL = {
    upsert_biz: '写入业务域', delete_biz: '删除业务域', update_policy: '更新运行策略'
  };

  function load() {
    state.loading = true;
    state.error = null;
    render();
    return API.api.audit().then(function (data) {
      state.items = (data && data.items) || [];
      state.loading = false;
      render();
    }, function (err) {
      state.loading = false;
      state.error = err;
      render();
    });
  }

  function visible() {
    var q = state.operator.trim().toLowerCase();
    return (state.items || []).filter(function (it) {
      if (state.action !== 'all' && it.action !== state.action) return false;
      if (state.biz !== 'all' && it.biz !== state.biz) return false;
      if (q && String(it.operator || '').toLowerCase().indexOf(q) < 0) return false;
      return true;
    });
  }

  function uniq(key) {
    var seen = {};
    (state.items || []).forEach(function (it) {
      var v = it[key];
      if (v !== undefined && v !== null && v !== '') seen[v] = true;
    });
    return Object.keys(seen).sort();
  }

  /* detail 展开区：行内展开而非弹窗，pre-wrap 保留 JSON 结构。
     若恰好 4000 字符说明被后端截断，必须标注，否则用户以为 JSON 坏了。 */
  function detailBlock(it) {
    var raw = String(it.detail || '');
    var pretty = raw;
    try {
      // 仅为可读性格式化。JSON.parse 不执行任何代码，且结果只经 textContent 输出
      pretty = JSON.stringify(JSON.parse(raw), null, 2);
    } catch (e) { pretty = raw; }
    var box = U.el('div', { class: 'sub-wrap' }, U.el('pre', { class: 'detail-pre', text: pretty }));
    if (raw.length >= DETAIL_LIMIT) {
      box.appendChild(U.el('p', {
        class: 'detail-trunc-note',
        text: 'detail 已被截断至 ' + DETAIL_LIMIT + ' 字符，以上不是完整内容。'
      }));
    }
    return box;
  }

  function table() {
    var head = U.el('thead', null, U.el('tr', null, [
      U.el('th', { style: 'width:28px' }),
      U.el('th', { text: '时间' }),
      U.el('th', { text: '动作' }),
      U.el('th', { text: '业务域' }),
      U.el('th', { text: '操作者' }),
      U.el('th', { text: '来源 IP' }),
      U.el('th', { text: 'detail' })
    ]));

    if (state.loading) return U.el('table', { class: 'tbl' }, [head, U.skeletonRows(7, 8)]);

    var rows = visible();
    var body = U.el('tbody');
    rows.forEach(function (it) {
      var open = !!state.expanded[it.id];
      var hasDetail = !!(it.detail && String(it.detail).length);
      var unknownOp = !it.operator || it.operator === 'unknown';
      body.appendChild(U.el('tr', {
        tabindex: '0',
        onkeydown: function (ev) {
          if (ev.key === 'Enter' && hasDetail) { ev.preventDefault(); toggle(it.id); }
        }
      }, [
        U.el('td', null, hasDetail ? U.el('button', {
          class: 'expander', type: 'button',
          'aria-expanded': open ? 'true' : 'false',
          'aria-label': (open ? '收起' : '展开') + '第 ' + it.id + ' 条变更详情',
          onclick: function (ev) { ev.stopPropagation(); toggle(it.id); }
        }, U.icon('i-chevron-right')) : null),
        U.el('td', null, U.el('time', {
          text: U.absTime(it.created_at), title: U.relTime(it.created_at),
          datetime: String(it.created_at || '')
        })),
        U.el('td', null, U.el('span', { class: 'badge' }, [
          U.icon(it.action === 'delete_biz' ? 'i-trash'
            : it.action === 'update_policy' ? 'i-nav-config' : 'i-edit'),
          U.el('span', { text: ACTION_LABEL[it.action] || it.action })
        ])),
        U.el('td', null, U.el('span', { class: 'mono', text: it.biz || '—' })),
        U.el('td', null, U.el('span', {
          class: unknownOp ? 'cell-meta' : '',
          style: unknownOp ? 'font-style: italic' : null
        }, [U.icon('i-user'), ' ', it.operator || 'unknown'])),
        U.el('td', null, U.el('span', { class: 'mono cell-meta', text: it.remote_addr || '—' })),
        U.el('td', null, hasDetail ? U.el('button', {
          class: 'link-btn cell-trunc mono', type: 'button',
          text: String(it.detail).slice(0, 60),
          onclick: function () { toggle(it.id); }
        }) : U.el('span', { class: 'cell-meta', text: '—' }))
      ]));
      if (open && hasDetail) {
        body.appendChild(U.el('tr', { class: 'sub' },
          U.el('td', { colspan: '7' }, detailBlock(it))));
      }
    });

    var t = U.el('table', { class: 'tbl' }, [head, body]);
    if (rows.length === 0) {
      var filtered = state.action !== 'all' || state.biz !== 'all' || state.operator.trim();
      return U.el('div', null, [t, U.emptyBlock([
        filtered
          ? U.el('span', { text: '最近 100 条记录中没有匹配当前筛选条件的变更。' })
          : U.el('span', { text: '暂无配置变更记录。所有通过管理面的写操作都会记录在此。' })
      ])]);
    }
    return t;
  }

  function toggle(id) {
    state.expanded[id] = !state.expanded[id];
    render();
  }

  /* CSV 导出：前端拼串，不需要后端。
     公式注入防护：以 = + - @ 开头的单元格前置单引号，
     否则 Excel 打开会把 audit.detail 里的内容当公式执行。 */
  function csvCell(v) {
    var s = String(v === null || v === undefined ? '' : v);
    if (/^[=+\-@\t\r]/.test(s)) s = "'" + s;
    return '"' + s.replace(/"/g, '""') + '"';
  }

  function exportCSV() {
    var rows = visible();
    if (rows.length === 0) { API.toast('warn', '当前筛选结果为空，没有可导出的记录'); return; }
    var lines = [['id', '时间', '动作', '业务域', '操作者', '来源IP', 'detail'].join(',')];
    rows.forEach(function (it) {
      lines.push([
        csvCell(it.id), csvCell(U.absTime(it.created_at)), csvCell(it.action),
        csvCell(it.biz), csvCell(it.operator || 'unknown'), csvCell(it.remote_addr),
        csvCell(it.detail)
      ].join(','));
    });
    // \ufeff BOM 让 Excel 正确识别 UTF-8，否则中文乱码
    var blob = new Blob(['\ufeff' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' });
    var url = URL.createObjectURL(blob);
    var a = U.el('a', { href: url, download: 'unirate-audit-' + Date.now() + '.csv' });
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
    API.toast('ok', '已导出 ' + rows.length + ' 条记录');
  }

  function toolbar() {
    var actionSel = U.el('select', {
      class: 'field', style: 'width: 150px', 'aria-label': '按动作筛选',
      onchange: function (ev) { state.action = ev.target.value; render(); }
    }, [U.el('option', { value: 'all', text: '全部动作' })].concat(
      uniq('action').map(function (a) {
        return U.el('option', { value: a, text: ACTION_LABEL[a] || a });
      })));
    actionSel.value = state.action;

    var bizSel = U.el('select', {
      class: 'field', style: 'width: 140px', 'aria-label': '按业务域筛选',
      onchange: function (ev) { state.biz = ev.target.value; render(); }
    }, [U.el('option', { value: 'all', text: '全部业务域' })].concat(
      uniq('biz').map(function (b) { return U.el('option', { value: b, text: b }); })));
    bizSel.value = state.biz;

    var opInput = U.el('input', {
      class: 'field', type: 'search', placeholder: '搜索操作者', value: state.operator,
      style: 'width: 180px', 'aria-label': '搜索操作者',
      oninput: function (ev) { state.operator = ev.target.value; render(); }
    });
    w.App.registerSearch(opInput);

    return U.el('div', { class: 'toolbar' }, [
      U.icon('i-filter', { class: 'cell-meta' }), actionSel, bizSel,
      U.el('div', { class: 'field-affix' }, [U.icon('i-search', { class: 'affix-lead' }), opInput]),
      U.el('span', { class: 'spacer' }),
      U.el('button', { class: 'btn', type: 'button', onclick: function () { load(); } },
        [U.icon('i-refresh'), U.el('span', { text: '重新读取' })]),
      U.el('button', { class: 'btn', type: 'button', onclick: exportCSV },
        [U.icon('i-download'), U.el('span', { text: '导出 CSV' })]),
      U.el('p', {
        class: 'toolbar-note',
        text: '显示最近 100 条变更记录，筛选在本地进行 —— 不是对全量历史的查询。'
      })
    ]);
  }

  function errorNote() {
    if (!state.error) return null;
    var e = state.error;
    var msg = e.status === 503
      ? '配置数据库不可达，无法读取审计日志。当前网关仍在用最后一份有效配置服务流量。'
      : '读取审计日志失败：' + e.message;
    return U.el('div', { class: 'note note--danger' }, [
      U.icon('i-status-fail'),
      U.el('div', null, [
        U.el('div', { text: msg }),
        U.el('button', {
          class: 'btn', type: 'button', style: 'margin-top: var(--sp-2)',
          onclick: function () { load(); }
        }, [U.icon('i-refresh'), U.el('span', { text: '重试' })])
      ])
    ]);
  }

  function render() {
    if (!root) return;
    var shown = visible().length;
    var total = (state.items || []).length;
    U.clear(root);
    U.append(root, U.el('div', { class: 'page' }, [
      U.el('div', { class: 'page-head' }, [
        U.el('h1', { class: 'page-title', text: '审计日志' }),
        U.el('span', {
          class: 'page-sub',
          text: state.loading ? '读取中' : '显示 ' + shown + ' / ' + total + ' 条'
        })
      ]),
      toolbar(),
      U.el('div', { class: 'stack' }, [
        errorNote(),
        U.el('section', { class: 'panel' },
          U.el('div', { class: 'panel-body panel-body--flush' },
            U.el('div', { class: 'table-wrap' }, table())))
      ])
    ]));
  }

  w.PageAudit = {
    mount: function (outlet) { root = outlet; render(); load(); },
    unmount: function () { root = null; }
  };
})(window);
