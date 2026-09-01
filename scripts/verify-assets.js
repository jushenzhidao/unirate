/* 资产接线校验（CI 门禁，不进前端产物）：跨文件调用的静态一致性 + 表单接线冒烟测试。
   零构建的代价是没有编译器 —— 调用一个不存在的导出只会在用户点开抽屉那一刻
   炸出 undefined is not a function，而 Go 测试全绿。这个脚本补上那道检查。 */
'use strict';
var fs = require('fs');
var vm = require('vm');
var path = require('path');
// 相对脚本自身定位，使 CI 与本地在任意工作目录下行为一致
var A = path.join(__dirname, '..', 'internal', 'adminui', 'assets') + path.sep;

var pass = 0, fail = 0;
function ck(name, got, want) {
  if (got === want) { pass++; return; }
  fail++;
  console.log('FAIL ' + name + '\n  got  ' + JSON.stringify(got) + '\n  want ' + JSON.stringify(want));
}
function ok(name, cond, detail) {
  if (cond) { pass++; return; }
  fail++;
  console.log('FAIL ' + name + (detail ? '\n  ' + detail : ''));
}

/* ---- 1. 静态检查：page-rules-form.js 引用的每个 F./V./P. 成员必须真实导出 ---- */
function exportsOf(file, globalName) {
  var src = fs.readFileSync(A + file, 'utf8');
  var m = src.match(new RegExp('w\\.' + globalName + '\\s*=\\s*\\{([\\s\\S]*?)\\n\\s*\\};'));
  if (!m) return null;
  var names = [];
  m[1].replace(/([A-Za-z_$][\w$]*)\s*:/g, function (_, n) { names.push(n); });
  return names;
}
var EXP = {
  F: exportsOf('rule-fields.js', 'RuleFields'),
  V: exportsOf('rule-validate.js', 'RuleValidate'),
  P: exportsOf('rule-preview.js', 'RulePreview'),
  S: exportsOf('rule-spec.js', 'RuleSpec')
};
Object.keys(EXP).forEach(function (k) {
  ok('能解析出 ' + k + ' 的导出列表', EXP[k] !== null && EXP[k].length > 0,
    String(EXP[k]));
});

var formSrc = fs.readFileSync(A + 'page-rules-form.js', 'utf8');
['F', 'V', 'P'].forEach(function (ns) {
  var used = {};
  formSrc.replace(new RegExp('\\b' + ns + '\\.([A-Za-z_$][\\w$]*)', 'g'), function (_, n) { used[n] = 1; });
  Object.keys(used).forEach(function (n) {
    ok('page-rules-form 用的 ' + ns + '.' + n + ' 已导出',
      EXP[ns].indexOf(n) >= 0,
      '实际导出：' + EXP[ns].join(', '));
  });
});

// rule-fields.js 引用的 RuleSpec / RuleValidate 成员也要真实存在
var fieldsSrc = fs.readFileSync(A + 'rule-fields.js', 'utf8');
var usedRV = {};
fieldsSrc.replace(/\bRV\.([A-Za-z_$][\w$]*)/g, function (_, n) { usedRV[n] = 1; });
Object.keys(usedRV).forEach(function (n) {
  ok('rule-fields 用的 RV.' + n + ' 已导出', EXP.V.indexOf(n) >= 0, EXP.V.join(', '));
});
var usedS = {};
fieldsSrc.replace(/\bS\.([A-Za-z_$][\w$]*)/g, function (_, n) { usedS[n] = 1; });
Object.keys(usedS).forEach(function (n) {
  ok('rule-fields 用的 S.' + n + ' 已导出', EXP.S.indexOf(n) >= 0, EXP.S.join(', '));
});

/* ---- 2. ctx 契约：rule-fields.js 用到的每个 ctx.X，表单必须提供 ---- */
var ctxUsed = {};
fieldsSrc.replace(/\bctx\.([A-Za-z_$][\w$]*)/g, function (_, n) { ctxUsed[n] = 1; });
var ctxProvided = {};
var ctxBlock = formSrc.match(/var ctx = \{([\s\S]*?)\n    \};/);
ok('能解析出表单的 ctx 定义', !!ctxBlock);
if (ctxBlock) {
  ctxBlock[1].replace(/([A-Za-z_$][\w$]*)\s*:/g, function (_, n) { ctxProvided[n] = 1; });
  Object.keys(ctxUsed).forEach(function (n) {
    ok('ctx.' + n + ' 由表单提供', !!ctxProvided[n],
      '表单提供的是：' + Object.keys(ctxProvided).join(', '));
  });
}

