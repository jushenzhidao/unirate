/* ============================================================================
   计数键预览 —— internal/limiter/limiter.go:220-256 的五分支镜像
   ----------------------------------------------------------------------------
   只读渲染，不产生任何判定。判定一律来自 rule-validate.js。

   这个预览的全部价值是「可复制去 Redis 查」。所以形态错了比不显示更糟 ——
   运维会拿一个不存在的字符串去 SCAN，然后怀疑限流没生效。
   因此两条纪律：
     1. 分支判定顺序照抄后端 switch，metric=token 在 algorithm **之前**（:226 早于 :236）
     2. 拿不到运行时真值的段（业务时区零点）显示占位符 + SCAN 通配符命令，
        绝不显示推算出来的数字（前端无合法途径获取 TZ_OFFSET_SECONDS）
   ========================================================================== */
(function (w) {
  'use strict';
  var U = w.U, S = w.RuleSpec, RV = w.RuleValidate;
  var K = S.KEY;

  /* 示例哈希值。前端不做 sha256（也不该做），但形态必须对：
     24 位纯 hex。safeVal 降级带 'h' 前缀，HashToken 裸值无前缀（key.go:46 vs :56）。 */
  var SAMPLE_HEX = '2bb80d537b1da3e38bd3';
  var SAMPLE_HEX24 = SAMPLE_HEX + '0361';

  /* 各维度的示例取值。limiter.go:167-186 dimValues 的取值来源。 */
  function sampleVal(dim, biz) {
    switch (dim) {
      case 'global': return K.emptyVal;   // limiter.go:171-172，且 safeVal('') 同为 '_'
      case 'biz': return biz || 'order';
      case 'path': return '/v1/chat/completions';
      case 'token': return null;          // 特殊：走 HashToken，见 tokenSeg
      case 'ip': return '10.1.2.3';
      case 'method': return 'POST';
      default: return dim;
    }
  }

  /* safeValShape —— key.go:38-47 的形态镜像。
     触发条件必须与后端一致：空串 → '_'；长度 > maxRawLen 或含 unsafeChars → 哈希降级。
     注意 path 通常含 '/'，所以实践中 path 段几乎总是哈希形态。 */
  function safeValShape(v) {
    if (v === '') return K.emptyVal;
    var unsafe = false;
    for (var i = 0; i < K.unsafeChars.length; i++) {
      if (v.indexOf(K.unsafeChars.charAt(i)) >= 0) { unsafe = true; break; }
    }
    if (v.length <= K.maxRawLen && !unsafe) return v;
    return K.hashPrefix + SAMPLE_HEX24;   // 'h' + 24 hex
  }

  /* token 段是 HashToken 的裸输出：无 'h' 前缀（key.go:51-57）。
     写成 'h...' 是设计稿里出现过的真实错误，会让 SCAN 一个都匹配不到。 */
  function tokenSeg() { return SAMPLE_HEX24; }

  function valsOf(dims, biz) {
    return dims.map(function (d) {
      if (d === 'token') return tokenSeg();
      return safeValShape(sampleVal(d, biz));
    });
  }

  /* boundary 分两档（key.go:73-82）：
     - 滚动族（s/m/h）：natural=false，与 tzOffset 无关，前端算得出逐位一致的真值
     - 自然族（d/w）：natural=true 且 tzOffset != 0 时按业务时区零点对齐，
       前端拿不到 TZ_OFFSET_SECONDS，只能给占位符 */
  var PLACEHOLDER_D = '<业务时区当日零点>';
  var PLACEHOLDER_W = '<业务时区本周零点>';

  /* nowMs 可注入：不注入时用 Date.now()。留这个参数是为了让边界值可被
     确定性比对 —— 一个只能在「当前时刻」自证的函数没法对预言机验证。 */
  function boundarySeg(win, nowMs) {
    if (!win.ok) return null;
    if (!win.natural) {
      var ms = nowMs === undefined ? Date.now() : nowMs;
      return String(Math.floor(Math.floor(ms / 1000) / win.sec) * win.sec);
    }
    return win.unit === 'w' ? PLACEHOLDER_W : PLACEHOLDER_D;
  }

  /* branchOf —— 照抄 limiter.go:220-256 的 switch 顺序。顺序即语义：
     metric=token 的规则永远走 tk，其 algorithm 字段不参与 key 选择。 */
  function branchOf(draft) {
    var algo = draft.algorithm || S.DEFAULTS.algorithm;
    if (draft.type === 'rate' && draft.metric === 'token') {
      return { id: 'tk', prefix: K.prefixTokenLedger, boundary: true, lua: 'tkadmit' };
    }
    if (draft.type === 'concurrency') {
      return { id: 'cc', prefix: K.prefixConcurrency, boundary: false, lua: 'conc' };
    }
    if (algo === 'token_bucket') {
      return { id: 'tb', prefix: K.prefixTokenBucket, boundary: false, lua: 'tb' };
    }
    if (algo === 'sliding_window') {
      return { id: 'rl_sliding', prefix: K.prefixRate, boundary: true, lua: 'sliding' };
    }
    return { id: 'rl_fixed', prefix: K.prefixRate, boundary: true, lua: 'fixed' };
  }

  /* sampleKey —— 返回 { key, branch, segments, natural, scan }。
     key 为 null 表示窗口未解析成功、无法给出形态（不猜）。 */
  function sampleKey(draft, biz, nowMs) {
    var dims = (draft.dimensions || []).slice();
    if (dims.length === 0) return null;
    var br = branchOf(draft);
    var win = RV.parseWindow(draft.window);
    var vals = valsOf(dims, biz);

    var segs = [br.prefix];
    if (br.id === 'cc') segs.push(safeValShape(biz || 'order')); // 首段 biz（key.go:129）
    segs.push(dims.join(K.dimJoin));
    segs.push(vals.join(K.valJoin));

    if (br.boundary) {
      if (!win.ok) return { key: null, branch: br, natural: false, scan: null };
      segs.push(String(win.sec));       // 纯解析结果，不涉时区，始终显示真值
      segs.push(boundarySeg(win, nowMs));
    }

    // SCAN 通配符：前缀段不含任何时区成分，故通配符前缀完全准确
    var scanSegs = segs.slice(0, br.boundary ? segs.length - 1 : segs.length);
    return {
      key: segs.join(K.sep),
      branch: br,
      natural: br.boundary && win.ok ? win.natural : false,
      scan: scanSegs.join(K.sep) + K.sep + '*'
    };
  }
  function copyBtn(text, label) {
    return U.el('button', {
      class: 'affix-btn', type: 'button', 'aria-label': label,
      onclick: function () {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(String(text)).then(function () {
            w.API.toast('ok', '已复制到剪贴板');
          }, function () {
            w.API.toast('warn', '浏览器拒绝了剪贴板写入，请手动选择复制');
          });
        } else {
          w.API.toast('warn', '当前浏览器不支持剪贴板 API，请手动选择复制');
        }
      }
    }, U.icon('i-copy'));
  }

  var BRANCH_DESC = {
    tk: 'Token 账本（metric=token 优先于 algorithm 判定）',
    cc: '并发闸门（无窗口段，键在请求结束时释放）',
    tb: '令牌桶（持久桶，无窗口边界段）',
    rl_sliding: '滑动窗口（ZSet，载荷 Window = 窗口毫秒数）',
    rl_fixed: '固定窗口（INCR，载荷 Expire = 窗口秒数 + 60）'
  };

  /* render —— 只读预览。同一个 key 形态下 sliding 与 fixed 必须可区分：
     两者 key 一致但 Lua 载荷不同，故分支说明里点明载荷差异。 */
  function render(draft, biz) {
    var box = U.el('div', { class: 'stack' });
    var info = sampleKey(draft, biz);

    if (!info) {
      box.appendChild(U.el('p', { class: 'helper', text: '选择限流维度后显示计数键预览。' }));
      return box;
    }

    box.appendChild(U.el('p', {
      class: 'helper',
      text: '分支：' + (BRANCH_DESC[info.branch.id] || info.branch.id)
    }));

    if (info.key === null) {
      box.appendChild(U.el('p', {
        class: 'helper helper--err',
        text: '时间窗口非法，无法推导计数键。修正窗口后此处会显示可复制的键形态。'
      }));
      return box;
    }

    var row = U.el('div', { class: 'field-affix' }, [
      U.el('code', {
        class: 'field field--mono',
        style: 'display:block; overflow-wrap:anywhere',
        text: info.key
      }),
      copyBtn(info.key, '复制计数键')
    ]);
    box.appendChild(row);

    if (info.natural) {
      box.appendChild(U.el('div', { class: 'note note--accent' }, [
        U.icon('i-info'),
        U.el('span', {
          text: '自然窗口按业务时区零点对齐，边界值取决于网关的 TZ_OFFSET_SECONDS，' +
            '控制台无法推算。用下面的通配符在 Redis 中查找实际键：'
        })
      ]));
      box.appendChild(U.el('div', { class: 'field-affix' }, [
        U.el('code', {
          class: 'field field--mono',
          style: 'display:block; overflow-wrap:anywhere',
          text: 'SCAN 0 MATCH ' + info.scan + ' COUNT 100'
        }),
        copyBtn('SCAN 0 MATCH ' + info.scan + ' COUNT 100', '复制 SCAN 命令')
      ]));
    }
    return box;
  }

  w.RulePreview = {
    sampleKey: sampleKey,
    branchOf: branchOf,
    safeValShape: safeValShape,
    boundarySeg: boundarySeg,
    render: render,
    PLACEHOLDER_D: PLACEHOLDER_D,
    PLACEHOLDER_W: PLACEHOLDER_W
  };
})(window);
