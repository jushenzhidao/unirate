/* ============================================================================
   规则校验器 —— internal/limiter/rule.go Validate() 的前端镜像
   ----------------------------------------------------------------------------
   纯函数，不碰 DOM。职责是把「加载期才暴露的配置错误」提前到键入那一刻。

   后端校验不下线，本文件是第一道而非唯一一道。因此有一条硬纪律：
   **前端绝不可比后端更严。** 拦住一个后端本会接受的配置，比漏报更难排查 ——
   用户看到的是「界面说不行，但这配置线上明明在跑」。

   所以这里镜像的不只是规则本身，还有 rule.go 的控制流：
   哪些字段在哪个分支被跳过、哪条检查先于哪条、哪个前置条件不满足时必须
   「跳过」而不是「通过」。三条陷阱在下方各自的实现处有标注。
   ========================================================================== */
(function (w) {
  'use strict';
  var S = w.RuleSpec;

  /* ---- parseWindow：严格移植 rule.go:79-102 -------------------------------
     Go 用 strconv.ParseInt(s, 10, 64)，所以「什么算合法数字」由它定义，
     不由直觉定义。实测后端行为（go test 探针）：

       "1h"   -> 3600     "+1h"  -> 3600   ← 前导正号后端接受
       "01h"  -> 3600     "1.5h" -> 拒绝
       "-1h"  -> 拒绝（n<=0）   "0s"  -> 拒绝（n<=0）
       "1_0h" -> 拒绝（下划线仅 base 0 允许）
       "0x10h"-> 拒绝     "1e2s" -> 拒绝
       " 1h"  -> 拒绝（ParseInt 不 trim）

     `+1h` 这一条与 spec-rule-viz.md §2 的描述相反 —— 该文件称必须拒绝。
     此处以实测的后端行为为准：拒绝它会让前端比后端更严。 */
  var INT10 = /^[+-]?[0-9]+$/;
  var INT64_MAX = '9223372036854775807';

  function parseWindow(raw) {
    var s = String(raw === null || raw === undefined ? '' : raw);
    if (s.length < 2) {
      return { ok: false, sec: 0, natural: false, code: 'window_format', value: s };
    }
    var unit = s.charAt(s.length - 1);
    var digits = s.slice(0, -1);
    if (!INT10.test(digits)) {
      return { ok: false, sec: 0, natural: false, code: 'window_format', value: s };
    }
    // int64 溢出：Go 的 ParseInt 报错，前端若用 Number 会静默得到近似值
    var bare = digits.replace(/^\+/, '');
    var trimmed = bare.replace(/^0+(?=[0-9])/, '');
    if (digits.charAt(0) !== '-' &&
        (trimmed.length > INT64_MAX.length ||
         (trimmed.length === INT64_MAX.length && trimmed > INT64_MAX))) {
      return { ok: false, sec: 0, natural: false, code: 'window_format', value: s };
    }
    var n = Number(digits);
    if (!isFinite(n) || n <= 0) {
      return { ok: false, sec: 0, natural: false, code: 'window_format', value: s };
    }
    if (!Object.prototype.hasOwnProperty.call(S.UNIT, unit)) {
      return { ok: false, sec: 0, natural: false, code: 'window_unit', value: unit };
    }
    return {
      ok: true,
      sec: n * S.UNIT[unit],
      natural: S.NATURAL_UNITS.indexOf(unit) >= 0,
      unit: unit,
      code: null
    };
  }

  /* ---- burstFloor：rule.go:171 是 int64 整除，必须 floor ------------------
     limit=10 / winSec=3 → 下限 3，不是 3.33 也不是 4。
     用 ceil 或 toFixed 会让 burst=3 被误判成过小 —— 后端接受它。

     winSec <= 0 时返回 null 而非 Infinity：调用方据此判「跳过」（陷阱 2）。 */
  function burstFloor(limit, winSec) {
    if (!isFinite(limit) || !isFinite(winSec) || winSec <= 0) return null;
    return Math.floor(limit / winSec);
  }

  function isInt(v) {
    return typeof v === 'number' && isFinite(v) && Math.floor(v) === v;
  }
  function numOr(v, dflt) {
    return isInt(v) ? v : (isFinite(Number(v)) && String(v).trim() !== '' ? Number(v) : dflt);
  }

  var MSG = {
    name_required: '规则名必填',
    dims_required: '至少选择一个限流维度',
    dim_unknown: '存在不支持的限流维度',
    dim_duplicated: '限流维度重复',
    dim_global_combo: 'global 是不分维度的全局限流，不能与具体维度组合',
    conc_required: '最大并发必须大于 0',
    limit_required: '限额必须大于 0',
    window_format: '时间窗口格式非法（例如 30s / 5m / 2h / 1d / 2w）',
    window_unit: '时间窗口单位非法（仅 s / m / h / d / w）',
    metric_unknown: '计量对象非法（仅 request 或 token）',
    tb_token: '令牌桶是持久速率桶，与窗口内总量语义不兼容。Token 预算请用固定或滑动窗口',
    burst_too_small: '突发容量过小',
    sliding_limit_max: '滑动窗口限额上限 ' + S.DEFAULTS.slidingLimitMax +
      '，超过会撑爆 ZSet 内存，请改用固定窗口',
    type_unknown: '规则类型非法（仅 rate 或 concurrency）'
  };

  /* ---- 维度校验：rule.go:110-128 ----------------------------------------
     顺序即后端顺序：空 → 未知 → 重复 → global 组合。
     后端在同一个循环里按下标推进，故「第一个未知维度」先于「第二个维度重复」。 */
  function checkDimensions(draft) {
    var dims = draft.dimensions || [];
    if (dims.length === 0) return [{ field: 'dimensions', code: 'dims_required' }];

    var seen = {};
    for (var i = 0; i < dims.length; i++) {
      var d = String(dims[i]).trim().toLowerCase();
      if (S.DIMS.indexOf(d) < 0) {
        return [{ field: 'dimensions', code: 'dim_unknown', value: d }];
      }
      if (seen[d]) {
        return [{ field: 'dimensions', code: 'dim_duplicated', value: d }];
      }
      if (d === 'global' && dims.length > 1) {
        return [{ field: 'dimensions', code: 'dim_global_combo' }];
      }
      seen[d] = true;
    }
    return [];
  }

  /* finish —— 规范化提示只在规则**整体通过**时才成立。

     差分测试（对 Validate() 跑 36 组 draft）抓到的真实缺陷：后端在若干处
     「先 return 错误、后做规范化」，故有阻断错误时规范化那几行根本执行不到。
     两个实例：
       - :163 token_bucket+token 先 return，:168 的 burst<=0 → limit 永不执行
       - :132 max_concurrent<=0 先 return，:135 的 timeout<=0 → 120 永不执行

     更一般地：Validate() 返回 error 时整条规则不落库，任何「将按 N 落库」
     都是假承诺。与其逐个 early-return 点建模（漏一个就是错），不如按这个
     不变量统一裁剪 —— 它对所有现存与未来的 early return 都成立。 */
  function finish(blocking, normalized, skipped) {
    return {
      blocking: blocking,
      normalized: blocking.length ? [] : normalized,
      skipped: skipped
    };
  }

  /* ---- 主校验 -------------------------------------------------------------
     返回 { blocking: [...], normalized: [...], skipped: [...] }

     与后端的一个刻意差异：后端首个错误即 return，前端并行报全部错误 ——
     表单上一次看清所有问题比逐个挤牙膏好。这不构成「更严」，因为错误集合
     是后端错误的超集且每一条都真实存在。

     但这带来一个连带要求（陷阱 3）：后端 400 回填时判断前后端是否漂移，
     必须用「后端错误是否落在前端已报集合内」，不能比「是否等于前端首错」。
     判定函数是下方的 backendErrorCovered。 */
  function validate(draft) {
    var blocking = [];
    var normalized = [];
    var skipped = [];

    /* 后端 rule.go:107 是 r.Name == ""，**不 trim**。纯空白 name 后端放行并落库，
       前端若 trim 后判空就会拦住后端接受的配置 —— 差分测试 140 组抓到 7 条误报
       （单/双空格、tab、换行、U+3000、空白+concurrency）。镜像类校验宁松不紧。 */
    if (draft.name === undefined || draft.name === null || String(draft.name) === '') {
      blocking.push({ field: 'name', code: 'name_required' });
    }

    blocking = blocking.concat(checkDimensions(draft));

    var type = draft.type;

    /* 陷阱 1 —— concurrency 分支后端 :129-137 检查完 max_concurrent 即 return nil。
       limit / window / metric / algorithm 四项**完全不校验**：填 '1.5h'、-5、
       乱值全部合法通过。前端对这四项必须整体短路，否则拦住后端会接受的配置。
       实测确认：{Type:concurrency, MaxConc:5, Window:'1.5h', Limit:-5,
       Metric:'bogus', Algorithm:'bogus'} → Validate() 返回 nil。 */
    if (type === 'concurrency') {
      var conc = numOr(draft.max_concurrent, 0);
      if (!(conc > 0)) blocking.push({ field: 'max_concurrent', code: 'conc_required' });

      var timeout = numOr(draft.timeout, 0);
      if (timeout <= 0) {
        normalized.push({
          field: 'timeout', to: S.DEFAULTS.timeoutSec,
          text: '留空或 0 将按 ' + S.DEFAULTS.timeoutSec + ' 秒落库'
        });
      }
      skipped.push({ fields: ['limit', 'window', 'metric', 'algorithm'],
        reason: '并发类型不校验这些字段' });
      return finish(blocking, normalized, skipped);
    }

    if (type !== 'rate') {
      blocking.push({ field: 'type', code: 'type_unknown', value: String(type) });
      return finish(blocking, normalized, skipped);
    }

    /* rate 分支。注意 limit<=0 在 ParseWindow **之前**（:141 vs :144）——
       limit=0 且 window='abc' 时后端报的是 limit。实测确认。 */
    var limit = numOr(draft.limit, 0);
    if (!(limit > 0)) blocking.push({ field: 'limit', code: 'limit_required' });

    var win = parseWindow(draft.window);
    if (!win.ok) blocking.push({ field: 'window', code: win.code, value: win.value });

    var metric = draft.metric || S.DEFAULTS.metric;
    if (metric !== 'request' && metric !== 'token') {
      blocking.push({ field: 'metric', code: 'metric_unknown', value: metric });
    }

    var algo = draft.algorithm || S.DEFAULTS.algorithm;

    if (algo === 'token_bucket') {
      if (metric === 'token') {
        blocking.push({ field: 'algorithm', code: 'tb_token' });
      }
      var burst = numOr(draft.burst, 0);

      /* :168 先把 burst<=0 规范化成 Limit，:171 才比下限。
         故 burst<=0 是**合法**的，绝不标红 —— 它等价于 burst=limit。 */
      if (burst <= 0) {
        if (limit > 0) {
          normalized.push({
            field: 'burst', to: limit,
            text: '留空或 0 将按限额 ' + limit + ' 落库'
          });
        }
      } else {
        /* 陷阱 2 —— 下限判定隐含依赖 winSec > 0。
           后端靠 :144 ParseWindow + :145 err 提前 return 保证这个前提；
           前端逐字段独立校验没有这个保证。JS 里 10/0 === Infinity，
           而 1 < Infinity 为真 → 窗口填 'abc' 时会误报「突发容量过小」，
           而后端报的是「窗口格式非法」。

           前置条件不满足必须返回「跳过」，不是「通过」也不是「失败」。 */
        var floor = burstFloor(limit, win.ok ? win.sec : 0);
        if (floor === null) {
          skipped.push({ fields: ['burst'], reason: '窗口未解析成功，突发容量下限无法判定' });
        } else if (burst < floor) {
          blocking.push({
            field: 'burst', code: 'burst_too_small',
            value: burst, floor: floor
          });
        }
      }
    }

    /* 滑动窗口上限在 token_bucket 分支**之外**（:177），但在 metric 之后。
       所以 algo=sliding 时它是可达的，algo=token_bucket 时它不可达。 */
    if (algo === 'sliding_window' && limit > S.DEFAULTS.slidingLimitMax) {
      blocking.push({ field: 'limit', code: 'sliding_limit_max', value: limit });
    }

    var wm = numOr(draft.watermark, 0);
    if (wm <= 0 || wm > 100) {
      normalized.push({
        field: 'watermark', to: S.DEFAULTS.watermark,
        text: '超出 1-100 将按 ' + S.DEFAULTS.watermark + '% 落库'
      });
    }

    return finish(blocking, normalized, skipped);
  }

  function message(item) {
    var base = MSG[item.code] || item.code;
    if (item.code === 'burst_too_small') {
      return base + '：至少 ' + item.floor + '（限额 ÷ 窗口秒数，向下取整）';
    }
    if (item.code === 'dim_unknown' || item.code === 'dim_duplicated') {
      return base + '：' + item.value;
    }
    if (item.code === 'window_unit') return base;
    return base;
  }

  function messages(result) {
    return result.blocking.map(message);
  }

  /* backendErrorCovered —— 陷阱 3 的漂移判定。
     后端单错误返回，前端并行多错。判据是「后端这条错误是否已在前端报出的
     集合内」；用「是否等于前端首错」会把正常情况误判成前后端不一致。
     未覆盖才是真漂移，说明前端镜像漏了一条后端规则。 */
  function backendErrorCovered(result, translatedBackendMsg) {
    var mine = messages(result);
    var s = String(translatedBackendMsg || '').trim();
    if (s === '') return true;
    for (var i = 0; i < mine.length; i++) {
      if (mine[i] === s || s.indexOf(mine[i]) >= 0 || mine[i].indexOf(s) >= 0) return true;
    }
    return false;
  }

  w.RuleValidate = {
    parseWindow: parseWindow,
    burstFloor: burstFloor,
    validate: validate,
    message: message,
    messages: messages,
    backendErrorCovered: backendErrorCovered,
    MSG: MSG
  };
})(window);
