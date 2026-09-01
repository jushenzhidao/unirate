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

  /* ---- 规则校验错误映射：后端返回英文技术文案，翻中文，未命中原文兜底，绝不吞。

     注意：匹配**不可用 `^` 锚定**。除 :108 的 "rule name required" 外，rule.go 的
     每一条都用 `fmt.Errorf("rule %q: ...", r.Name)` 包装，实际到达前端的是
         rule "api-limit": limit must be > 0
     开头是 `rule "` 而不是 `limit`。原实现 16 条里 15 条用了 `^`，
     全部匹配不到，界面上永远显示英文原文。

     另有两类与 `^` 无关的失配，一并修正：
       - 词形不符：`rule name is required` / 实际 `rule name required`
                   `at least one dimension` / 实际 `dimensions required`
                   `invalid dimension`      / 实际 `unknown dimension %q`
                   `invalid rule type`      / 实际 `unknown type %q`
                   `invalid metric`         / 实际 `unknown metric %q`
                   `global ... combined`    / 实际 `cannot combine with others`
       - 结构不可达：`^watermark`（:181 静默改写成 80，从不报错）
                     `^timeout`（:135 静默改写成 120，从不报错）
                     `invalid algorithm`（Validate() 根本不校验算法枚举）
                     `window is required`（空窗口走 invalid window %q）
                     `limit must be`（已被 `limit must be > 0` 覆盖）
         这 5 条对应的后端错误不存在，保留只会让人误以为覆盖了。已删除。

     逐条锚定到 rule.go 的真实文案，顺序即优先级（具体在前、宽泛在后）。 */
  var RULE_ERR_MAP = [
    // :108 —— 唯一不带 rule %q 前缀的一条
    [/rule name required/i, '规则名必填'],
    // :111
    [/dimensions required/i, '至少选择一个限流维度'],
    // :117 / :120
    [/unknown dimension/i, '存在不支持的限流维度'],
    [/duplicated dimension/i, '限流维度重复'],
    // :124
    [/dimension 'global' cannot combine/i,
      'global 是不分维度的全局限流，不能与具体维度组合'],
    // :133
    [/max_concurrent must be > 0/i, '最大并发必须大于 0'],
    // :142
    [/limit must be > 0/i, '限额必须大于 0'],
    // :100 —— 单位错误比 :81/:86 的通用窗口错误更具体，必须排在它前面
    [/invalid window unit/i, '时间窗口单位非法（仅 s / m / h / d / w）'],
    // :81 / :86
    [/invalid window/i, '时间窗口格式非法（例如 30s / 5m / 2h / 1d / 2w）'],
    // :154
    [/unknown metric/i, '计量对象非法（仅 request 或 token）'],
    // :164
    [/token_bucket cannot be used with metric=token/i,
      '令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定或滑动窗口'],
    // :172
    [/burst too small for rate/i,
      '突发容量过小，至少要等于「限额 ÷ 窗口秒数」向下取整的值'],
    // :178
    [/sliding_window with limit > 100000/i,
      '滑动窗口限额上限 100000，超过会撑爆 ZSet 内存，请改用固定窗口'],
    // :187
    [/unknown type/i, '规则类型非法（仅 rate 或 concurrency）']
  ];

  /* translateRuleError —— 剥掉 `rule "名字": ` 前缀后再展示。
     前缀对用户无信息量（他正在编辑的就是这条规则），留着会把每条错误
     都撑长一截，反而盖住真正的原因。未命中映射时也要剥。 */
  var RULE_PREFIX = /^rule\s+"[^"]*":\s*/i;

  function translateRuleError(raw) {
    var s = String(raw || '').trim();
    for (var i = 0; i < RULE_ERR_MAP.length; i++) {
      if (RULE_ERR_MAP[i][0].test(s)) return RULE_ERR_MAP[i][1];
    }
    return s.replace(RULE_PREFIX, ''); // 未命中不吞，剥前缀后原文显示
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
