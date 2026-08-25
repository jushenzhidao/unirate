/* ============================================================================
   鉴权 fetch 封装 / 会话 / Toast / 错误映射
   ----------------------------------------------------------------------------
   Token 存 sessionStorage 而非 localStorage：关闭标签页即失效，
   避免 XSS 拿到一个长期有效的管理面凭证。
   Token 绝不进 URL、不进日志、不进 Toast 文案。
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U;
  var TOKEN_KEY = 'unirate_admin_token';
  var OPERATOR_KEY = 'unirate_operator';

  function safeGet(store, key) {
    try { return w[store].getItem(key); } catch (e) { return null; }
  }
  function safeSet(store, key, val) {
    try { w[store].setItem(key, val); } catch (e) { /* 隐私模式下写入抛异常，忽略 */ }
  }
  function safeDel(store, key) {
    try { w[store].removeItem(key); } catch (e) {}
  }

  var session = {
    token: function () { return safeGet('sessionStorage', TOKEN_KEY) || ''; },
    setToken: function (t) { safeSet('sessionStorage', TOKEN_KEY, t); },
    clear: function () { safeDel('sessionStorage', TOKEN_KEY); },
    operator: function () { return safeGet('localStorage', OPERATOR_KEY) || ''; },
    setOperator: function (v) {
      if (v) safeSet('localStorage', OPERATOR_KEY, v);
      else safeDel('localStorage', OPERATOR_KEY);
    }
  };

  /* ApiError 区分四类：http（带 status）/ network / parse。
     调用方据此给不同文案 —— 401 与 403 的处置完全不同，不能合并成"请求失败"。 */
  function ApiError(kind, status, message, payload) {
    this.name = 'ApiError';
    this.kind = kind;
    this.status = status || 0;
    this.message = message || '请求失败';
    this.payload = payload || null;
  }
  ApiError.prototype = Object.create(Error.prototype);

  var onUnauthorized = null;

  /* request(path, opts)
     opts.token   覆盖会话令牌（登录页试探用）
     opts.silent401  登录试探时不触发全局会话失效跳转 */
  function request(path, opts) {
    opts = opts || {};
    var token = opts.token !== undefined ? opts.token : session.token();
    var headers = { 'Accept': 'application/json' };
    if (token) headers['Authorization'] = 'Bearer ' + token;
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    // 写操作带操作者名，让审计日志可读（后端 audit 回落 unknown）
    var m = (opts.method || 'GET').toUpperCase();
    if (m !== 'GET') {
      var op = session.operator();
      if (op) headers['X-Operator'] = op;
    }

    return fetch(path, {
      method: m,
      headers: headers,
      cache: 'no-store',
      credentials: 'omit',
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body)
    }).then(function (res) {
      return res.text().then(function (raw) {
        var data = null;
        if (raw) { try { data = JSON.parse(raw); } catch (e) { data = null; } }
        if (res.ok) return data;
        if (res.status === 401 && !opts.silent401 && onUnauthorized) onUnauthorized();
        var msg = (data && (data.error || data.message)) || ('HTTP ' + res.status);
        throw new ApiError('http', res.status, msg, data);
      });
    }, function (netErr) {
      // fetch 只在网络层失败时 reject；这与 4xx/5xx 是完全不同的故障
      throw new ApiError('network', 0, netErr && netErr.message ? netErr.message : '网络不可达', null);
    });
  }

  /* 202 saved_but_publish_failed 是后端真实分支：已落库但发布失败，
     绝不能当成功静默处理，否则运维以为配置已生效。 */
  function isPublishFailed(data) {
    return !!(data && data.status === 'saved_but_publish_failed');
  }

  var api = {
    snapshot: function (opts) { return request('admin/snapshot', opts); },
    bizs: function () { return request('admin/bizs'); },
    upsertBiz: function (payload) { return request('admin/bizs', { method: 'POST', body: payload }); },
    deleteBiz: function (biz) {
      return request('admin/bizs/' + encodeURIComponent(biz), { method: 'DELETE' });
    },
    validateRules: function (rules) {
      return request('admin/rules/validate', { method: 'POST', body: rules });
    },
    audit: function () { return request('admin/audit'); },
    reload: function () { return request('admin/reload', { method: 'POST' }); },
    policy: function () { return request('admin/policy'); },
    putPolicy: function (payload) { return request('admin/policy', { method: 'PUT', body: payload }); },
    validatePolicy: function (payload) {
      return request('admin/policy/validate', { method: 'POST', body: payload });
    },
    metrics: function () { return request('admin/metrics', { raw: true }); }
  };

  /* ---- 规则校验错误映射：后端返回英文技术文案，已知前缀翻中文，
     未命中原文兜底显示，绝不吞（DESIGN.md §5.3）。 */
  var RULE_ERR_MAP = [
    [/^rule name is required/i, '规则名必填'],
    [/^limit must be > 0/i, '限额必须大于 0'],
    [/^limit must be/i, '限额取值非法'],
    [/^max_concurrent must be > 0/i, '最大并发必须大于 0'],
    [/^invalid window/i, '时间窗口格式非法（例如 1s / 5m / 1h / 1d / 1w）'],
    [/^window is required/i, '时间窗口必填'],
    [/^invalid rule type/i, '规则类型非法（仅 rate 或 concurrency）'],
    [/^invalid metric/i, '计量对象非法（仅 request 或 token）'],
    [/^invalid algorithm/i, '算法非法（仅固定窗口 / 滑动窗口 / 令牌桶）'],
    [/^at least one dimension/i, '至少选择一个限流维度'],
    [/^invalid dimension/i, '存在不支持的限流维度'],
    [/global.*combined|combined.*global/i, 'global 是不分维度的全局限流，不能与具体维度组合'],
    [/token_bucket.*token|token.*token_bucket/i,
      '令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定或滑动窗口'],
    [/^burst/i, '突发容量取值非法'],
    [/^watermark/i, '水位告警阈值取值非法（0-100）'],
    [/^timeout/i, '持有超时取值非法']
  ];

  function translateRuleError(raw) {
    var s = String(raw || '').trim();
    for (var i = 0; i < RULE_ERR_MAP.length; i++) {
      if (RULE_ERR_MAP[i][0].test(s)) return RULE_ERR_MAP[i][1];
    }
    return s; // 未命中不吞，原文显示
  }

  /* ---- Toast ---- */
  var TOAST_ICON = { ok: 'i-check', danger: 'i-status-fail', warn: 'i-status-warn', info: 'i-info' };

  function toast(kind, message, detail) {
    var host = document.getElementById('toasts');
    if (!host) return;
    var node = U.el('div', { class: 'toast', dataset: { kind: kind }, role: 'status' }, [
      U.icon(TOAST_ICON[kind] || TOAST_ICON.info),
      U.el('div', null, [
        U.el('div', { text: message }),
        detail ? U.el('pre', { text: String(detail) }) : null
      ]),
      U.el('button', {
        class: 'affix-btn', type: 'button', 'aria-label': '关闭提示',
        onclick: function () { if (node.parentNode) node.parentNode.removeChild(node); }
      }, U.icon('i-close'))
    ]);
    host.appendChild(node);
    var ttl = kind === 'danger' ? 12000 : 6000;
    w.setTimeout(function () { if (node.parentNode) node.parentNode.removeChild(node); }, ttl);
  }

  w.API = {
    session: session, request: request, api: api,
    ApiError: ApiError, isPublishFailed: isPublishFailed,
    translateRuleError: translateRuleError, toast: toast,
    setUnauthorizedHandler: function (fn) { onUnauthorized = fn; }
  };
})(window);
