/* ============================================================================
   登录页 #/login —— DESIGN.md §5.1
   ----------------------------------------------------------------------------
   提交动作：以输入的 token 试探 GET /admin/snapshot。200 才落 sessionStorage。

   四种失败必须分开说清楚，这是本页最重要的一条：
     401 → 令牌错
     403 → CIDR 拦截（令牌可能是对的，来源 IP 不在白名单）
     503 → 配置存储未就绪（令牌可能是对的，网关还在 bootstrap）
     网络失败 → 管理面根本不可达
   把这四种合并成「登录失败」会让运维在最需要线索的时候拿不到线索：
   403 要去改 admin.allow_cidrs，401 要去核对 ADMIN_TOKEN，两件完全不同的事。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  function setError(msg, kind, retry) {
    var box = document.getElementById('auth-error');
    U.clear(box);
    var input = document.getElementById('auth-token');
    if (!msg) {
      input.setAttribute('aria-invalid', 'false');
      return;
    }
    box.appendChild(U.el('div', {
      class: 'note note--' + (kind || 'danger'), style: 'margin-top: var(--sp-2)'
    }, [
      U.icon(kind === 'warn' ? 'i-status-warn' : 'i-status-fail'),
      U.el('div', null, [
        U.el('div', { text: msg }),
        retry ? U.el('button', {
          class: 'btn', type: 'button', style: 'margin-top: var(--sp-2)', onclick: retry
        }, [U.icon('i-refresh'), U.el('span', { text: '重试' })]) : null
      ])
    ]));
    // 令牌错才标红字段；503 / 网络失败不是用户输错，标红会误导
    input.setAttribute('aria-invalid', kind === 'warn' ? 'false' : 'true');
  }

  function submit(ev, onSuccess) {
    if (ev) ev.preventDefault();
    var input = document.getElementById('auth-token');
    var btn = document.getElementById('auth-submit');
    var token = input.value.trim();
    if (!token) { setError('请输入 Admin Token。', 'warn'); input.focus(); return; }

    setError(null);
    btn.disabled = true;
    U.clear(btn);
    U.append(btn, [U.icon('i-spinner', { class: 'spin' }), U.el('span', { text: '验证中' })]);

    function done() {
      btn.disabled = false;
      U.clear(btn);
      btn.appendChild(U.el('span', { text: '进入控制台' }));
    }

    // silent401 避免触发全局会话失效跳转 —— 这里 401 是「令牌输错了」，
    // 不是「会话过期」，两者的用户处置完全不同
    API.api.snapshot({ token: token, silent401: true }).then(function (snap) {
      API.session.setToken(token);
      API.session.setOperator(document.getElementById('auth-operator').value.trim());
      // 立刻清空输入框：令牌不该在 DOM 里多留一秒
      input.value = '';
      done();
      onSuccess(snap);
    }, function (err) {
      done();
      if (err.kind === 'network') {
        setError('无法连接管理面（' + location.origin + '）。确认 admin 端口可达。',
          'danger', function () { submit(null, onSuccess); });
      } else if (err.status === 401) {
        setError('令牌无效。请核对部署配置中的 ADMIN_TOKEN。');
      } else if (err.status === 403) {
        setError('当前来源 IP 不在 Admin 白名单内（admin.allow_cidrs）。');
      } else if (err.status === 503) {
        setError('管理面已启动，但配置存储未就绪。令牌可能正确，稍后重试。',
          'warn', function () { submit(null, onSuccess); });
      } else {
        setError('登录失败：' + err.message);
      }
    });
  }

  /* bindReveal 明文切换。图标与 aria-label 必须同步翻转，
     否则读屏用户听到的状态和实际相反。 */
  function bindReveal() {
    var reveal = document.getElementById('auth-reveal');
    reveal.addEventListener('click', function () {
      var input = document.getElementById('auth-token');
      var toText = input.type === 'password';
      input.type = toText ? 'text' : 'password';
      U.clear(reveal);
      reveal.appendChild(U.icon(toText ? 'i-eye-off' : 'i-eye'));
      reveal.setAttribute('aria-label', toText ? '隐藏令牌明文' : '显示令牌明文');
      input.focus();
    });
  }

  w.PageLogin = { submit: submit, setError: setError, bindReveal: bindReveal };
})(window);
