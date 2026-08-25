/* ============================================================================
   业务域与限流规则 #/rules —— DESIGN.md §5.3
   ----------------------------------------------------------------------------
   两级表格：外层 biz，展开行显示该 biz 的规则表。
   不做「列表页 + 详情页」跳转 —— SRE 常需横向比较多个 biz 的规则，跳转会丢上下文。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  var state = {
    items: null,
    loading: true,
    error: null,
    expanded: {},
    query: '',
    filter: 'all',
    flash: null,
    innerPage: {}
  };
  var root = null;

  function load(focusBiz) {
    state.loading = true;
    state.error = null;
    render();
    return API.api.bizs().then(function (data) {
      state.items = (data && data.items) || [];
      state.loading = false;
      if (focusBiz) state.expanded[focusBiz] = true;
      render();
    }, function (err) {
      state.loading = false;
      state.error = err;
      render();
    });
  }

  function visible() {
    var q = state.query.trim().toLowerCase();
    return (state.items || []).filter(function (b) {
      if (state.filter === 'enabled' && !b.enabled) return false;
      if (state.filter === 'disabled' && b.enabled) return false;
      if (!q) return true;
      if (String(b.biz).toLowerCase().indexOf(q) >= 0) return true;
      if (String(b.base_url || '').toLowerCase().indexOf(q) >= 0) return true;
      return (b.rules || []).some(function (r) {
        return String(r.name || '').toLowerCase().indexOf(q) >= 0;
      });
    });
  }

  function outerTable() {
    var head = U.el('thead', null, U.el('tr', null, [
      U.el('th', { style: 'width:28px' }),
      U.el('th', { text: '状态' }),
      U.el('th', { text: '业务域' }),
      U.el('th', { text: 'base_url' }),
      U.el('th', { class: 'num', text: '规则数' }),
      U.el('th', { text: '剥离前缀' }),
      U.el('th', { text: 'Token 计量' }),
      U.el('th', { text: '更新时间' }),
      U.el('th', { style: 'width:80px' })
    ]));

    if (state.loading) {
      return U.el('table', { class: 'tbl' }, [head, U.skeletonRows(9, 6)]);
    }

    var rows = visible();
    var body = U.el('tbody');
    rows.forEach(function (b) {
      var open = !!state.expanded[b.biz];
      var tr = U.el('tr', {
        tabindex: '0',
        dataset: state.flash === b.biz ? { flash: '1' } : null,
        onkeydown: function (ev) {
          if (ev.key === 'Enter') { ev.preventDefault(); toggle(b.biz); }
        }
      }, [
        U.el('td', null, U.el('button', {
          class: 'expander', type: 'button',
          'aria-expanded': open ? 'true' : 'false',
          'aria-label': (open ? '收起' : '展开') + '业务域 ' + b.biz + ' 的规则',
          onclick: function (ev) { ev.stopPropagation(); toggle(b.biz); }
        }, U.icon('i-chevron-right'))),
        U.el('td', null, U.statusBadge(b.enabled ? 'healthy' : 'disabled', b.enabled ? '启用' : '已停用')),
        U.el('td', null, U.el('span', { class: 'mono', text: b.biz })),
        U.el('td', null, U.el('span', {
          class: 'mono cell-meta',
          text: b.base_url ? U.midTrunc(b.base_url, 44) : '（兜底域，不转发）',
          title: b.base_url || ''
        })),
        U.el('td', { class: 'num' }, U.el('span', { class: 'mono', text: String((b.rules || []).length) })),
        U.el('td', { class: 'cell-meta', text: b.path_strip_prefix ? '是' : '否' }),
        U.el('td', { class: 'cell-meta', text: b.token_metering ? (b.token_metering.mode || 'auto') : '未配置' }),
        U.el('td', null, U.el('span', {
          class: 'mono cell-meta',
          text: b.updated_at ? U.absTime(b.updated_at) : '—',
          title: b.updated_at ? U.relTime(b.updated_at) : ''
        })),
        U.el('td', null, U.el('div', { class: 'row-actions' }, [
          U.el('button', {
            class: 'btn btn--ghost btn--icon', type: 'button', 'aria-label': '编辑业务域 ' + b.biz,
            onclick: function (ev) { ev.stopPropagation(); w.BizForm.openBiz(b, afterSave); }
          }, U.icon('i-edit')),
          U.el('button', {
            class: 'btn btn--ghost btn--icon', type: 'button', 'aria-label': '删除业务域 ' + b.biz,
            onclick: function (ev) { ev.stopPropagation(); confirmDelete(b); }
          }, U.icon('i-trash'))
        ]))
      ]);
      body.appendChild(tr);
      if (open) {
        body.appendChild(U.el('tr', { class: 'sub' },
          U.el('td', { colspan: '9' }, w.RulesInnerTable.render(b, {
            page: state.innerPage[b.biz] || 0,
            onPage: function (p) { state.innerPage[b.biz] = p; render(); },
            onEdit: function (bz, r) { w.RulesForm.open(bz, r, afterSave); },
            onCreate: function (bz) { w.RulesForm.open(bz, null, afterSave); }
          }))));
      }
    });

    var table = U.el('table', { class: 'tbl' }, [head, body]);
    if (rows.length === 0) {
      var isFiltered = state.query.trim() || state.filter !== 'all';
      return U.el('div', null, [
        table,
        isFiltered
          ? U.emptyBlock([U.el('span', { text: '没有业务域匹配当前筛选条件。' })])
          : U.emptyBlock([
            U.el('strong', { text: '尚未配置任何业务域' }),
            U.el('span', { text: '网关会以 * 兜底规则处理所有流量。' }),
            U.el('span', {
              class: 'helper',
              text: '路径首段即业务域标识，例如 POST /openai/v1/chat/completions 命中 biz openai。'
            }),
            U.el('button', {
              class: 'btn btn--primary', type: 'button', style: 'margin-top: var(--sp-2)',
              onclick: function () { w.BizForm.openBiz(null, afterSave); }
            }, [U.icon('i-plus'), U.el('span', { text: '新增业务域' })])
          ])
      ]);
    }
    return table;
  }

  function toggle(biz) {
    state.expanded[biz] = !state.expanded[biz];
    render();
  }

  function afterSave(biz, publishFailed) {
    state.flash = biz;
    load(biz).then(function () {
      w.setTimeout(function () { state.flash = null; }, 400);
    });
    if (!publishFailed) w.App.refreshHealth();
  }

  /* 删除整个业务域会让该域流量瞬间失控，因此要求手动输入 biz 名才激活按钮 */
  function confirmDelete(b) {
    var n = (b.rules || []).length;
    var input = U.el('input', {
      class: 'field field--lg field--mono', type: 'text', autocomplete: 'off', spellcheck: 'false',
      'aria-label': '输入业务域名称以确认删除'
    });
    var btn = U.el('button', { class: 'btn btn--danger btn--lg', type: 'button', disabled: true },
      [U.icon('i-trash'), U.el('span', { text: '删除业务域' })]);
    input.addEventListener('input', function () { btn.disabled = input.value !== b.biz; });

    var close = w.App.overlay(U.el('div', { class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'del-t' }, [
      U.el('h2', { id: 'del-t', text: '删除业务域 ' + b.biz }),
      U.el('p', { class: 'helper' }, U.el('span', {
        text: '删除后 ' + b.biz + ' 的全部 ' + n + ' 条限流规则立即失效，该域流量将不再受限。输入 ' +
          b.biz + ' 确认。'
      })),
      U.el('div', { style: 'margin-top: var(--sp-3)' }, input),
      U.el('div', { class: 'modal-foot' }, [
        U.el('button', {
          class: 'btn btn--lg', type: 'button', text: '取消',
          onclick: function () { close(); }
        }),
        btn
      ])
    ]), input);

    btn.addEventListener('click', function () {
      btn.disabled = true;
      U.clear(btn);
      U.append(btn, [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '删除中' })]);
      API.api.deleteBiz(b.biz).then(function (data) {
        close();
        API.toast('ok', '已删除业务域 ' + b.biz + '，配置版本 ' + (data && data.config_version));
        delete state.expanded[b.biz];
        load();
        w.App.refreshHealth();
      }, function (err) {
        close();
        API.toast('danger', '删除业务域失败：' + err.message);
      });
    });
  }

  function toolbar() {
    var search = U.el('input', {
      class: 'field', type: 'search', placeholder: '搜索业务域 / 规则名 / base_url',
      value: state.query, style: 'width: 260px',
      'aria-label': '搜索业务域、规则名或 base_url',
      oninput: function (ev) { state.query = ev.target.value; render(); }
    });
    w.App.registerSearch(search);

    var sel = U.el('select', {
      class: 'field', style: 'width: 120px', 'aria-label': '按启用状态筛选',
      onchange: function (ev) { state.filter = ev.target.value; render(); }
    }, [
      U.el('option', { value: 'all', text: '全部' }),
      U.el('option', { value: 'enabled', text: '仅启用' }),
      U.el('option', { value: 'disabled', text: '仅停用' })
    ]);
    sel.value = state.filter;

    return U.el('div', { class: 'toolbar' }, [
      U.el('div', { class: 'field-affix' }, [
        U.icon('i-search', { class: 'affix-lead' }), search
      ]),
      U.icon('i-filter', { class: 'cell-meta' }), sel,
      U.el('span', { class: 'spacer' }),
      U.el('button', {
        class: 'btn', type: 'button', onclick: function () { load(); }
      }, [U.icon('i-refresh'), U.el('span', { text: '重新读取' })]),
      U.el('button', {
        class: 'btn btn--primary', type: 'button',
        onclick: function () { w.BizForm.openBiz(null, afterSave); }
      }, [U.icon('i-plus'), U.el('span', { text: '新增业务域' })])
    ]);
  }

  function errorNote() {
    var e = state.error;
    if (!e) return null;
    // 「管理面挂了 ≠ 网关挂了」—— 运维必须能区分这两件事
    var msg = e.status === 503
      ? '配置数据库不可达，无法读写业务域。当前网关仍在用最后一份有效配置服务流量。'
      : '读取业务域失败：' + e.message;
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
    var count = (state.items || []).length;
    U.clear(root);
    U.append(root, U.el('div', { class: 'page' }, [
      U.el('div', { class: 'page-head' }, [
        U.el('h1', { class: 'page-title', text: '限流规则' }),
        U.el('span', {
          class: 'page-sub',
          text: state.loading ? '读取中' : count + ' 个业务域，展开行查看该域规则'
        })
      ]),
      toolbar(),
      U.el('div', { class: 'stack' }, [
        errorNote(),
        U.el('section', { class: 'panel' },
          U.el('div', { class: 'panel-body panel-body--flush' },
            U.el('div', { class: 'table-wrap' }, outerTable())))
      ])
    ]));
  }

  w.PageRules = {
    mount: function (outlet, params) {
      root = outlet;
      var focus = params && params.biz;
      if (focus) state.expanded[focus] = true;
      render();
      load(focus);
    },
    unmount: function () { root = null; },
    reload: function () { return load(); }
  };
})(window);