/* ---- 3. CSS 类名：JS 里用的每个 class 都必须在 css 里有定义 ---- */
var css = ['tokens.css', 'base.css', 'layout.css', 'components.css', 'overlay.css', 'charts.css']
  .map(function (f) { return fs.readFileSync(A + f, 'utf8'); }).join('\n');
['page-rules-form.js', 'rule-preview.js', 'rule-fields.js'].forEach(function (f) {
  var src = fs.readFileSync(A + f, 'utf8');
  var seen = {};
  src.replace(/class:\s*'([^']+)'/g, function (_, v) {
    v.split(/\s+/).forEach(function (c) { if (c) seen[c] = 1; });
  });
  Object.keys(seen).forEach(function (c) {
    ok(f + ' 的 class "' + c + '" 在 CSS 中有定义',
      css.indexOf('.' + c) >= 0);
  });
});

/* ---- 4. 图标 id：U.icon('x') 的 x 必须在 icons.svg 里 ---- */
var icons = fs.readFileSync(A + 'icons.svg', 'utf8');
['page-rules-form.js', 'rule-preview.js', 'rule-fields.js'].forEach(function (f) {
  var src = fs.readFileSync(A + f, 'utf8');
  var seen = {};
  src.replace(/U\.icon\('([^']+)'/g, function (_, v) { seen[v] = 1; });
  Object.keys(seen).forEach(function (id) {
    ok(f + ' 的图标 "' + id + '" 存在于 icons.svg', icons.indexOf('id="' + id + '"') >= 0);
  });
});

