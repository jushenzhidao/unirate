/* ============================================================================
   规则表单（右侧抽屉，宽 520px）—— DESIGN.md §5.3
   ----------------------------------------------------------------------------
   用抽屉不用 modal：编辑规则时需要对照左侧列表里其他规则的限额。

   本次改动的核心是**校验前移**：本地 rule-validate.js 逐位镜像后端
   internal/limiter/rule.go 的 Validate()，字段一变即出结论（同步，0ms），
   后端 POST /admin/rules/validate 保持 400ms debounce 作为终审，不下线。

   三档节流（Spec §6），三个档位对应三种代价：
     - 本地校验    0ms   纯函数，微秒级，任何延迟都是白等
     - 键/JSON预览 rAF   要写 DOM，合并到同一帧，避免连打时反复布局
     - 后端复核    400ms 走网络，且它只是复核，不该抢在本地结论前面

   重绘纪律：输入时**绝不**重建输入框所在的 DOM，否则光标丢失、输入法候选
   被打断。只有改变字段可见性的操作（切类型 / 切算法 / 预设⇄自定义窗口）
   才重建结构 —— 那些操作本来就不是在输入框里连打。因此 body 分三块：
   fields 会被重建，notices 与 preview 各自就地更新，互不牵连。

   字段控件在 rule-fields.js；键预览在 rule-preview.js；
   业务域本体表单在 page-biz-form.js。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API, F = w.RuleFields, V = w.RuleValidate, P = w.RulePreview;
  var DEBOUNCE = 400;

  /* 字段名 → 控件的 aria-label。用于把本地校验结论回写成 aria-invalid。
     控件由 rule-fields.js 创建，表单侧只能按 label 反查 —— 这比让两个文件
     互传节点引用更松耦合，代价是 label 改名会静默失效，故集中在此一处。 */
  var FIELD_LABEL = {
    limit: ['限额'],
    window: ['时间窗口', '自定义时间窗口'],
    burst: ['突发容量'],
    max_concurrent: ['最大并发'],
    timeout: ['持有超时秒数']
  };

  /* 字段名 → 中文名。错误集中展示时要说清是哪个字段，
     只给一句「必须大于 0」在有五个数字输入框的表单里等于没说。 */
  var FIELD_NAME = {
    name: '规则名', limit: '限额', window: '时间窗口', burst: '突发容量',
    metric: '计量维度', algorithm: '算法', dimensions: '限流维度',
    max_concurrent: '最大并发', timeout: '持有超时', watermark: '水位阈值',
    type: '类型'
  };

  function defaultRule() {
    return {
      name: '', type: 'rate', metric: 'request', dimensions: ['biz'],
      window: '1m', limit: 1000, algorithm: 'fixed_window',
      burst: 0, watermark: 80, max_concurrent: 50, timeout: 0, enabled: true
    };
  }

  function ruleEnabled(r) { return r.enabled === null || r.enabled === undefined || r.enabled === true; }

  /* open —— 编辑或新增 biz 下的一条规则。
     保存走 POST /admin/bizs（整份 biz upsert），因此必须带上该 biz 的其余规则，
     否则会把没在编辑的规则一并删掉。 */
  function open(biz, rule, onSaved) {
    var isNew = !rule;
    var idx = isNew ? (biz.rules || []).length : (biz.rules || []).indexOf(rule);
    var draft = JSON.parse(JSON.stringify(rule || defaultRule()));
    if (draft.enabled === null || draft.enabled === undefined) draft.enabled = true;

    var result = V.validate(draft);   // 本地结论，每次改动同步重算
    var backendErrors = [];           // 后端驳回本条规则的原始英文错误
    var drift = [];                   // 后端报了而本地没覆盖的 —— 镜像漂移
    var timer = null, rafId = null;

    var summary = U.el('div', { class: 'note', style: 'flex:1 1 auto' });
    var saveBtn = U.el('button', { class: 'btn btn--primary btn--lg', type: 'button' },
      U.el('span', { text: isNew ? '新增规则' : '保存规则' }));
    var body = U.el('div', { class: 'drawer-body' });
    var fields = U.el('div', { class: 'stack' });      // 结构变化时整体重建
    var notices = U.el('div', { class: 'stack' });     // 校验结论，就地更新
    var preview = U.el('div', { class: 'stack' });     // 只读预览，就地更新

    function siblingRules() {
      var all = (biz.rules || []).slice();
      if (isNew) all.push(draft); else all[idx] = draft;
      return all;
    }

    function setSummary(kind, iconId, text) {
      summary.className = 'note' + (kind ? ' note--' + kind : '');
      U.clear(summary);
      U.append(summary, [U.icon(iconId), U.el('span', { text: text })]);
    }

    /* 本地结论决定能否保存。后端只能把本地放行的再否掉，不能把本地否掉的放行 ——
       本地更严就是缺陷（会拦住后端本会接受的配置），那种情况由 drift 暴露。 */
    function localBlocked() { return result.blocking.length > 0; }
    function syncSaveState() {
      saveBtn.disabled = localBlocked() || backendErrors.length > 0;
    }

    /* markInvalid —— 把本地阻断结论回写到控件的 aria-invalid。
       只加不减：每次都从全量控件重置，避免上一帧的红框留在已修好的字段上。 */
    function markInvalid() {
      var bad = {};
      result.blocking.forEach(function (b) { bad[b.field] = true; });
      Object.keys(FIELD_LABEL).forEach(function (field) {
        FIELD_LABEL[field].forEach(function (label) {
          var node = fields.querySelector('[aria-label="' + label + '"]');
          if (node) node.setAttribute('aria-invalid', bad[field] ? 'true' : 'false');
        });
      });
    }

    /* renderNotices —— 阻断 / 规范化 / 跳过 / 后端 / 漂移，五类，顺序即优先级。

       跳过项必须显式说出来。「这个字段当前不参与校验」和「这个字段校验过了
       没问题」是两回事，把前者显示成后者会让人以为填错也无所谓。 */
    function renderNotices() {
      U.clear(notices);

      if (result.blocking.length) {
        notices.appendChild(U.el('div', { class: 'note note--danger', role: 'alert' }, [
          U.icon('i-status-fail'),
          U.el('div', null, result.blocking.map(function (b) {
            var who = FIELD_NAME[b.field] || b.field;
            return U.el('div', { text: who + '：' + V.message(b) });
          }))
        ]));
      }

      result.normalized.forEach(function (n) {
        notices.appendChild(U.el('div', { class: 'note' }, [
          U.icon('i-info'),
          U.el('span', { text: (FIELD_NAME[n.field] || n.field) + '：' + n.text })
        ]));
      });

      result.skipped.forEach(function (s) {
        var who = s.fields.map(function (f) { return FIELD_NAME[f] || f; }).join('、');
        notices.appendChild(U.el('div', { class: 'note' }, [
          U.icon('i-info'),
          U.el('span', { text: s.reason + '（' + who + '）' })
        ]));
      });

      if (backendErrors.length) {
        notices.appendChild(U.el('div', { class: 'note note--danger', role: 'alert' }, [
          U.icon('i-status-fail'),
          U.el('div', null, [U.el('div', { text: '网关校验未通过：' })].concat(
            backendErrors.map(function (e) {
              return U.el('div', { text: API.translateRuleError(e) });
            })))
        ]));
      }

      /* 漂移告警：后端报错而本地放行 = 本地镜像落后于 rule.go。
         不静默吞掉 —— 否则下次 rule.go 新增校验而前端没跟进时，界面会一直
         说「通过」但保存永远失败，且没有任何线索指向该改哪个文件。 */
      if (drift.length) {
        notices.appendChild(U.el('div', { class: 'note note--warn' }, [
          U.icon('i-status-warn'),
          U.el('span', {
            text: '网关报出了本地校验未覆盖的问题，前端校验规则可能落后于网关版本；' +
              '请以网关结论为准，并将此情况反馈给维护者。'
          })
        ]));
      }
    }

    function renderPreview() {
      U.clear(preview);
      preview.appendChild(U.el('p', { class: 'helper', text: '计数键预览' }));
      preview.appendChild(P.render(draft, biz.biz));
    }

    /* ---- 三档节流 -------------------------------------------------------- */

    // 第 1 档：本地校验，同步。纯函数，没有等待的理由。
    function recompute() {
      result = V.validate(draft);
      ctx.result = result;
      syncSaveState();
    }

    // 第 2 档：写 DOM 的部分合并到同一帧。预览是只读信息，晚一帧无影响。
    function scheduleRepaint() {
      if (rafId) return;
      var raf = w.requestAnimationFrame ? w.requestAnimationFrame.bind(w)
        : function (fn) { return w.setTimeout(fn, 16); };
      rafId = raf(function () {
        rafId = null;
        markInvalid();
        renderNotices();
        renderPreview();
      });
    }

    // 第 3 档：后端复核。
    function scheduleBackend() {
      if (timer) w.clearTimeout(timer);
      timer = w.setTimeout(backendValidate, DEBOUNCE);
    }

    function localSummary() {
      if (localBlocked()) {
        setSummary('danger', 'i-status-fail',
          '本条规则有 ' + result.blocking.length + ' 项问题待修正');
        return;
      }
      var extra = [];
      if (result.normalized.length) extra.push(result.normalized.length + ' 项将被规范化');
      if (result.skipped.length) extra.push(result.skipped.length + ' 组字段本类型不校验');
      setSummary(null, 'i-spinner',
        '本地校验通过' + (extra.length ? '（' + extra.join('、') + '）' : '') + '，正在向网关复核');
    }

    /* revalidate —— 输入时调用。不重建结构，只重算 + 就地更新。 */
    function revalidate() {
      recompute();
      scheduleRepaint();
      /* 本地不通过就不发请求（Spec §9 验收 1）：后端会报同样的问题，
         多一次往返只会让「校验中」的转圈盖住已经给出的本地结论。 */
      if (localBlocked()) {
        if (timer) { w.clearTimeout(timer); timer = null; }
        backendErrors = [];
        drift = [];
        localSummary();
        return;
      }
      localSummary();
      scheduleBackend();
    }

    /* backendValidate —— 终审。只有本地放行才会走到这里。 */
    function backendValidate() {
      var rules = siblingRules();
      API.api.validateRules(rules).then(function (data) {
        backendErrors = [];
        drift = [];
        setSummary('ok', 'i-check',
          '校验通过：' + ((data && data.checked) || rules.length) + ' 条规则');
        syncSaveState();
        scheduleRepaint();
      }, function (err) {
        var problems = (err.payload && err.payload.problems) || [];
        var mine = [], others = 0;
        problems.forEach(function (p) {
          // index 是规则数组下标，按它回填到对应规则；非本条的单独计数
          if (parseInt(p.index, 10) === idx) mine.push(p.error);
          else others++;
        });

        if (mine.length === 0 && problems.length === 0) {
          backendErrors = [];
          drift = [];
          setSummary('danger', 'i-status-fail', '校验请求失败：' + err.message);
          syncSaveState();
          scheduleRepaint();
          return;
        }

        backendErrors = mine;

        /* 漂移判定用「集合覆盖」而非「首错相等」：后端遇错即返回单条，
           本地并行报多条，用相等判定会把完全正常的情况判成不一致。
           只有当后端某条错误不被本地任何一条覆盖时，才算漂移。
           形参顺序是 (result, translatedMsg)，且第二参必须是**已翻译**的中文 ——
           backendErrorCovered 内部拿 message(result) 的中文结论去比串，
           喂英文原文会一条都比不上，把每条后端错误都误判成漂移。 */
        drift = mine
          .map(function (e) { return API.translateRuleError(e); })
          .filter(function (m) { return !V.backendErrorCovered(result, m); });

        var msg = '网关驳回本条规则的 ' + mine.length + ' 项问题';
        if (mine.length === 0) msg = '本条规则通过';
        if (others > 0) msg += '，该业务域另有 ' + others + ' 条已存在的规则不合法';
        setSummary('danger', 'i-status-fail', msg);
        syncSaveState();
        scheduleRepaint();
      });
    }

    /* ctx 交给 rule-fields.js：
         set        —— 改变字段可见性的值，需重建结构
         revalidate —— 输入类改动，只重算 + 就地更新，绝不重建（否则失焦）
         repaint    —— 显式要求重建结构
         result     —— 本帧结论，供字段控件按需自取 */
    var ctx = {
      draft: draft,
      result: result,
      set: function (key, val) { draft[key] = val; rebuild(); },
      revalidate: revalidate,
      repaint: function () { rebuild(); }
    };

    /* rebuild —— 重建字段区。只在字段可见性可能变化时调用。
       作用域收窄到 fields：notices 与 preview 不在其中，各自就地更新，
       所以重建结构不会让它们闪烁。 */
    function rebuild() {
      recompute();
      U.clear(fields);

      U.append(fields, [
        F.row('规则名', U.el('input', {
          class: 'field field--lg field--mono', type: 'text', value: draft.name || '',
          autocomplete: 'off', spellcheck: 'false', 'aria-label': '规则名',
          oninput: function (ev) { draft.name = ev.target.value; revalidate(); }
        }), null, true, F.fieldNotes(ctx, 'name')),
        F.row('类型', F.seg([{ v: 'rate', t: '速率 rate' }, { v: 'concurrency', t: '并发 concurrency' }],
          draft.type, function (v) { ctx.set('type', v); }))
      ]);

      if (draft.type === 'concurrency') {
        U.append(fields, F.concurrencyFields(ctx));
      } else {
        var rate = F.rateFields(ctx);
        U.append(fields, rate.nodes);
        // 令牌桶 + Token 是硬禁用组合，后端也会拒
        if (rate.conflict) saveBtn.disabled = true;
      }

      fields.appendChild(F.dimensionField(ctx));
      fields.appendChild(F.watermarkField(ctx));

      fields.appendChild(U.el('div', { class: 'form-row' },
        U.el('label', { class: 'switch' }, [
          U.el('input', {
            type: 'checkbox', checked: ruleEnabled(draft) ? true : false,
            onchange: function (ev) { draft.enabled = ev.target.checked; revalidate(); }
          }),
          U.el('span', { text: '启用该规则' })
        ])));

      // 结构变了，本地结论要立刻反映到新控件上，不能等下一帧
      markInvalid();
      renderNotices();
      renderPreview();
      if (localBlocked()) {
        if (timer) { w.clearTimeout(timer); timer = null; }
        backendErrors = [];
        drift = [];
        localSummary();
      } else {
        localSummary();
        scheduleBackend();
      }
    }

    saveBtn.addEventListener('click', function () {
      saveBtn.disabled = true;
      U.clear(saveBtn);
      U.append(saveBtn, [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '保存中' })]);
      API.api.upsertBiz({
        biz: biz.biz, base_url: biz.base_url || '',
        path_strip_prefix: !!biz.path_strip_prefix, enabled: !!biz.enabled,
        rules: siblingRules(), token_metering: biz.token_metering || null
      }).then(function (data) {
        close();
        if (API.isPublishFailed(data)) {
          w.BizForm.publishFailedToast();
          onSaved && onSaved(biz.biz, true);
        } else {
          API.toast('ok', '规则已保存，配置版本 ' + (data && data.config_version) +
            '，' + biz.biz + ' 共 ' + ((data && data.rules) || 0) + ' 条规则');
          onSaved && onSaved(biz.biz, false);
        }
      }, function (err) {
        U.clear(saveBtn);
        U.append(saveBtn, U.el('span', { text: isNew ? '新增规则' : '保存规则' }));
        saveBtn.disabled = false;
        API.toast('danger', '保存规则失败：' + err.message);
      });
    });

    U.append(body, [fields, notices, preview]);
    rebuild();

    var close = w.App.drawer({
      title: isNew ? '新增规则 · ' + biz.biz : '编辑规则 · ' + (rule.name || ''),
      body: body,
      foot: [summary, U.el('button', {
        class: 'btn btn--lg', type: 'button', text: '取消',
        onclick: function () {
          // 抽屉关掉后待发的请求已无意义，回调里还会去改已卸载的节点
          if (timer) w.clearTimeout(timer);
          close();
        }
      }), saveBtn]
    });
  }

  w.RulesForm = { open: open };
})(window);
