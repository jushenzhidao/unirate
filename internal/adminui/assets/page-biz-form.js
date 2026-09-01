/* ============================================================================
   业务域本体表单（右侧抽屉）
   ----------------------------------------------------------------------------
   与规则表单分开：业务域改的是转发目标与开关（base_url / 前缀剥离 / 启用），
   规则改的是限流参数。两者的校验端点与失败后果都不同，混在一个表单里
   会让「我只是想停用这个域」和「我要改限额」共用一个保存按钮。

   publishFailedToast 放在本模块：202 分支两个表单都会遇到，且它属于
   「写入成功但发布失败」这一类业务域级别的结果处理。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  /* 202 saved_but_publish_failed 是后端真实分支：已落 SoT 但发布失败。
     绝不能当成功静默处理 —— 运维会以为配置已生效，而实际最长要等一个轮询周期。 */
  function publishFailedToast() {
    API.toast('warn', '已写入数据库，但配置发布失败。网关会在下次轮询（≤30s）拉取。' +
      '如需立即生效，去配置快照页手动重载。');
    var host = document.getElementById('toasts');
    if (host && host.lastChild) {
      host.lastChild.appendChild(U.el('button', {
        class: 'btn', type: 'button', style: 'margin-top: var(--sp-2)',
        onclick: function () { location.hash = '#/config'; }
      }, [U.icon('i-nav-config'), U.el('span', { text: '去配置快照页重载' })]));
    }
  }

  function openBiz(biz, onSaved) {
    var isNew = !biz;
    var draft = biz ? JSON.parse(JSON.stringify(biz)) : {
      biz: '', base_url: '', path_strip_prefix: true, enabled: true, rules: []
    };
    var body = U.el('div', { class: 'drawer-body' });
    var saveBtn = U.el('button', { class: 'btn btn--primary btn--lg', type: 'button' },
      U.el('span', { text: isNew ? '创建业务域' : '保存业务域' }));

    var nameInput = U.el('input', {
      class: 'field field--lg field--mono', type: 'text', value: draft.biz,
      autocomplete: 'off', spellcheck: 'false', disabled: !isNew,
      'aria-label': '业务域标识',
      oninput: function (ev) { draft.biz = ev.target.value.trim(); }
    });
    var urlInput = U.el('input', {
      class: 'field field--lg field--mono', type: 'text', value: draft.base_url || '',
      autocomplete: 'off', spellcheck: 'false', placeholder: 'http://upstream:9000',
      'aria-label': '上游 base_url',
      oninput: function (ev) { draft.base_url = ev.target.value.trim(); }
    });

    U.append(body, [
      U.el('div', { class: 'form-row' }, [
        U.el('label', null, [U.el('span', { text: '业务域标识' }), U.el('span', { class: 'req', text: ' *' })]),
        nameInput,
        U.el('p', {
          class: 'helper',
          text: isNew
            ? '路径首段即业务域，例如 openai 对应 POST /openai/v1/chat/completions。创建后不可改名'
            : '业务域标识创建后不可修改'
        })
      ]),
      U.el('div', { class: 'form-row' }, [
        U.el('label', null, [
          U.el('span', { text: '上游 base_url' }),
          draft.biz === '*' ? null : U.el('span', { class: 'req', text: ' *' })
        ]),
        urlInput,
        U.el('p', { class: 'helper', text: '* 兜底域可留空，它只承载限流规则、不转发流量' })
      ]),
      U.el('div', { class: 'form-row' }, U.el('label', { class: 'switch' }, [
        U.el('input', {
          type: 'checkbox', checked: draft.path_strip_prefix ? true : false,
          onchange: function (ev) { draft.path_strip_prefix = ev.target.checked; }
        }),
        U.el('span', { text: '转发时剥离路径首段（业务域前缀）' })
      ])),
      U.el('div', { class: 'form-row' }, U.el('label', { class: 'switch' }, [
        U.el('input', {
          type: 'checkbox', checked: draft.enabled ? true : false,
          onchange: function (ev) { draft.enabled = ev.target.checked; }
        }),
        U.el('span', { text: '启用该业务域' })
      ])),
      U.el('div', { class: 'note' }, [
        U.icon('i-info'),
        U.el('span', {
          text: isNew
            ? '创建后在列表中展开该行即可新增限流规则。'
            : '该域现有 ' + ((draft.rules || []).length) + ' 条规则，保存业务域不会改动它们。'
        })
      ])
    ]);

    saveBtn.addEventListener('click', function () {
      // 这两条与后端 upsertBiz 的前置校验一致，前端先拦是为了少一次往返
      if (!draft.biz) { API.toast('warn', '业务域标识必填'); nameInput.focus(); return; }
      if (!draft.base_url && draft.biz !== '*') {
        API.toast('warn', '除 * 兜底域外，base_url 必填'); urlInput.focus(); return;
      }
      // 后端只校验非空，形如 "xx" 的值能存进去，但转发时必然失败且只在运行期报错。
      // 在这里拦掉，让错误停在配置期。
      if (draft.base_url && !/^https?:\/\/[^\s/]+/.test(draft.base_url)) {
        API.toast('warn', 'base_url 需以 http:// 或 https:// 开头，例如 https://api.openai.com/v1');
        urlInput.focus(); return;
      }
      saveBtn.disabled = true;
      U.clear(saveBtn);
      U.append(saveBtn, [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '保存中' })]);
      API.api.upsertBiz({
        biz: draft.biz, base_url: draft.base_url || '',
        path_strip_prefix: !!draft.path_strip_prefix, enabled: !!draft.enabled,
        // 保留原有规则：这个表单不碰规则，漏传会把它们全删掉
        rules: draft.rules || [], token_metering: draft.token_metering || null
      }).then(function (data) {
        close();
        if (API.isPublishFailed(data)) {
          publishFailedToast();
          onSaved && onSaved(draft.biz, true);
        } else {
          API.toast('ok', '业务域 ' + draft.biz + ' 已保存，配置版本 ' + (data && data.config_version));
          onSaved && onSaved(draft.biz, false);
        }
      }, function (err) {
        saveBtn.disabled = false;
        U.clear(saveBtn);
        U.append(saveBtn, U.el('span', { text: isNew ? '创建业务域' : '保存业务域' }));
        API.toast('danger', '保存业务域失败：' + err.message);
      });
    });

    var close = w.App.drawer({
      title: isNew ? '新增业务域' : '编辑业务域 · ' + draft.biz,
      body: body,
      foot: [U.el('span', { class: 'spacer' }), U.el('button', {
        class: 'btn btn--lg', type: 'button', text: '取消', onclick: function () { close(); }
      }), saveBtn]
    }, isNew ? nameInput : urlInput);
  }

  w.BizForm = { openBiz: openBiz, publishFailedToast: publishFailedToast };
})(window);
