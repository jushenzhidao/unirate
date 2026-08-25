/* ============================================================================
   规则表单（右侧抽屉，宽 520px）—— DESIGN.md §5.3
   ----------------------------------------------------------------------------
   用抽屉不用 modal：编辑规则时需要对照左侧列表里其他规则的限额。
   实时校验对接 POST /admin/rules/validate，字段变化后 400ms debounce，
   按 problems[].index 把 error 回填到对应规则的字段级错误。

   字段组构造在 rule-fields.js；业务域本体表单在 page-biz-form.js。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API, F = w.RuleFields;
  var DEBOUNCE = 400;

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

    var errors = {};
    var timer = null;
    var summary = U.el('div', { class: 'note', style: 'flex:1 1 auto' });
    var saveBtn = U.el('button', { class: 'btn btn--primary btn--lg', type: 'button' },
      U.el('span', { text: isNew ? '新增规则' : '保存规则' }));
    var body = U.el('div', { class: 'drawer-body' });

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

    function validate() {
      setSummary(null, 'i-spinner', '校验中');
      var rules = siblingRules();
      API.api.validateRules(rules).then(function (data) {
        errors = {};
        setSummary('ok', 'i-check', '校验通过：' + ((data && data.checked) || rules.length) + ' 条规则');
        saveBtn.disabled = false;
        paint();
      }, function (err) {
        errors = {};
        var problems = (err.payload && err.payload.problems) || [];
        var mine = [], others = 0;
        problems.forEach(function (p) {
          // index 是规则数组下标，按它回填到对应规则；非本条的单独计数
          if (parseInt(p.index, 10) === idx) mine.push(API.translateRuleError(p.error));
          else others++;
        });
        if (mine.length) errors.rule = mine;
        if (mine.length === 0 && problems.length === 0) {
          setSummary('danger', 'i-status-fail', '校验请求失败：' + err.message);
        } else {
          var msg = mine.length ? '本条规则有 ' + mine.length + ' 项问题' : '本条规则通过';
          if (others > 0) msg += '，该业务域另有 ' + others + ' 条已存在的规则不合法';
          setSummary('danger', 'i-status-fail', msg);
        }
        // 校验未通过时禁用保存，但不拦截继续编辑
        saveBtn.disabled = mine.length > 0;
        paint();
      });
    }

    function revalidate() {
      if (timer) w.clearTimeout(timer);
      timer = w.setTimeout(validate, DEBOUNCE);
    }

    // ctx 交给 rule-fields.js：set 会重绘（值影响其他字段的可见性），
    // revalidate 只触发校验（不重绘，否则输入框会失焦）
    var ctx = {
      draft: draft,
      set: function (key, val) { draft[key] = val; revalidate(); paint(); },
      revalidate: revalidate,
      repaint: function () { revalidate(); paint(); }
    };

    function paint() {
      U.clear(body);
      U.append(body, [
        F.row('规则名', U.el('input', {
          class: 'field field--lg field--mono', type: 'text', value: draft.name || '',
          autocomplete: 'off', spellcheck: 'false',
          'aria-invalid': errors.rule ? 'true' : 'false',
          oninput: function (ev) { draft.name = ev.target.value; revalidate(); }
        }), null, true),
        F.row('类型', F.seg([{ v: 'rate', t: '速率 rate' }, { v: 'concurrency', t: '并发 concurrency' }],
          draft.type, function (v) { ctx.set('type', v); }))
      ]);

      if (draft.type === 'concurrency') {
        U.append(body, F.concurrencyFields(ctx));
      } else {
        var rate = F.rateFields(ctx);
        U.append(body, rate.nodes);
        // 令牌桶 + Token 是硬禁用组合，前端先拦一道（后端也会拒）
        if (rate.conflict) saveBtn.disabled = true;
      }

      body.appendChild(F.dimensionField(ctx));
      body.appendChild(F.row('水位告警阈值 %', U.el('input', {
        class: 'field field--lg field--mono', type: 'number', min: '0', max: '100',
        value: String(draft.watermark === undefined ? 80 : draft.watermark),
        'aria-label': '水位告警阈值百分比',
        oninput: function (ev) { draft.watermark = parseInt(ev.target.value, 10); revalidate(); }
      }), '用量达到该百分比时在监控看板标记为接近上限'));

      body.appendChild(U.el('div', { class: 'form-row' },
        U.el('label', { class: 'switch' }, [
          U.el('input', {
            type: 'checkbox', checked: ruleEnabled(draft) ? true : false,
            onchange: function (ev) { draft.enabled = ev.target.checked; revalidate(); }
          }),
          U.el('span', { text: '启用该规则' })
        ])));

      if (errors.rule) {
        body.appendChild(U.el('div', { class: 'note note--danger' }, [
          U.icon('i-status-fail'),
          U.el('div', null, errors.rule.map(function (m) { return U.el('div', { text: m }); }))
        ]));
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

    setSummary(null, 'i-info', '修改字段后自动校验');
    paint();
    var close = w.App.drawer({
      title: isNew ? '新增规则 · ' + biz.biz : '编辑规则 · ' + (rule.name || ''),
      body: body,
      foot: [summary, U.el('button', {
        class: 'btn btn--lg', type: 'button', text: '取消',
        onclick: function () { close(); }
      }), saveBtn]
    });
    validate();
  }

  w.RulesForm = { open: open };
})(window);
