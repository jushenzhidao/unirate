/* ============================================================================
   运行策略编辑区（/admin/policy）—— 嵌在配置快照页
   ----------------------------------------------------------------------------
   硬约束：min / max / desc / warn / enum 全部由后端提供，
   前端不硬编码任何约束值 —— 后端调整护栏后界面自动跟随，不会漂移。

   三态展示：source（default / env / page）+ overridden_by_env。
   overridden_by_env=true 的语义要看清：优先级是 page > env > default，
   所以它不代表页面改动无效，而是提示「本项在部署侧也被固定过，当前页面值正压着它」——
   一旦页面覆盖被清除，生效值会回落到 env 值而非内置默认值。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  var state = {
    items: null, priority: '', loading: true, error: null,
    edits: {},      // key -> 原始字符串（待提交）
    problems: {},   // key -> 后端返回的错误文案
    saving: false
  };
  var rerender = null;
  var timer = null;

  var SOURCE_LABEL = { page: '页面覆盖', env: '环境变量', default: '内置默认' };

  function load(cb) {
    rerender = cb || rerender;
    state.loading = true;
    state.error = null;
    return API.api.policy().then(function (data) {
      state.items = (data && data.items) || [];
      state.priority = (data && data.priority) || '';
      state.loading = false;
      if (rerender) rerender();
    }, function (err) {
      state.loading = false;
      state.error = err;
      if (rerender) rerender();
    });
  }

  function dirty() { return Object.keys(state.edits).length > 0; }

  function scheduleValidate() {
    if (timer) w.clearTimeout(timer);
    timer = w.setTimeout(function () {
      if (!dirty()) { state.problems = {}; if (rerender) rerender(); return; }
      API.api.validatePolicy({ values: state.edits }).then(function () {
        state.problems = {};
        if (rerender) rerender();
      }, function (err) {
        // 后端 problems 是 { key: message }，原文兜底不吞
        state.problems = (err.payload && err.payload.problems) || {};
        if (rerender) rerender();
      });
    }, 400);
  }

  function edit(key, raw) {
    if (raw === '' || raw === null || raw === undefined) delete state.edits[key];
    else state.edits[key] = String(raw);
    scheduleValidate();
    if (rerender) rerender();
  }

  function control(it) {
    var pending = state.edits[it.key];
    var cur = pending !== undefined ? pending : String(it.value);

    if (it.kind === 'bool') {
      var sel = U.el('select', {
        class: 'field', style: 'width: 140px', 'aria-label': it.key + ' 取值',
        onchange: function (ev) { edit(it.key, ev.target.value); }
      }, [
        U.el('option', { value: 'true', text: '开启 true' }),
        U.el('option', { value: 'false', text: '关闭 false' })
      ]);
      sel.value = String(cur) === 'true' ? 'true' : 'false';
      return sel;
    }
    if (it.kind === 'enum' && it.enum) {
      var es = U.el('select', {
        class: 'field', style: 'width: 140px', 'aria-label': it.key + ' 取值',
        onchange: function (ev) { edit(it.key, ev.target.value); }
      }, it.enum.map(function (v) { return U.el('option', { value: v, text: v }); }));
      es.value = cur;
      return es;
    }
    // int 与 duration 都走文本输入：duration 必须带单位（后端只接受字符串，
    // 裸数字会被当纳秒解释），int 用 number 会被浏览器静默改写超界值
    var attrs = {
      class: 'field field--mono', type: it.kind === 'int' ? 'number' : 'text',
      value: cur, autocomplete: 'off', spellcheck: 'false',
      'aria-label': it.key + ' 取值',
      'aria-invalid': state.problems[it.key] ? 'true' : 'false',
      oninput: function (ev) { edit(it.key, ev.target.value); }
    };
    // min/max 来自后端，不硬编码
    if (it.kind === 'int') {
      if (it.min !== undefined && it.min !== null) attrs.min = String(it.min);
      if (it.max !== undefined && it.max !== null) attrs.max = String(it.max);
    } else {
      attrs.placeholder = it.min !== undefined ? '例如 ' + it.min : '';
    }
    return U.el('input', attrs);
  }

  function item(it) {
    var pending = state.edits[it.key] !== undefined;
    var problem = state.problems[it.key];
    var bound = (it.min !== undefined && it.min !== null && it.max !== undefined && it.max !== null)
      ? '允许范围 [' + it.min + ', ' + it.max + ']'
      : (it.enum ? '可选 ' + it.enum.join(' / ') : '');

    var node = U.el('div', { class: 'pol-item' }, [
      U.el('div', { class: 'pol-head' }, [
        U.el('span', { class: 'pol-key', text: it.key }),
        U.el('span', { class: 'badge pol-src', text: SOURCE_LABEL[it.source] || it.source }),
        pending ? U.el('span', { class: 'badge', dataset: { state: 'warning' } }, [
          U.icon('i-status-warn'), U.el('span', { text: '待保存' })
        ]) : null
      ]),
      U.el('div', { class: 'pol-ctl' }, [
        control(it),
        bound ? U.el('span', { class: 'pol-bound', text: bound }) : null,
        U.el('span', { class: 'spacer' }),
        it.source === 'page' ? U.el('button', {
          class: 'btn btn--ghost', type: 'button',
          onclick: function () { resetKey(it); }
        }, [U.icon('i-history'), U.el('span', { text: '清除页面覆盖' })]) : null
      ]),
      U.el('p', { class: 'pol-desc', text: it.desc || '' }),
      U.el('p', { class: 'pol-desc pol-bound' }, U.el('span', {
        text: '生效值 ' + it.value + ' · 环境变量 ' + it.env_value + ' · 内置默认 ' + it.default
      }))
    ]);

    // 被 env 显式设置时必须明确提示，并说清优先级，否则运维会误判改动无效
    if (it.overridden_by_env) {
      node.appendChild(U.el('div', { class: 'note pol-warn' }, [
        U.icon('i-info'),
        U.el('span', {
          text: '此项已被环境变量 ' + it.env_name + ' 固定为 ' + it.env_value +
            '。优先级 ' + (state.priority || 'page > env > default') +
            '，页面覆盖仍会生效；清除页面覆盖后会回落到该环境变量值，而不是内置默认值。'
        })
      ]));
    }
    // warn 是后端提供的高风险提示，用户改动该项时必须显性展示
    if (it.warn && pending) {
      node.appendChild(U.el('div', { class: 'note note--warn pol-warn' }, [
        U.icon('i-status-warn'), U.el('span', { text: it.warn })
      ]));
    }
    if (problem) {
      node.appendChild(U.el('div', { class: 'note note--danger pol-warn' }, [
        U.icon('i-status-fail'), U.el('span', { text: String(problem) })
      ]));
    }
    return node;
  }

  function resetKey(it) {
    var close = w.App.overlay(U.el('div', {
      class: 'modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'rst-t'
    }, [
      U.el('h2', { id: 'rst-t', text: '清除页面覆盖 · ' + it.key }),
      U.el('p', { class: 'helper', text: '清除后生效值会回落到 ' +
        (it.overridden_by_env ? '环境变量 ' + it.env_name + ' 的值 ' + it.env_value : '内置默认值 ' + it.default) +
        '，并立即发布到网关。' }),
      U.el('div', { class: 'modal-foot' }, [
        U.el('button', { class: 'btn btn--lg', type: 'button', text: '取消', onclick: function () { close(); } }),
        U.el('button', {
          class: 'btn btn--danger btn--lg', type: 'button',
          onclick: function () { close(); submit({ reset: [it.key] }); }
        }, [U.icon('i-history'), U.el('span', { text: '确认清除' })])
      ])
    ]));
  }

  function submit(payload) {
    state.saving = true;
    if (rerender) rerender();
    API.api.putPolicy(payload).then(function (data) {
      state.saving = false;
      state.edits = {};
      state.problems = {};
      if (API.isPublishFailed(data)) {
        API.toast('warn', '策略已写入数据库，但配置发布失败。网关会在下次轮询（≤30s）拉取。' +
          '如需立即生效，请手动重载配置。');
      } else {
        API.toast('ok', '运行策略已更新，配置版本 ' + (data && data.config_version));
        // PUT 成功后后端回传同一份三态视图，直接采用，避免再发一次 GET
        // 被别人的改动夹在中间
        if (data && data.items) {
          state.items = data.items;
          state.priority = data.priority || state.priority;
        }
      }
      if (rerender) rerender();
      w.App.refreshHealth();
    }, function (err) {
      state.saving = false;
      state.problems = (err.payload && err.payload.problems) || {};
      if (rerender) rerender();
      API.toast('danger', '运行策略更新失败：' + err.message);
    });
  }

  function panel() {
    var head = U.el('div', { class: 'panel-head' }, [
      U.el('span', { class: 'panel-title', text: '运行策略' }),
      U.el('span', { class: 'spacer' }),
      U.el('span', { class: 'helper', text: state.priority ? '优先级 ' + state.priority : '' })
    ]);
    var body = U.el('div', { class: 'panel-body' });

    if (state.loading) {
      [1, 2, 3].forEach(function (i) {
        body.appendChild(U.el('span', { class: 'skel', style: 'width:' + (55 + i * 10) + '%; margin-bottom: 14px' }));
      });
      return U.el('section', { class: 'panel' }, [head, body]);
    }
    if (state.error) {
      body.appendChild(U.el('div', { class: 'note note--danger' }, [
        U.icon('i-status-fail'),
        U.el('div', null, [
          U.el('div', {
            text: state.error.status === 503
              ? '配置存储未就绪，无法读取运行策略。'
              : '读取运行策略失败：' + state.error.message
          }),
          U.el('button', {
            class: 'btn', type: 'button', style: 'margin-top: var(--sp-2)',
            onclick: function () { load(); }
          }, [U.icon('i-refresh'), U.el('span', { text: '重试' })])
        ])
      ]));
      return U.el('section', { class: 'panel' }, [head, body]);
    }

    body.appendChild(U.el('p', {
      class: 'helper', style: 'margin: 0 0 var(--sp-2)',
      text: '取值范围与风险提示由网关提供，随部署版本变化。保存后立即发布到全部实例。'
    }));
    (state.items || []).forEach(function (it) { body.appendChild(item(it)); });

    var hasProblem = Object.keys(state.problems).length > 0;
    body.appendChild(U.el('div', { class: 'row row-wrap', style: 'margin-top: var(--sp-4)' }, [
      U.el('span', {
        class: 'helper',
        text: dirty() ? Object.keys(state.edits).length + ' 项待保存' : '没有待保存的改动'
      }),
      U.el('span', { class: 'spacer' }),
      dirty() ? U.el('button', {
        class: 'btn', type: 'button', text: '放弃改动',
        onclick: function () {
          state.edits = {}; state.problems = {};
          if (rerender) rerender();
        }
      }) : null,
      U.el('button', {
        class: 'btn btn--primary', type: 'button',
        disabled: !dirty() || hasProblem || state.saving,
        onclick: function () { submit({ values: state.edits }); }
      }, state.saving
        ? [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '保存中' })]
        : [U.icon('i-check'), U.el('span', { text: '保存策略' })])
    ]));

    return U.el('section', { class: 'panel' }, [head, body]);
  }

  w.PagePolicy = { load: load, panel: panel };
})(window);
