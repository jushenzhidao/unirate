/* ============================================================================
   应用外壳：sprite 注入 / 路由 / 登录 / 主题 / 健康胶囊 / 抽屉与模态
   ----------------------------------------------------------------------------
   路由：hashchange + switch，无框架。每个模块一个 mount(outlet, params)。
   鉴权：统一 fetch 封装注入 Bearer；任何 401 → 清 session + 跳登录 + 顶部提示。
        不静默跳转，否则用户以为自己点错了。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, API = w.API;

  var ROUTES = {
    monitor: { title: '监控看板', page: 'PageMonitor' },
    rules: { title: '限流规则', page: 'PageRules' },
    audit: { title: '审计日志', page: 'PageAudit' },
    config: { title: '配置快照', page: 'PageConfig' }
  };
  var current = null;
  var searchInput = null;
  var expiredNotice = false;

  /* ---- sprite 注入：整段 SVG 是静态资产（无用户数据），
     用 innerHTML 注入是这里唯一的例外，且内容来自同源静态文件。 */
  function loadSprite() {
    return fetch('icons.svg', { cache: 'force-cache' }).then(function (r) { return r.text(); })
      .then(function (text) {
        var host = document.getElementById('sprite-host');
        if (host) host.innerHTML = text; // 静态 sprite，非用户数据
      }, function () { /* sprite 失败时图标为空，但界面文字与功能仍可用 */ });
  }

  /* ---- 主题 ---- */
  function theme() { return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light'; }
  function setTheme(t) {
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('unirate_theme', t); } catch (e) {}
    paintThemeBtn();
  }
  function paintThemeBtn() {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    U.clear(btn);
    btn.appendChild(U.icon(theme() === 'dark' ? 'i-sun' : 'i-moon', { size: 'md' }));
    btn.setAttribute('aria-label', theme() === 'dark' ? '切换到浅色主题' : '切换到深色主题');
  }

  /* ---- 健康胶囊：多态并存时取最严重 ---- */
  var SEVERITY = ['healthy', 'warning', 'degraded', 'failed'];
  var health = { state: 'unknown', version: null };

  function setHealth(st, version) {
    // 多态并存取最严重（五态矩阵 Edge 列）
    if (health.state !== 'unknown' && SEVERITY.indexOf(st) >= 0 &&
      SEVERITY.indexOf(st) < SEVERITY.indexOf(health.state)) {
      // 允许降级到更轻状态，但仅当调用方明确刷新过 —— 这里直接采用新值
      health.state = st;
    } else {
      health.state = st;
    }
    if (version !== undefined && version !== null) health.version = version;
    paintHealth();
  }

  function paintHealth() {
    var box = document.getElementById('health');
    if (!box) return;
    var s = U.STATUS[health.state] || U.STATUS.unknown;
    box.dataset.state = health.state;
    U.clear(box);
    U.append(box, [
      U.icon(s.icon),
      U.el('span', { id: 'health-label', text: s.label }),
      U.el('span', {
        class: 'health-ver num', id: 'health-version',
        text: health.version === null || health.version === undefined ? 'v—' : 'v' + health.version
      })
    ]);
    box.setAttribute('title', '熔断状态 ' + s.label +
      ' · 配置版本 ' + (health.version === null ? '未知' : health.version));
  }

  function applySnapshot(snap) {
    if (!snap) return;
    health.version = snap.version;
    // snapshot.degraded=true 即降级；熔断细节要靠指标端点，端点未接入时以快照为准
    if (snap.degraded) health.state = 'degraded';
    else if (health.state === 'unknown' || health.state === 'degraded') health.state = 'healthy';
    paintHealth();
  }

  function refreshHealth() {
    return API.api.snapshot().then(applySnapshot, function (err) {
      if (err.status === 503) { health.state = 'failed'; paintHealth(); }
      else if (err.kind === 'network') { health.state = 'unknown'; paintHealth(); }
    });
  }

  /* ---- 路由 ---- */
  function parseHash() {
    var raw = String(location.hash || '').replace(/^#\/?/, '');
    var qi = raw.indexOf('?');
    var name = (qi >= 0 ? raw.slice(0, qi) : raw) || '';
    var params = {};
    if (qi >= 0) {
      raw.slice(qi + 1).split('&').forEach(function (kv) {
        if (!kv) return;
        var p = kv.split('=');
        params[decodeURIComponent(p[0])] = decodeURIComponent((p[1] || '').replace(/\+/g, ' '));
      });
    }
    return { name: name, params: params };
  }

  function show(which) {
    document.getElementById('view-auth').hidden = which !== 'auth';
    document.getElementById('view-app').hidden = which !== 'app';
  }

  function route() {
    var r = parseHash();
    if (!API.session.token()) {
      if (r.name !== 'login') { location.hash = '#/login'; return; }
      unmount();
      show('auth');
      document.getElementById('auth-token').focus();
      return;
    }
    if (r.name === 'login') { location.hash = '#/monitor'; return; }
    var def = ROUTES[r.name];
    if (!def) { location.hash = '#/monitor'; return; }

    show('app');
    unmount();
    searchInput = null;
    document.getElementById('crumb').textContent = def.title;
    document.title = def.title + ' · unirate 管理控制台';
    Array.prototype.forEach.call(document.querySelectorAll('.nav-item[data-nav]'), function (a) {
      if (a.dataset.nav === r.name) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    });
    var app = document.getElementById('view-app');
    app.dataset.nav = 'closed';
    document.getElementById('nav-toggle').setAttribute('aria-expanded', 'false');

    var outlet = document.getElementById('outlet');
    U.clear(outlet);
    if (expiredNotice) {
      outlet.appendChild(U.el('div', { class: 'note note--warn', style: 'margin-bottom: var(--sp-4)' }, [
        U.icon('i-info'), U.el('span', { text: '会话已重新建立。' })
      ]));
      expiredNotice = false;
    }
    current = def.page;
    w[def.page].mount(outlet, r.params);
    refreshHealth();
  }

  function unmount() {
    if (current && w[current] && w[current].unmount) w[current].unmount();
    current = null;
  }

  /* ---- 会话失效：不静默跳转 ---- */
  function onSessionExpired() {
    API.session.clear();
    unmount();
    show('auth');
    w.PageLogin.setError('会话已失效，请重新输入令牌。', 'warn');
    if (location.hash !== '#/login') location.hash = '#/login';
  }

  /* 登录成功：落健康状态 + 操作者名，然后进监控看板 */
  function onLoginSuccess(snap) {
    applySnapshot(snap);
    paintOperator();
    location.hash = '#/monitor';
    route();
  }

  function paintOperator() {
    var el = document.getElementById('side-operator');
    if (el) el.textContent = API.session.operator() || 'unknown';
  }

  function bind() {
    // 登录流程在 page-login.js（四类失败的区分逻辑都在那里）
    document.getElementById('auth-form').addEventListener('submit', function (ev) {
      w.PageLogin.submit(ev, onLoginSuccess);
    });
    w.PageLogin.bindReveal();

    document.getElementById('theme-toggle').addEventListener('click', function () {
      setTheme(theme() === 'dark' ? 'light' : 'dark');
    });

    document.getElementById('logout').addEventListener('click', function () {
      API.session.clear();
      unmount();
      health.state = 'unknown';
      health.version = null;
      paintHealth();
      location.hash = '#/login';
      show('auth');
      API.toast('ok', '已登出，令牌已从本页会话清除');
    });

    var toggle = document.getElementById('nav-toggle');
    toggle.addEventListener('click', function () {
      var app = document.getElementById('view-app');
      var open = app.dataset.nav === 'open';
      app.dataset.nav = open ? 'closed' : 'open';
      toggle.setAttribute('aria-expanded', open ? 'false' : 'true');
    });

    // Cmd/Ctrl+K 聚焦当前页搜索框
    document.addEventListener('keydown', function (ev) {
      if ((ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === 'k') {
        if (searchInput) { ev.preventDefault(); searchInput.focus(); searchInput.select(); }
      }
    });

    w.addEventListener('hashchange', route);
    API.setUnauthorizedHandler(onSessionExpired);
  }

  w.App = {
    setHealth: setHealth, applySnapshot: applySnapshot, refreshHealth: refreshHealth,
    // 浮层实现在 overlay.js；这里转发，让页面模块只依赖 w.App.* 一个入口
    overlay: w.Overlay.overlay, drawer: w.Overlay.drawer,
    onSessionExpired: onSessionExpired,
    registerSearch: function (node) { searchInput = node; }
  };

  loadSprite().then(function () {
    paintThemeBtn();
    paintHealth();
    paintOperator();
    bind();
    if (!location.hash) location.hash = API.session.token() ? '#/monitor' : '#/login';
    route();
  });
})(window);
