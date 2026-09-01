/* ============================================================================
   规则表单的字段构造器
   ----------------------------------------------------------------------------
   从 page-rules-form.js 拆出：按规则类型分支的字段组（rate / concurrency）
   与共用控件（分段控件、字段行、维度 chips）。

   全部函数接受一个显式 ctx（draft / 回调），不持有任何模块级可变状态 ——
   同一时刻只可能有一个抽屉，但把状态放模块级会让「关掉再开一个」继承上次的残留。

   常量一律读 RuleSpec（rule-spec.js），本文件不再自持副本：
   校验器与预览都要用同一份常量，各留一份迟早分叉，而分叉的症状是
   「界面允许的配置后端拒绝」这类无法自证的错。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, S = w.RuleSpec, RV = w.RuleValidate;

  /* row —— 字段行。notes 是 fieldNotes() 产出的节点数组（阻断/规范化提示），
     跟在 helper 之后：helper 讲「这个字段是什么」，notes 讲「你现在填的这个值
     会怎样」。两者都要在，缺前者用户不知道填什么，缺后者不知道错在哪。 */
  function row(labelText, control, helperText, required, notes) {
    return U.el('div', { class: 'form-row' }, [
      U.el('label', null, [
        U.el('span', { text: labelText }),
        required ? U.el('span', { class: 'req', text: ' *' }) : null
      ]),
      control,
      helperText ? U.el('p', { class: 'helper', text: helperText }) : null
    ].concat(notes || []));
  }

  /* itemsFor —— 取属于某字段的校验条目。
     字段自己从 ctx.result 里挑，而不是由表单逐个分发：新增一条校验只需
     在 rule-validate.js 里带上 field，无需同步改表单的分发逻辑。 */
  function itemsFor(ctx, field) {
    var r = (ctx && ctx.result) || { blocking: [], normalized: [], skipped: [] };
    return {
      blocking: (r.blocking || []).filter(function (b) { return b.field === field; }),
      normalized: (r.normalized || []).filter(function (n) { return n.field === field; }),
      skipped: (r.skipped || []).filter(function (s) {
        return (s.fields || []).indexOf(field) >= 0;
      })
    };
  }

  function hasError(ctx, field) {
    return itemsFor(ctx, field).blocking.length > 0;
  }

  /* fieldNotes —— 阻断错误红字 + 规范化提示中性字。
     规范化不是错误，绝不能标红：「留空将按 1000 落库」标红会让用户以为
     必须改，然后去填一个后端本来就会替他填好的值。 */
  function fieldNotes(ctx, field) {
    var it = itemsFor(ctx, field);
    var out = [];
    it.blocking.forEach(function (b) {
      out.push(U.el('p', { class: 'helper helper--err', role: 'alert',
        text: RV.message(b) }));
    });
    it.normalized.forEach(function (n) {
      out.push(U.el('p', { class: 'helper', text: n.text }));
    });
    /* 跳过项也要说出来。「本类型不校验这个字段」与「校验过了没问题」
       是两回事，后者会让人以为填错也无妨。 */
    it.skipped.forEach(function (s) {
      out.push(U.el('p', { class: 'helper', text: s.reason }));
    });
    return out;
  }

  /* seg —— 分段控件。
     opts.disabledFn 返回 true 时用 aria-disabled 而非 disabled：
     disabled 会把按钮移出 Tab 序，读屏用户既听不到它、也听不到
     aria-describedby 指向的原因，等于「不可用」这件事只有视觉用户知道。
     opts.describedBy 指向解释不可用原因的元素 id。 */
  function seg(options, current, onPick, opts) {
    opts = opts || {};
    var box = U.el('div', { class: 'seg', role: 'group' });
    options.forEach(function (o) {
      var off = opts.disabledFn ? !!opts.disabledFn(o.v) : false;
      box.appendChild(U.el('button', {
        type: 'button', text: o.t,
        'aria-pressed': current === o.v ? 'true' : 'false',
        'aria-disabled': off ? 'true' : null,
        'aria-describedby': off && opts.describedBy ? opts.describedBy : null,
        onclick: function () {
          if (off) return;   // aria-disabled 不会自动拦点击，必须显式短路
          onPick(o.v);
        }
      }));
    });
    return box;
  }

  /* windowField —— 预设窗口（optgroup 分族）+ 自定义。

     用原生 optgroup 分「滚动窗口 / 自然对齐」两族：两者的差别不是时长而是
     对齐语义（自然族受部署的 TZ_OFFSET_SECONDS 影响）。分族让这个差别在
     下拉展开那一刻可见，而不是选完之后再用一行说明去补救。 */
  function windowField(ctx) {
    var draft = ctx.draft;
    var preset = S.WINDOWS.indexOf(draft.window) >= 0;
    var groups = S.WINDOW_GROUPS.map(function (g) {
      return U.el('optgroup', { label: g.label },
        g.items.map(function (v) { return U.el('option', { value: v, text: v }); }));
    });
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
    }, groups.concat([U.el('option', { value: '__custom', text: '自定义…' })]));
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

  /* windowCaption —— 窗口语义说明，两行，均走中性色。

     不用 --c-warn：自然对齐是语义不是警告，染警告色会稀释该色在真正的
     临界预警里的信号价值。

     时区一律不显示任何数值或推算时刻 —— 前端无合法途径拿到运行时的
     TZ_OFFSET_SECONDS（无 policy key、snapshot 刻意不含该字段）。
     拿不到真值的量，展示它的**来源**而不是猜一个值。 */
  function windowCaption(draft) {
    var win = RV.parseWindow(draft.window);
    if (!win.ok) return null;
    var lines;
    if (win.natural) {
      var days = Math.floor(win.sec / 86400);
      lines = [
        '自然' + (win.unit === 'w' ? '周' : '日') + ' · 每个周期在业务时区零点重置，' +
          '不是滚动 ' + days + ' 天',
        '零点位置取决于部署的 TZ_OFFSET_SECONDS，改动需重启网关'
      ];
    } else {
      lines = [
        '滚动窗口 · 按绝对时间对齐，不受业务时区影响',
        '窗口长度 ' + win.sec + ' 秒'
      ];
    }
    return U.el('div', null, lines.map(function (t) {
      return U.el('p', { class: 'helper', text: t });
    }));
  }

  /* limitField —— 限额 + 实时换算。
     换算文本就地更新（不整体 repaint），否则每敲一个字符都重建整个表单，
     输入框会失焦。

     换算一律现算 parseWindow，不查 WINDOW_SECONDS 表：查表在 30s / 2h 这类
     合法但不在预设内的窗口上取不到键，换算文本会静默消失（原实现的真缺陷）。 */
  function limitField(ctx) {
    var draft = ctx.draft;
    function rateText() {
      var win = RV.parseWindow(draft.window);
      if (!win.ok || !isFinite(draft.limit) || draft.limit <= 0) return '';
      return '约 ' + (draft.limit / win.sec).toFixed(win.sec === 1 ? 0 : 1) +
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
    var noteId = 'rule-algo-conflict';

    /* 陷阱：不能无条件写 disabledFn: v => v==='token_bucket' && metric==='token'。
       编辑一条既有的非法规则时，token_bucket 正是当前选中项，禁用它会渲染出
       既 aria-pressed=true 又不可用的按钮 —— 用户被锁在非法状态里出不来。
       所以只在**尚未冲突**时预防性禁用；已冲突时两个 seg 都保持可点，
       靠下方的 danger note 指路。 */
    var algoDisabled = conflict ? null : function (v) {
      return v === 'token_bucket' && draft.metric === 'token';
    };
    var metricDisabled = conflict ? null : function (v) {
      return v === 'token' && draft.algorithm === 'token_bucket';
    };

    var out = [
      row('计量维度', seg(S.METRICS, draft.metric,
        function (v) { ctx.set('metric', v); },
        { disabledFn: metricDisabled, describedBy: noteId }),
        null, false, fieldNotes(ctx, 'metric')),
      row('时间窗口', windowField(ctx), null, true, fieldNotes(ctx, 'window')),
      windowCaption(draft),
      row('限额', limitField(ctx), null, true, fieldNotes(ctx, 'limit')),
      row('算法', seg(S.ALGOS, draft.algorithm,
        function (v) { ctx.set('algorithm', v); },
        { disabledFn: algoDisabled, describedBy: noteId }),
        conflict ? '令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定或滑动窗口' : null,
        false, fieldNotes(ctx, 'algorithm'))
    ];
    if (conflict) {
      out.push(U.el('div', { class: 'note note--danger', id: noteId, style: 'margin-bottom: var(--sp-4)' }, [
        U.icon('i-status-fail'),
        U.el('span', { text: '当前组合不可用：算法为令牌桶且计量为 Token。请改用固定窗口或滑动窗口。' })
      ]));
    } else {
      // 供 aria-describedby 引用：解释为何某个选项不可用。视觉上不占位。
      out.push(U.el('p', {
        id: noteId, class: 'helper',
        text: '令牌桶与 Token 计量不可同时使用，故其中一项会禁用另一项的对应选项。'
      }));
    }
    if (draft.algorithm === 'token_bucket') {
      out.push(row('突发容量 burst', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '0',
        value: String(draft.burst || 0), 'aria-label': '突发容量',
        oninput: function (ev) { draft.burst = parseInt(ev.target.value, 10) || 0; ctx.revalidate(); }
      }), '留空或 0 按限额处理。令牌桶在限额之上允许的瞬时突发量',
        false, fieldNotes(ctx, 'burst')));
    }
    return { nodes: out, conflict: conflict };
  }

  /* concurrencyFields —— 类型为 concurrency 时的字段组。

     后端 rule.go:129-137 检查完 max_concurrent 即 return nil，
     故 limit / window / metric / algorithm 四项**完全不校验**。
     这里整组不渲染（而非渲染后禁用）：渲染一个后端根本不读的字段，
     会让人以为它有用。 */
  function concurrencyFields(ctx) {
    var draft = ctx.draft;
    return [
      row('最大并发', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '1',
        value: String(draft.max_concurrent || 0), 'aria-label': '最大并发',
        oninput: function (ev) { draft.max_concurrent = parseInt(ev.target.value, 10); ctx.revalidate(); }
      }), null, true, fieldNotes(ctx, 'max_concurrent')),
      row('持有超时（秒）', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '0',
        value: String(draft.timeout || 0), 'aria-label': '持有超时秒数',
        oninput: function (ev) { draft.timeout = parseInt(ev.target.value, 10) || 0; ctx.revalidate(); }
      }), '留空或 0 取默认 ' + S.DEFAULTS.timeoutSec + 's。超时后并发槽位自动释放，' +
        '防止请求异常退出导致槽位泄漏', false, fieldNotes(ctx, 'timeout')),
      U.el('p', { class: 'helper', text: '并发类型不使用时间窗口、限额、算法与计量维度，故不显示这些字段。' })
    ];
  }

  /* watermarkField —— 水位告警阈值。后端 :251-257 对越界值静默规范化成 80，
     不阻断，所以越界只给中性提示、绝不标红：标红会让用户去改一个后端
     本来就会替他填好的值。 */
  function watermarkField(ctx) {
    var draft = ctx.draft;
    return row('水位告警阈值 %', U.el('input', {
      class: 'field field--lg field--mono', type: 'number', min: '1', max: '100',
      value: String(draft.watermark === undefined ? S.DEFAULTS.watermark : draft.watermark),
      'aria-label': '水位告警阈值百分比',
      oninput: function (ev) {
        draft.watermark = parseInt(ev.target.value, 10);
        ctx.revalidate();
      }
    }), '用量达到该百分比时在监控看板标记为接近上限', false,
      fieldNotes(ctx, 'watermark'));
  }

  /* dimensionField —— 维度多选 chips。
     选中 global 后其余维度立即禁用：global 表示不分维度的全局限流。 */
  function dimensionField(ctx) {
    var draft = ctx.draft;
    var list0 = draft.dimensions || [];
    var hasGlobal = list0.indexOf('global') >= 0;
    var chips = U.el('div', { class: 'chips' });
    S.DIMS.forEach(function (d) {
      var on = list0.indexOf(d) >= 0;
      chips.appendChild(U.el('button', {
        class: 'chip', type: 'button', text: d,
        'aria-pressed': on ? 'true' : 'false',
        disabled: hasGlobal && d !== 'global',
        onclick: function () {
          var list = (draft.dimensions || []).slice();
          if (d === 'global') {
            /* 反向排他：已选了具体维度时点 global 会清空它们。
               原实现静默清空，属数据丢失 —— 用户不会预期「加一个维度」
               的动作删掉已选的几个。故先确认。 */
            if (!on && list.length > 0) {
              var names = list.join('、');
              if (!w.confirm('global 是不分维度的全局限流，选择它会清除已选的 ' +
                  names + '。确定要切换吗？')) {
                return;
              }
            }
            list = on ? [] : ['global'];
          } else if (on) {
            list.splice(list.indexOf(d), 1);
          } else {
            list.push(d);
          }
          ctx.set('dimensions', list);
        }
      }));
    });
    return row('限流维度', chips,
      hasGlobal ? 'global 表示不分维度的全局限流，不能与具体维度组合'
        : '多选。维度组合决定计数键的粒度', true,
      fieldNotes(ctx, 'dimensions'));
  }

  w.RuleFields = {
    row: row, seg: seg,
    hasError: hasError, fieldNotes: fieldNotes,
    rateFields: rateFields, concurrencyFields: concurrencyFields,
    dimensionField: dimensionField, watermarkField: watermarkField,
    windowCaption: windowCaption
  };
})(window);
