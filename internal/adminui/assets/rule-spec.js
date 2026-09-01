/* ============================================================================
   规则常量表 —— 纯数据，零函数逻辑
   ----------------------------------------------------------------------------
   这里的每一个值都在后端有对应权威定义（internal/limiter/rule.go 与 key.go）。
   本文件被 rulespec_test.go 用正则抽字面量、与 Go 真函数的输出逐位比对，
   所以「改这里必须同时改后端」不是口头约定，而是会红的测试。

   为什么单独一个文件而不是塞进 rule-fields.js：
   校验器（rule-validate.js）与预览（rule-preview.js）都要读这些常量，
   而它们都不该依赖表单控件模块。常量下沉后依赖是单向的。

   分隔符一律 ASCII 半角 '|'（U+007C）。全宽 '\u2502' 不在 emoji 门禁的扫描
   区间内，CI 抓不到 —— 设计稿里曾真的写成全宽，故这里显式记一笔。
   ========================================================================== */
(function (w) {
  'use strict';

  /* 规则类型 —— rule.go:21-24。两者的校验分支完全不同，见 rule.go:130 switch */
  var TYPES = [
    { v: 'rate', t: '速率 rate' },
    { v: 'concurrency', t: '并发 concurrency' }
  ];

  /* 计量对象 —— rule.go:29-32 */
  var METRICS = [
    { v: 'request', t: '请求数 request' },
    { v: 'token', t: 'Token 数 token' }
  ];

  /* 限流算法 —— rule.go:12-16 */
  var ALGOS = [
    { v: 'fixed_window', t: '固定窗口' },
    { v: 'sliding_window', t: '滑动窗口' },
    { v: 'token_bucket', t: '令牌桶' }
  ];

  /* 维度白名单 —— rule.go:44-47 validDims。集合必须与后端完全相等 */
  var DIMS = ['global', 'biz', 'ip', 'token', 'path', 'method'];

  /* 窗口预设。保留 1m/1w：移除会让既有规则的窗口值掉出预设、落进自定义输入框 */
  var WINDOWS = ['1s', '1m', '5m', '1h', '1d', '1w', '2w'];

  /* 两族的差别不是时长而是对齐语义 —— 自然族受部署的业务时区偏移影响。
     下拉用 optgroup 分族，让这个差别在展开那一刻可见，而非选完后补文案。 */
  var WINDOW_GROUPS = [
    { label: '滚动窗口（按绝对时间对齐）', items: ['1s', '1m', '5m', '1h'] },
    { label: '自然对齐（按业务时区零点）', items: ['1d', '1w', '2w'] }
  ];

  /* 单位 → 秒。严格对应 rule.go:88-101 的 switch */
  var UNIT = { s: 1, m: 60, h: 3600, d: 86400, w: 604800 };

  /* natural 完全由单位决定，与数值无关（rule.go:95-98）——
     故 30s / 2h 同样是 false，不存在「窗口够长就算自然日」这回事。 */
  var NATURAL_UNITS = ['d', 'w'];

  /* 计数键构造常量 —— key.go:26-34。
     emptyVal 是 '_' 不是 '-'（key.go:40，且 limiter.go:171 的 global 取值同为 '_'）。 */
  var KEY = {
    sep: '|',
    dimJoin: '.',
    valJoin: '.',
    prefixRate: 'rl',
    prefixTokenBucket: 'tb',
    prefixConcurrency: 'cc',
    prefixTokenLedger: 'tk',
    maxRawLen: 48,
    hashPrefix: 'h',
    hashHexLen: 24,
    emptyVal: '_',
    /* safeVal 触发哈希的字符集 —— key.go:42 ContainsAny "|/ \t\r\n" */
    unsafeChars: '|/ \t\r\n'
  };

  /* 后端静默规范化的落库值。会真实影响行为，UI 必须显式告知。 */
  var DEFAULTS = {
    metric: 'request',        // rule.go:150-152
    algorithm: 'fixed_window',// rule.go:156-158
    watermark: 80,            // rule.go:181-183
    timeoutSec: 120,          // rule.go:135-137
    slidingLimitMax: 100000   // rule.go:177
  };

  w.RuleSpec = {
    TYPES: TYPES, METRICS: METRICS, ALGOS: ALGOS, DIMS: DIMS,
    WINDOWS: WINDOWS, WINDOW_GROUPS: WINDOW_GROUPS,
    UNIT: UNIT, NATURAL_UNITS: NATURAL_UNITS,
    KEY: KEY, DEFAULTS: DEFAULTS
  };
})(window);
