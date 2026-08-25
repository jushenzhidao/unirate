/* ============================================================================
   浮层：抽屉与模态的焦点管理
   ----------------------------------------------------------------------------
   Esc 关闭 + 焦点陷阱 + 关闭后焦点归还触发元素。
   焦点归还是最容易漏的一条：抽屉关掉后若不归还，键盘用户的焦点会落回
   document.body，得从头 Tab 一遍才能回到刚才那一行。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U;

  var FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),' +
    'textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

  /* trap(container, initial) → close()
     返回的 close 是幂等的：scrim 点击、Esc、按钮回调可能都会调它。 */
  function trap(container, initial) {
    var returnTo = document.activeElement;

    function onKey(ev) {
      if (ev.key === 'Escape') { ev.preventDefault(); close(); return; }
      if (ev.key !== 'Tab') return;
      var nodes = Array.prototype.filter.call(
        container.querySelectorAll(FOCUSABLE),
        function (n) { return n.offsetParent !== null || n === document.activeElement; });
      if (nodes.length === 0) return;
      var first = nodes[0], last = nodes[nodes.length - 1];
      // 循环焦点：到边界时绕回，Tab 不会跑到浮层外面的页面上
      if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
      else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
    }

    // 捕获阶段监听：确保浮层内的组件无法先吞掉 Esc
    document.addEventListener('keydown', onKey, true);
    var target = initial || container.querySelector(FOCUSABLE);
    if (target) w.setTimeout(function () { target.focus(); }, 0);

    var closed = false;
    function close() {
      if (closed) return; // 幂等：重复调用不再解绑第二次
      closed = true;
      document.removeEventListener('keydown', onKey, true);
      if (container.parentNode) container.parentNode.removeChild(container);
      if (returnTo && returnTo.focus) returnTo.focus();
    }
    return close;
  }

  function host() { return document.getElementById('overlay-host'); }

  /* overlay(modalNode) —— 居中模态，用于确认类操作 */
  function overlay(modalNode, initialFocus) {
    var box = U.el('div');
    var scrim = U.el('div', { class: 'scrim' });
    box.appendChild(scrim);
    box.appendChild(U.el('div', { class: 'modal-wrap' }, modalNode));
    host().appendChild(box);
    var close = trap(box, initialFocus);
    scrim.addEventListener('click', function () { close(); });
    return close;
  }

  /* drawer(opts) —— 右侧抽屉。规则表单用它而非 modal：
     编辑规则时需要对照左侧列表里其他规则的限额。 */
  function drawer(opts, initialFocus) {
    var box = U.el('div');
    var scrim = U.el('div', { class: 'scrim' });
    var closeBtn = U.el('button', {
      class: 'btn btn--ghost btn--icon', type: 'button', 'aria-label': '关闭'
    }, U.icon('i-close'));
    var panel = U.el('aside', {
      class: 'drawer', role: 'dialog', 'aria-modal': 'true', 'aria-label': opts.title
    }, [
      U.el('div', { class: 'drawer-head' }, [
        U.el('span', { class: 'drawer-title', text: opts.title }),
        U.el('span', { class: 'spacer' }),
        closeBtn
      ]),
      opts.body,
      U.el('div', { class: 'drawer-foot' }, opts.foot || [])
    ]);
    box.appendChild(scrim);
    box.appendChild(panel);
    host().appendChild(box);
    var close = trap(box, initialFocus);
    closeBtn.addEventListener('click', function () { close(); });
    scrim.addEventListener('click', function () { close(); });
    return close;
  }

  w.Overlay = { overlay: overlay, drawer: drawer };
})(window);
