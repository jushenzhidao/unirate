/* ============================================================================
   配置快照与热更新 #/config —— DESIGN.md §5.5
   ----------------------------------------------------------------------------
   左栏状态字段（定义列表，密度高于 KPI 卡）+ 右栏只读 JSON 视图（带搜索）
   + 手动重载 + 运行策略编辑区（page-policy.js）。
   JSON 视图逐行 textContent 渲染，快照里含用户可控的 biz 名与 base_url。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  var state = { snap: null, loading: true, error: null, query: '', reloading: false, flashVer: false };
  var root = null;

  function load() {
    state.loading = true;
    state.error = null;
    render();
    return API.api.snapshot().then(function (data) {
      state.snap = data;
      state.loading = false;
      render();
      w.App.applySnapshot(data);
    }, function (err) {
      state.loading = false;
      state.error = err;
      render();
    });
  }

  function bizStats() {
    var bizs = (state.snap && state.snap.bizs) || {};
    var keys = Object.keys(bizs);
    var enabled = keys.filter(function (k) { return bizs[k] && bizs[k].enabled; }).length;
    return { total: keys.length, enabled: enabled };
  }

  function copyBtn(text, label) {
    return U.el('button', {
      class: 'affix-btn', type: 'button', 'aria-label': label,
      onclick: function () {
        var ok = function () { API.toast('ok', '已复制到剪贴板'); };
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(String(text)).then(ok, function () {
            API.toast('warn', '浏览器拒绝了剪贴板写入，请手动选择复制');
          });
        } else {
          API.toast('warn', '当前浏览器不支持剪贴板 API，请手动选择复制');
        }
      }
    }, U.icon('i-copy'));
  }

  function leftColumn() {
    if (state.loading) {
      return U.el('section', { class: 'panel' }, U.el('div', { class: 'panel-body' },
        [1, 2, 3, 4].map(function (i) {
          return U.el('span', { class: 'skel', style: 'width:' + (50 + i * 8) + '%; margin-bottom: 12px' });
        })));
    }
    var snap = state.snap || {};
    var st = bizStats();
    var degraded = !!snap.degraded;

    var panel = U.el('section', { class: 'panel' });
    var bodyEl = U.el('div', { class: 'panel-body' });

    if (degraded) {
      bodyEl.appendChild(U.el('div', { class: 'note note--degraded', style: 'margin-bottom: var(--sp-3)' }, [
        U.icon('i-status-degraded'),
        U.el('span', {
          text: '配置中心不可达，当前使用最后一份有效配置（版本 ' + snap.version +
            '，加载于 ' + U.absTime(snap.loaded_at) + '）。网关仍在正常限流，但新的配置变更不会生效。'
        })
      ]));
    }

    bodyEl.appendChild(U.el('dl', { class: 'dl' }, [
      U.el('div', null, [
        U.el('dt', { text: '配置版本' }),
        U.el('dd', null, U.el('span', { class: 'row' }, [
          U.el('span', {
            class: 'num', dataset: state.flashVer ? { flash: '1' } : null,
            style: 'font-size: var(--fs-sec)', text: String(snap.version)
          }),
          copyBtn(snap.version, '复制配置版本号')
        ]))
      ]),
      U.el('div', null, [
        U.el('dt', { text: '加载时间' }),
        U.el('dd', null, [
          U.el('span', { class: 'num', text: U.absTime(snap.loaded_at) }),
          U.el('small', { text: U.relTime(snap.loaded_at) })
        ])
      ]),
      U.el('div', null, [
        U.el('dt', { text: '降级状态' }),
        U.el('dd', null, U.statusBadge(degraded ? 'degraded' : 'healthy'))
      ]),
      U.el('div', null, [
        U.el('dt', { text: '业务域数量' }),
        U.el('dd', null, [
          U.el('span', { class: 'num', text: String(st.total) }),
          U.el('small', { text: '其中 ' + st.enabled + ' 个已启用' })
        ])
      ])
    ]));

    var reloadBtn = U.el('button', {
      class: 'btn btn--primary btn--lg', type: 'button', disabled: state.reloading,
      onclick: confirmReload
    }, state.reloading
      ? [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '重载中' })]
      : [U.icon('i-refresh'), U.el('span', { text: '重新加载配置' })]);

    bodyEl.appendChild(U.el('div', { style: 'margin-top: var(--sp-4)' }, reloadBtn));
    panel.appendChild(U.el('div', { class: 'panel-head' },
      U.el('span', { class: 'panel-title', text: '运行状态' })));
    panel.appendChild(bodyEl);
    return panel;
  }

  function confirmReload() {
    var snap = state.snap || {};
    var close = w.App.overlay(U.el('div', {
      class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'rl-t'
    }, [
      U.el('h2', { id: 'rl-t', text: '重新加载配置' }),
      U.el('p', { class: 'helper', text: '将从配置库重新读取全部业务域配置并发布到网关。当前版本 ' + snap.version + '。' }),
      U.el('div', { class: 'modal-foot' }, [
        U.el('button', { class: 'btn btn--lg', type: 'button', text: '取消', onclick: function () { close(); } }),
        U.el('button', {
          class: 'btn btn--primary btn--lg', type: 'button',
          onclick: function () { close(); doReload(); }
        }, [U.icon('i-refresh'), U.el('span', { text: '确认重载' })])
      ])
    ]));
  }

  function doReload() {
    var oldVer = state.snap ? state.snap.version : null;
    state.reloading = true;
    render();
    API.api.reload().then(function (data) {
      state.reloading = false;
      var newVer = data && data.config_version;
      // 版本号未变时必须明说，否则用户以为没生效
      if (String(newVer) === String(oldVer)) {
        API.toast('warn', '配置无变化，版本仍为 ' + newVer + '（' + ((data && data.bizs) || 0) + ' 个业务域）');
      } else {
        API.toast('ok', '配置已重载：版本 ' + oldVer + ' → ' + newVer +
          '，' + ((data && data.bizs) || 0) + ' 个业务域');
        state.flashVer = true;
        w.setTimeout(function () { state.flashVer = false; }, 400);
      }
      load();
    }, function (err) {
      // 失败时保留原值，展开错误详情
      state.reloading = false;
      render();
      API.toast('danger', '配置重载失败，当前生效配置保持不变。', err.message);
    });
  }

  /* JSON 视图：逐行建 span + textContent。
     快照含用户可控的 biz 名与 base_url，绝不能走 innerHTML。 */
  function jsonView() {
    var pre = U.el('pre', { class: 'json-view', tabindex: '0' });
    if (state.loading) {
      pre.appendChild(U.el('span', { class: 'skel', style: 'width:70%' }));
      return pre;
    }
    if (!state.snap) {
      return U.emptyBlock([U.el('span', { text: '无法读取快照' })]);
    }
    var text;
    try { text = JSON.stringify(state.snap, null, 2); } catch (e) { text = '（快照无法序列化）'; }
    var lines = text.split('\n');
    if (lines.length === 1 && !lines[0]) {
      return U.emptyBlock([U.el('span', { text: '快照为空' })]);
    }
    var q = state.query.trim().toLowerCase();
    var hits = 0;
    lines.forEach(function (line) {
      var span = U.el('span', { class: 'json-line' });
      if (q && line.toLowerCase().indexOf(q) >= 0) {
        hits++;
        // 高亮匹配：切片后逐段建节点，仍然只用 textContent
        var lower = line.toLowerCase();
        var pos = 0, at;
        while ((at = lower.indexOf(q, pos)) >= 0) {
          if (at > pos) span.appendChild(document.createTextNode(line.slice(pos, at)));
          span.appendChild(U.el('mark', { text: line.slice(at, at + q.length) }));
          pos = at + q.length;
        }
        if (pos < line.length) span.appendChild(document.createTextNode(line.slice(pos)));
      } else {
        span.textContent = line;
      }
      pre.appendChild(span);
    });
    state.hits = hits;
    return pre;
  }

  function rightColumn() {
    var search = U.el('input', {
      class: 'field', type: 'search', placeholder: '搜索快照内容（biz 名 / base_url / 规则名）',
      value: state.query, style: 'width: 280px', 'aria-label': '搜索快照内容',
      oninput: function (ev) { state.query = ev.target.value; render(); }
    });
    w.App.registerSearch(search);

    return U.el('section', { class: 'panel' }, [
      U.el('div', { class: 'panel-head' }, [
        U.el('span', { class: 'panel-title', text: '当前生效配置' }),
        U.el('span', { class: 'spacer' }),
        state.query.trim() ? U.el('span', {
          class: 'helper', text: (state.hits || 0) + ' 行匹配'
        }) : null,
        U.el('div', { class: 'field-affix' }, [U.icon('i-search', { class: 'affix-lead' }), search])
      ]),
      U.el('div', { class: 'panel-body' }, [
        U.el('p', {
          class: 'helper', style: 'margin: 0 0 var(--sp-2)',
          text: '快照来自网关内存，不是数据库。如需对比数据库真值，重载后再看。'
        }),
        jsonView()
      ])
    ]);
  }

  function errorNote() {
    if (!state.error) return null;
    var e = state.error;
    var msg = e.status === 503
      ? '配置存储未就绪，网关可能仍在 bootstrap。'
      : '读取配置快照失败：' + e.message;
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
    U.clear(root);
    U.append(root, U.el('div', { class: 'page' }, [
      U.el('div', { class: 'page-head' }, [
        U.el('h1', { class: 'page-title', text: '配置快照' }),
        U.el('span', {
          class: 'page-sub',
          text: state.loading ? '读取中' : '版本 ' + ((state.snap && state.snap.version) || '—') + ' · 网关内存中的生效配置'
        })
      ]),
      U.el('div', { class: 'stack' }, [
        errorNote(),
        U.el('div', { class: 'cfg-grid' }, [
          U.el('div', { class: 'stack' }, [leftColumn(), w.PagePolicy.panel()]),
          rightColumn()
        ])
      ])
    ]));
  }

  w.PageConfig = {
    mount: function (outlet) {
      root = outlet;
      render();
      load();
      w.PagePolicy.load(render);
    },
    unmount: function () { root = null; },
    rerender: render
  };
})(window);
