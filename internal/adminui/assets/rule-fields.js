/* ============================================================================
   规则表单的字段构造器
   ----------------------------------------------------------------------------
   从 page-rules-form.js 拆出：按规则类型分支的字段组（rate / concurrency）
   与共用控件（分段控件、字段行、维度 chips）。

   全部函数接受一个显式 ctx（draft / 回调），不持有任何模块级可变状态 ——
   同一时刻只可能有一个抽屉，但把状态放模块级会让「关掉再开一个」继承上次的残留。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U;

  var ALGOS = [
    { v: 'fixed_window', t: '固定窗口' },
    { v: 'sliding_window', t: '滑动窗口' },
    { v: 'token_bucket', t: '令牌桶' }
  ];
  var WINDOWS = ['1s', '1m', '5m', '1h', '1d', '1w'];
  var DIMS = ['global', 'biz', 'ip', 'token', 'path', 'method'];
  // 窗口 → 秒。仅用于「≈ 33.3 req/s」这类换算展示，不参与校验（校验在后端）
  var WINDOW_SECONDS = { '1s': 1, '1m': 60, '5m': 300, '1h': 3600, '1d': 86400, '1w': 604800 };

  function row(labelText, control, helperText, required) {
    return U.el('div', { class: 'form-row' }, [
      U.el('label', null, [
        U.el('span', { text: labelText }),
        required ? U.el('span', { class: 'req', text: ' *' }) : null
      ]),
      control,
      helperText ? U.el('p', { class: 'helper', text: helperText }) : null
    ]);
  }

  function seg(options, current, onPick, disabledFn) {
    var box = U.el('div', { class: 'seg', role: 'group' });
    options.forEach(function (o) {
      box.appendChild(U.el('button', {
        type: 'button', text: o.t,
        'aria-pressed': current === o.v ? 'true' : 'false',
        disabled: disabledFn ? disabledFn(o.v) : false,
        onclick: function () { onPick(o.v); }
      }));
    });
    return box;
  }

  /* windowField —— 预设窗口 + 自定义。
     自定义输入只在预设未命中时出现，避免默认就摆两个控件让人不知道填哪个。 */
  function windowField(ctx) {
    var draft = ctx.draft;
    var preset = WINDOWS.indexOf(draft.window) >= 0;
    var sel = U.el('select', {
      class: 'field field--lg', 'aria-label': '时间窗口',
      onchange: function (ev) {
        if (ev.target.value === '__custom') {
          // 切到自定义时清空，否则会带着上一个预设值进自定义框，看不出是否已改
          draft.window = '';
          ctx.repaint();
          return;
        }
        ctx.set('window', ev.target.value);
      }
    }, WINDOWS.map(function (v) { return U.el('option', { value: v, text: v }); })
      .concat([U.el('option', { value: '__custom', text: '自定义…' })]));
    sel.value = preset ? draft.window : '__custom';

    var box = U.el('div', { class: 'row' }, [sel]);
    if (!preset) {
      box.appendChild(U.el('input', {
        class: 'field field--lg field--mono', type: 'text', value: draft.window || '',
        placeholder: '例如 30s / 2h', 'aria-label': '自定义时间窗口',
        oninput: function (ev) { draft.window = ev.target.value; ctx.revalidate(); }
      }));
    }
    return box;
  }

  /* limitField —— 限额 + 实时换算。
     换算文本就地更新（不整体 repaint），否则每敲一个字符都重建整个表单，
     输入框会失焦。 */
  function limitField(ctx) {
    var draft = ctx.draft;
    function rateText() {
      var secs = WINDOW_SECONDS[draft.window];
      if (!secs || !isFinite(draft.limit) || draft.limit <= 0) return '';
      return '约 ' + (draft.limit / secs).toFixed(secs === 1 ? 0 : 1) +
        (draft.metric === 'token' ? ' tok/s' : ' req/s');
    }
    var rate = U.el('span', { class: 'pol-bound', text: rateText() });
    var input = U.el('input', {
      class: 'field field--lg field--mono', type: 'number', min: '1',
      value: String(draft.limit === undefined ? '' : draft.limit),
      'aria-label': '限额',
      oninput: function (ev) {
        draft.limit = parseInt(ev.target.value, 10);
        ctx.revalidate();
        rate.textContent = rateText();
      }
    });
    return U.el('div', { class: 'row' }, [input, rate]);
  }

  /* rateFields —— 类型为 rate 时的字段组。返回节点数组。 */
  function rateFields(ctx) {
    var draft = ctx.draft;
    // 令牌桶是持久速率桶，与窗口内总量语义不兼容
    var conflict = draft.algorithm === 'token_bucket' && draft.metric === 'token';
    var out = [
      row('计量维度', seg([{ v: 'request', t: '请求数 request' }, { v: 'token', t: 'Token 数 token' }],
        draft.metric, function (v) { ctx.set('metric', v); })),
      row('时间窗口', windowField(ctx), null, true),
      row('限额', limitField(ctx), null, true),
      row('算法', seg(ALGOS, draft.algorithm, function (v) { ctx.set('algorithm', v); }),
        conflict ? '令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定或滑动窗口' : null)
    ];
    if (conflict) {
      out.push(U.el('div', { class: 'note note--danger', style: 'margin-bottom: var(--sp-4)' }, [
        U.icon('i-status-fail'),
        U.el('span', { text: '当前组合不可用：算法为令牌桶且计量为 Token。请改用固定窗口或滑动窗口。' })
      ]));
    }
    if (draft.algorithm === 'token_bucket') {
      out.push(row('突发容量 burst', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '0',
        value: String(draft.burst || 0), 'aria-label': '突发容量',
        oninput: function (ev) { draft.burst = parseInt(ev.target.value, 10) || 0; ctx.revalidate(); }
      }), '令牌桶在限额之上允许的瞬时突发量'));
    }
    return { nodes: out, conflict: conflict };
  }

  /* concurrencyFields —— 类型为 concurrency 时的字段组 */
  function concurrencyFields(ctx) {
    var draft = ctx.draft;
    return [
      row('最大并发', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '1',
        value: String(draft.max_concurrent || 0), 'aria-label': '最大并发',
        oninput: function (ev) { draft.max_concurrent = parseInt(ev.target.value, 10); ctx.revalidate(); }
      }), null, true),
      row('持有超时（秒）', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '0',
        value: String(draft.timeout || 0), 'aria-label': '持有超时秒数',
        oninput: function (ev) { draft.timeout = parseInt(ev.target.value, 10) || 0; ctx.revalidate(); }
      }), '留空或 0 取默认 120s。超时后并发槽位自动释放，防止请求异常退出导致槽位泄漏')
    ];
  }

  /* dimensionField —— 维度多选 chips。
     选中 global 后其余维度立即禁用：global 表示不分维度的全局限流。 */
  function dimensionField(ctx) {
    var draft = ctx.draft;
    var hasGlobal = (draft.dimensions || []).indexOf('global') >= 0;
    var chips = U.el('div', { class: 'chips' });
    DIMS.forEach(function (d) {
      var on = (draft.dimensions || []).indexOf(d) >= 0;
      chips.appendChild(U.el('button', {
        class: 'chip', type: 'button', text: d,
        'aria-pressed': on ? 'true' : 'false',
        disabled: hasGlobal && d !== 'global',
        onclick: function () {
          var list = (draft.dimensions || []).slice();
          if (d === 'global') list = on ? [] : ['global'];
          else if (on) list.splice(list.indexOf(d), 1);
          else list.push(d);
          ctx.set('dimensions', list);
        }
      }));
    });
    return row('限流维度', chips,
      hasGlobal ? 'global 表示不分维度的全局限流，不能与具体维度组合'
        : '多选。维度组合决定计数键的粒度', true);
  }

  w.RuleFields = {
    row: row, seg: seg,
    rateFields: rateFields, concurrencyFields: concurrencyFields,
    dimensionField: dimensionField
  };
})(window);