/* ---- 5. 禁令扫描：emoji / 全宽竖线 / 硬编码色值 / innerHTML / eval ---- */
var NEW_FILES = ['rule-spec.js', 'rule-validate.js', 'rule-preview.js', 'rule-fields.js', 'page-rules-form.js'];
NEW_FILES.forEach(function (f) {
  var src = fs.readFileSync(A + f, 'utf8');
  ok(f + ' 无 emoji', !/[\u{1F300}-\u{1F9FF}\u{2600}-\u{26FF}\u{2700}-\u{27BF}]/u.test(src));
  ok(f + ' 无全宽竖线 U+2502', src.indexOf('\u2502') < 0);
  ok(f + ' 无 innerHTML', src.indexOf('innerHTML') < 0);
  ok(f + ' 无 eval', !/\beval\s*\(/.test(src));
  ok(f + ' 无 new Function', src.indexOf('new Function') < 0);
  var hex = (src.match(/#[0-9a-fA-F]{3,8}\b/g) || []).filter(function (h) {
    return ['#fff', '#ffffff', '#000', '#000000'].indexOf(h.toLowerCase()) < 0;
  });
  ok(f + ' 无硬编码色值', hex.length === 0, '发现：' + hex.join(', '));
  ok(f + ' 无外链', !/https?:\/\//.test(src.replace(/https?:\/\/www\.w3\.org/g, '')));
});

/* ---- 6. 冒烟：在 jsdom 式最小 DOM 桩上真的把表单跑一遍 ---- */
/* dom.js 用 `children instanceof Node` 区分节点与文本，所以桩必须是
   一个真构造器的实例，纯对象字面量过不了这个判定。 */
function Node() { }
function makeNode(tag) {
  var n = new Node();
  Object.assign(n, {
    tagName: (tag || 'div').toUpperCase(), childNodes: [], attrs: {}, listeners: {},
    className: '', textContent: '', value: '', checked: false, disabled: false,
    style: {},
    appendChild: function (c) { if (c) { n.childNodes.push(c); c.parentNode = n; } return c; },
    setAttribute: function (k, v) { n.attrs[k] = String(v); },
    getAttribute: function (k) { return n.attrs[k] === undefined ? null : n.attrs[k]; },
    removeAttribute: function (k) { delete n.attrs[k]; },
    addEventListener: function (k, fn) { (n.listeners[k] = n.listeners[k] || []).push(fn); },
    removeChild: function (c) {
      var i = n.childNodes.indexOf(c); if (i >= 0) n.childNodes.splice(i, 1); return c;
    },
    querySelector: function (sel) {
      var m = sel.match(/^\[aria-label="(.+)"\]$/);
      if (!m) return null;
      var found = null;
      (function walk(x) {
        if (found || !x) return;
        if (x.attrs && x.attrs['aria-label'] === m[1]) { found = x; return; }
        (x.childNodes || []).forEach(walk);
      })(n);
      return found;
    }
  });
  Object.defineProperty(n, 'firstChild', {
    get: function () { return n.childNodes[0] || null; }
  });
  return n;
}
function allNodes(root) {
  var out = [];
  (function walk(x) { if (!x) return; out.push(x); (x.childNodes || []).forEach(walk); })(root);
  return out;
}

var rafQueue = [], timeoutQueue = [];
var doc = {
  createElement: makeNode,
  createElementNS: function (ns, t) { return makeNode(t); },
  createTextNode: function (t) { var n = makeNode('#text'); n.textContent = t; return n; },
  getElementById: function () { return null; },
  body: makeNode('body'),
  addEventListener: function () { }
};
var winStub = {
  document: doc,
  requestAnimationFrame: function (fn) { rafQueue.push(fn); return rafQueue.length; },
  setTimeout: function (fn, ms) { timeoutQueue.push({ fn: fn, ms: ms }); return timeoutQueue.length; },
  clearTimeout: function (id) { if (timeoutQueue[id - 1]) timeoutQueue[id - 1].cancelled = true; },
  location: { hash: '', pathname: '/' },
  navigator: { clipboard: null },
  addEventListener: function () { },
  localStorage: { getItem: function () { return null; }, setItem: function () { } },
  fetch: function () { return Promise.resolve({ ok: true, json: function () { return Promise.resolve({}); } }); }
};
var ctxVM = vm.createContext(Object.assign(winStub, {
  window: winStub, document: doc, console: console,
  JSON: JSON, Math: Math, Date: Date, String: String, Number: Number,
  Object: Object, Array: Array, isFinite: isFinite, parseInt: parseInt,
  Node: Node,
  parseFloat: parseFloat, Promise: Promise, RegExp: RegExp, Error: Error
}));
['dom.js', 'api.js', 'rule-spec.js', 'rule-validate.js', 'rule-fields.js', 'rule-preview.js', 'page-rules-form.js']
  .forEach(function (f) {
    try { vm.runInContext(fs.readFileSync(A + f, 'utf8'), ctxVM, { filename: f }); }
    catch (e) { fail++; console.log('FAIL 加载 ' + f + '：' + e.message); }
  });

ok('RulesForm 已注册', typeof winStub.RulesForm === 'object' && typeof winStub.RulesForm.open === 'function');

// App.drawer 桩：捕获抽屉内容
var drawerArg = null;
winStub.App = { drawer: function (o) { drawerArg = o; return function () { }; } };
// 拦截网络：冒烟不该真发请求
var validateCalls = 0;
winStub.API.api = winStub.API.api || {};
winStub.API.api.validateRules = function () { validateCalls++; return Promise.resolve({ checked: 1 }); };
winStub.API.api.upsertBiz = function () { return Promise.resolve({}); };
winStub.API.toast = function () { };

function flush() {
  var r = rafQueue.slice(); rafQueue.length = 0;
  r.forEach(function (fn) { fn(); });
}
function runTimers() {
  var t = timeoutQueue.slice(); timeoutQueue.length = 0;
  t.forEach(function (e) { if (!e.cancelled) e.fn(); });
}

var biz = { biz: 'order', base_url: '', enabled: true, rules: [] };

/* 场景 A：合法的 rate 规则，开局本地通过 → 应排一次后端复核 */
validateCalls = 0;
try {
  winStub.RulesForm.open(biz, {
    name: 'r1', type: 'rate', metric: 'request', dimensions: ['biz'],
    window: '1m', limit: 1000, algorithm: 'fixed_window',
    burst: 0, watermark: 80, enabled: true
  }, function () { });
  flush();
  ok('场景A 抽屉已构造', !!drawerArg);
  ok('场景A 合法规则未禁用保存', drawerArg && drawerArg.foot[2].disabled === false);
  ok('场景A 本地通过则排了后端复核', timeoutQueue.filter(function (e) { return e.ms === 400; }).length === 1);
} catch (e) {
  fail++; console.log('FAIL 场景A 抛异常：' + e.message + '\n' + e.stack.split('\n').slice(0, 4).join('\n'));
}

/* 场景 B（验收 1）：本地不通过 → 不得发后端请求 */
timeoutQueue.length = 0; rafQueue.length = 0; validateCalls = 0;
try {
  winStub.RulesForm.open(biz, {
    name: '', type: 'rate', metric: 'request', dimensions: [],
    window: 'abc', limit: -5, algorithm: 'fixed_window', watermark: 80, enabled: true
  }, function () { });
  flush();
  ok('场景B 非法规则禁用保存', drawerArg && drawerArg.foot[2].disabled === true);
  ok('验收1 本地不通过时不排后端请求',
    timeoutQueue.filter(function (e) { return e.ms === 400 && !e.cancelled; }).length === 0,
    '实际排了 ' + timeoutQueue.length + ' 个');
  ok('验收1 本地不通过时未发出请求', validateCalls === 0);
  var texts = allNodes(drawerArg.body).map(function (n) { return n.textContent || ''; }).join(' | ');
  ok('场景B 展示了具体阻断原因', /限额|窗口|维度|规则名/.test(texts), texts.slice(0, 200));
} catch (e) {
  fail++; console.log('FAIL 场景B 抛异常：' + e.message + '\n' + e.stack.split('\n').slice(0, 4).join('\n'));
}

/* 场景 C（陷阱 1 / 验收 4）：concurrency 带一堆非法的 rate 字段 → 本地必须放行 */
timeoutQueue.length = 0; rafQueue.length = 0;
try {
  winStub.RulesForm.open(biz, {
    name: 'c1', type: 'concurrency', max_concurrent: 5, timeout: 0,
    limit: -5, window: 'abc', metric: 'bogus', algorithm: 'bogus',
    dimensions: ['biz'], watermark: 80, enabled: true
  }, function () { });
  flush();
  ok('陷阱1 concurrency 不因 rate 字段非法而禁用保存',
    drawerArg && drawerArg.foot[2].disabled === false);
  var t3 = allNodes(drawerArg.body).map(function (n) { return n.textContent || ''; }).join(' | ');
  ok('陷阱1 显式说明这些字段不校验', /不校验|不使用/.test(t3), t3.slice(0, 300));
} catch (e) {
  fail++; console.log('FAIL 场景C 抛异常：' + e.message + '\n' + e.stack.split('\n').slice(0, 4).join('\n'));
}

/* 场景 D（陷阱 2 / 验收 7）：token_bucket + burst=1 + window=abc
   必须报「窗口格式非法」，绝不能报「突发容量过小」 */
timeoutQueue.length = 0; rafQueue.length = 0;
try {
  winStub.RulesForm.open(biz, {
    name: 'd1', type: 'rate', metric: 'request', dimensions: ['biz'],
    window: 'abc', limit: 3600, algorithm: 'token_bucket', burst: 1,
    watermark: 80, enabled: true
  }, function () { });
  flush();
  var t4 = allNodes(drawerArg.body).map(function (n) { return n.textContent || ''; }).join(' | ');
  ok('陷阱2 报了窗口问题', /窗口/.test(t4), t4.slice(0, 300));
  ok('陷阱2 未误报突发容量过小', !/突发容量.*(过小|不小于 Infinity)/.test(t4), t4.slice(0, 300));
} catch (e) {
  fail++; console.log('FAIL 场景D 抛异常：' + e.message + '\n' + e.stack.split('\n').slice(0, 4).join('\n'));
}

/* 场景 E：切换类型不应抛异常（会触发 rebuild） */
try {
  timeoutQueue.length = 0; rafQueue.length = 0;
  winStub.RulesForm.open(biz, null, function () { });
  flush();
  var segBtns = allNodes(drawerArg.body).filter(function (n) {
    return n.tagName === 'BUTTON' && /并发/.test(n.textContent || '');
  });
  ok('找到类型切换按钮', segBtns.length > 0);
  if (segBtns.length) {
    segBtns[0].listeners.click ? segBtns[0].listeners.click[0]() : (segBtns[0].attrs.onclick && segBtns[0].attrs.onclick());
  }
  flush();
  ok('切换类型后未抛异常', true);
} catch (e) {
  fail++; console.log('FAIL 场景E 抛异常：' + e.message + '\n' + e.stack.split('\n').slice(0, 5).join('\n'));
}

console.log('\n通过 ' + pass + ' 条，失败 ' + fail + ' 条');
process.exit(fail ? 1 : 0);
