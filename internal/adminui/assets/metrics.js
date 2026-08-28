/* ============================================================================
   指标适配层（可插拔）—— DESIGN.md §6.4
   ----------------------------------------------------------------------------
   数据源决策：不从 obs 端口（29091）取 /metrics。该端口无鉴权且全网暴露，
   跨端口取数要开 CORS，等于让任意网页读运行指标；且同一界面三模块要令牌、
   监控裸奔，鉴权边界不成立。指标必须走 admin 端口的鉴权端点。

   当前状态：admin 端口已提供受鉴权的 GET admin/metrics，原样透传 Prometheus
   文本（后端刻意不预聚合 —— 速率需要采样基线，而网关多实例部署时把基线放
   某个实例的内存会让看板每次轮询拿到不同基线算出的数字）。因此解析、一阶
   差分、首帧基线、直方图插值、负值丢弃全部留在本文件，是正确的归属。

   两个必须处理的分支（否则看板会说谎）：
   1. Counter 是单调累计值，必须一阶差分再除采样间隔才是速率。
      首帧只存基线不出图 —— KPI 显示 "—" 而非 0，否则误报「零流量」。
   2. 网关重启导致差分为负 → 判负丢弃该次采样并提示基线已重置，
      否则 QPS 会出现巨大负值尖刺。
   ========================================================================== */
(function (w) {
  'use strict';

  // admin 端口指标端点已就绪（internal/admin/metrics.go）
  var ENDPOINT_READY = true;
  var ENDPOINT = 'admin/metrics';
  var WINDOW = 60;   // 滚动窗口采样点数
  var BUCKETS = [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30];

  /* TTFT 与端到端延迟的桶边界不同，必须分别持有。
     两者关注区间不重叠：延迟要覆盖 30s 长流，TTFT 的判别区间集中在 0.1~5s。
     用 BUCKETS 去解析 TTFT 的 le 标签会导致大部分桶匹配不上（取到 0），
     算出的分位数看起来是个正常数字，实际完全错误 —— 这类错误不会报错，
     只会静默给出可信外观的假数据，所以必须与后端 obs.TTFTBounds 严格对齐。 */
  var TTFT_BUCKETS = [0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10, 20];

  /* ---- Prometheus 文本解析 ----
     行格式 name{k="v",...} value；跳过 # 开头行；无标签指标（如 uptime）也要支持。 */
  function parsePrometheus(text) {
    var out = {};
    var lines = String(text || '').split('\n');
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].trim();
      if (!line || line.charAt(0) === '#') continue;
      var name, labels = {}, rest;
      var brace = line.indexOf('{');
      var sp;
      if (brace === -1) {
        sp = line.indexOf(' ');
        if (sp === -1) continue;
        name = line.slice(0, sp);
        rest = line.slice(sp + 1);
      } else {
        name = line.slice(0, brace);
        var close = line.lastIndexOf('}');
        if (close === -1) continue;
        labels = parseLabels(line.slice(brace + 1, close));
        rest = line.slice(close + 1).trim();
      }
      var v = parseFloat(rest);
      if (!isFinite(v)) continue;
      (out[name] = out[name] || []).push({ labels: labels, value: v });
    }
    return out;
  }

  function parseLabels(s) {
    var res = {};
    var re = /([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"/g;
    var m;
    while ((m = re.exec(s)) !== null) {
      res[m[1]] = m[2].replace(/\\"/g, '"').replace(/\\n/g, '\n').replace(/\\\\/g, '\\');
    }
    return res;
  }

  function sum(samples, filter) {
    if (!samples) return 0;
    var t = 0;
    for (var i = 0; i < samples.length; i++) {
      if (!filter || filter(samples[i].labels)) t += samples[i].value;
    }
    return t;
  }

  function groupSum(samples, key) {
    var m = {};
    (samples || []).forEach(function (s) {
      var k = s.labels[key];
      if (k === undefined) return;
      m[k] = (m[k] || 0) + s.value;
    });
    return m;
  }

  /* histQuantile —— 累积桶线性插值。
     调用方必须传入已做过差分的桶（窗口内增量），否则算出来是进程启动至今的
     全历史分位，故障中会被历史数据稀释、完全失去意义。

     返回 { value, lo, hi, exact } 而不是单个数字。
     原因是**分辨率上限由桶边界决定，而不是由插值精度决定**：
     桶边界是 1/5/10/25/50/100/250/500ms/1/2.5/5/10/30s，每个区间宽度占其
     上界的 50%-100%。若 P99 落在 (1s, 2.5s] 这一档，线性插值给出的 "2479ms"
     其真实不确定性是 ±1500ms —— 显示到 0.1ms 会暗示比实际高四个数量级的精度，
     而 SRE 会拿这个数字去对 SLO。所以把所在区间一并返回，由 UI 明示误差。

     exact=true 仅当该分位恰好落在桶边界上（区间退化为一点），此时无插值误差。

     bounds 可选，默认为端到端延迟的 BUCKETS。TTFT 必须显式传 TTFT_BUCKETS —— 
     桶边界与被解析的指标不匹配时结果是静默错误，不是异常。 */
  function histQuantile(cumulative, q, bounds) {
    var bk = bounds || BUCKETS;
    var total = cumulative.length ? cumulative[cumulative.length - 1] : 0;
    if (!(total > 0)) return null;
    var target = total * q;
    var prevCount = 0, prevBound = 0;
    for (var i = 0; i < bk.length; i++) {
      var c = cumulative[i] || 0;
      if (c >= target) {
        var span = c - prevCount;
        if (span <= 0) {
          // 该桶内无样本增量，分位点就落在桶边界上，没有插值成分
          return { value: bk[i], lo: bk[i], hi: bk[i], exact: true };
        }
        var frac = (target - prevCount) / span;
        return {
          value: prevBound + (bk[i] - prevBound) * frac,
          lo: prevBound, hi: bk[i], exact: false
        };
      }
      prevCount = c;
      prevBound = bk[i];
    }
    // 超出最后一个有限桶（落在 +Inf 桶）：只能给下界，上界未知
    var last = bk[bk.length - 1];
    return { value: last, lo: last, hi: Infinity, exact: false };
  }

  /* cumulativeOf —— 按给定桶边界提取累积桶计数。
     le 标签的字符串形式必须与后端渲染完全一致，否则匹配不上会静默取 0。 */
  function cumulativeOf(samples, bounds) {
    return bounds.map(function (b) {
      var key = String(b);
      return sum(samples, function (l) { return l.le === key; });
    });
  }

  /* ---- 状态机：持有基线与滚动窗口 ---- */
  function createStore() {
    return {
      prev: null,          // 上一帧原始累计值 + 时间戳
      series: { pass: [], reject: [], labels: [] },
      kpi: null,           // null 表示尚无速率可算（首帧），KPI 显示 —
      buckets: null,       // 窗口内增量桶
      restarted: false,    // 本次采样检出计数器回退
      sampleCount: 0
    };
  }

  function snapshotOf(parsed) {
    var req = parsed['unirate_requests_total'] || [];
    var rej = parsed['unirate_rejected_total'] || [];
    var bucket = parsed['unirate_request_duration_seconds_bucket'] || [];
    var tokKind = parsed['unirate_tokens_by_kind_total'] || [];
    return {
      at: Date.now(),
      requests: sum(req),
      rejected: sum(rej),
      settled: sum(parsed['unirate_tokens_settled_total']),
      degraded: sum(parsed['unirate_degraded_decisions_total']),
      inFlight: sum(parsed['unirate_concurrency_in_flight']),
      breaker: sum(parsed['unirate_redis_breaker_open']),
      version: sum(parsed['unirate_config_version']),
      buckets: cumulativeOf(bucket, BUCKETS),

      // —— 新增指标 ——
      // RPM/TPM 是后端滚动窗口 gauge，**不参与差分**：它们已经是速率，
      // 再做一阶差分会得到「速率的变化量」，那是另一个物理量。
      rpm: sum(parsed['unirate_requests_per_minute']),
      tpm: sum(parsed['unirate_tokens_per_minute']),
      rpmByBiz: groupSum(parsed['unirate_requests_per_minute'], 'biz'),
      tpmByBiz: groupSum(parsed['unirate_tokens_per_minute'], 'biz'),

      ttftBuckets: cumulativeOf(parsed['unirate_ttft_seconds_bucket'] || [], TTFT_BUCKETS),
      ttftCount: sum(parsed['unirate_ttft_seconds_count']),
      upstreamBuckets: cumulativeOf(parsed['unirate_upstream_duration_seconds_bucket'] || [], BUCKETS),
      activeStreams: sum(parsed['unirate_sse_streams_active']),
      sseFrames: sum(parsed['unirate_sse_frames_total']),
      promptTokens: sum(tokKind, function (l) { return l.kind === 'prompt'; }),
      completionTokens: sum(tokKind, function (l) { return l.kind === 'completion'; }),
      rejectByBiz: groupSum(rej, 'biz'),
      rejectRuleByBiz: (function () {
        var m = {};
        rej.forEach(function (s) {
          var b = s.labels.biz, r = s.labels.rule;
          if (!b || !r) return;
          if (!m[b] || m[b].value < s.value) m[b] = { rule: r, value: s.value };
        });
        return m;
      })(),
      watermarks: (parsed['unirate_rule_watermark_ratio_percent'] || []).map(function (s) {
        return { biz: s.labels.biz || '—', rule: s.labels.rule || '—', pct: s.value };
      })
    };
  }

  /* ingest —— 把一帧原始文本并入状态机。返回是否产生了可出图的速率。 */
  function ingest(store, text) {
    var cur = snapshotOf(parsePrometheus(text));
    var prev = store.prev;
    store.sampleCount++;
    store.restarted = false;
    store.raw = cur;

    if (!prev) {
      // 首帧只存基线，不出图：counter 的绝对值不是速率
      store.prev = cur;
      return false;
    }
    var dt = (cur.at - prev.at) / 1000;
    if (dt <= 0) { store.prev = cur; return false; }

    var dReq = cur.requests - prev.requests;
    var dRej = cur.rejected - prev.rejected;
    var dSettled = cur.settled - prev.settled;
    var dDegraded = cur.degraded - prev.degraded;

    // 计数器回退 = 网关重启。丢弃该次采样并把新值当基线，
    // 否则 QPS 出现巨大负值尖刺。
    // 新增的累计计数器同样要纳入判负，否则重启后派生速率出现负值尖刺。
    // RPM/TPM/inFlight/activeStreams 是 gauge，本来就会降，不能参与此判断。
    if (dReq < 0 || dRej < 0 || dSettled < 0 || dDegraded < 0 ||
        cur.sseFrames < prev.sseFrames || cur.ttftCount < prev.ttftCount) {
      store.prev = cur;
      store.restarted = true;
      store.kpi = null;
      return false;
    }

    var diffBuckets = function (curB, prevB) {
      return curB.map(function (v, i) {
        var d = v - (prevB[i] || 0);
        return d < 0 ? 0 : d;
      });
    };
    var dBuckets = diffBuckets(cur.buckets, prev.buckets);
    store.buckets = dBuckets;

    var qps = dReq / dt;
    var rps = dRej / dt;
    var p99 = histQuantile(dBuckets, 0.99);

    // TTFT 与上游延迟同样必须先差分再求分位，否则拿到的是进程启动至今的
    // 全历史分位，故障期间会被历史数据稀释。桶边界各自传入，不能混用。
    var dTtft = diffBuckets(cur.ttftBuckets, prev.ttftBuckets);
    var dUp = diffBuckets(cur.upstreamBuckets, prev.upstreamBuckets);
    var ttftP50 = histQuantile(dTtft, 0.50, TTFT_BUCKETS);
    var ttftP95 = histQuantile(dTtft, 0.95, TTFT_BUCKETS);
    var upP99 = histQuantile(dUp, 0.99);

    var prevKpi = store.kpi;
    store.kpi = {
      qps: qps,
      rejectRate: dReq > 0 ? (dRej / dReq) * 100 : 0,
      p99ms: p99 === null ? null : p99.value * 1000,
      // 分位数所在桶区间，供 UI 明示插值误差（见 histQuantile 注释）
      p99Range: p99 === null ? null : {
        lo: p99.lo * 1000, hi: p99.hi * 1000, exact: p99.exact
      },
      tokensPerSec: dSettled / dt,
      inFlight: cur.inFlight,
      breaker: cur.breaker,
      version: cur.version,
      degradedGrowing: dDegraded > 0,

      // RPM/TPM 直接取后端滚动窗口值，不做差分（它们已是速率）
      rpm: cur.rpm,
      tpm: cur.tpm,
      rpmByBiz: cur.rpmByBiz,
      tpmByBiz: cur.tpmByBiz,

      ttftP50ms: ttftP50 === null ? null : ttftP50.value * 1000,
      ttftP95ms: ttftP95 === null ? null : ttftP95.value * 1000,
      ttftP95Range: ttftP95 === null ? null : {
        lo: ttftP95.lo * 1000, hi: ttftP95.hi * 1000, exact: ttftP95.exact
      },
      upstreamP99ms: upP99 === null ? null : upP99.value * 1000,
      activeStreams: cur.activeStreams,
      framesPerSec: (cur.sseFrames - prev.sseFrames) / dt,
      promptTokens: cur.promptTokens,
      completionTokens: cur.completionTokens,
      prev: prevKpi ? {
        qps: prevKpi.qps, rejectRate: prevKpi.rejectRate, p99ms: prevKpi.p99ms,
        rpm: prevKpi.rpm, tpm: prevKpi.tpm, ttftP95ms: prevKpi.ttftP95ms
      } : null
    };

    push(store.series.pass, qps);
    push(store.series.reject, rps);
    push(store.series.labels, w.U.clockTime(new Date(cur.at)));
    store.prev = cur;
    return true;
  }

  function push(arr, v) {
    arr.push(v);
    while (arr.length > WINDOW) arr.shift();
  }

  /* fetchMetrics —— 同源受鉴权取数。
     ENDPOINT_READY 保留为开关：端点缺失时（如旧版网关）给出可诊断的
     错误说明，而不是让上层收到一个语义不明的网络失败。 */
  function fetchMetrics() {
    if (!ENDPOINT_READY) {
      return Promise.reject(new w.API.ApiError(
        'pending', 0,
        '指标端点待接入：admin 端口尚未提供 ' + ENDPOINT +
        '。obs 端口（29091）无鉴权且全网暴露，跨端口取数会让任意网页读到运行指标，已否决该方案。',
        null));
    }
    var token = w.API.session.token();
    var headers = { 'Accept': 'text/plain' };
    if (token) headers['Authorization'] = 'Bearer ' + token;
    return fetch(ENDPOINT, { headers: headers, cache: 'no-store', credentials: 'omit' })
      .then(function (res) {
        if (!res.ok) {
          if (res.status === 401) w.App && w.App.onSessionExpired && w.App.onSessionExpired();
          throw new w.API.ApiError('http', res.status, '指标拉取失败 HTTP ' + res.status, null);
        }
        return res.text();
      }, function (e) {
        throw new w.API.ApiError('network', 0, e && e.message ? e.message : '指标端点不可达', null);
      });
  }

  w.Metrics = {
    parsePrometheus: parsePrometheus,
    histQuantile: histQuantile,
    createStore: createStore,
    ingest: ingest,
    fetchMetrics: fetchMetrics,
    cumulativeOf: cumulativeOf,
    BUCKETS: BUCKETS,
    TTFT_BUCKETS: TTFT_BUCKETS,
    WINDOW: WINDOW,
    endpointReady: function () { return ENDPOINT_READY; },
    endpointPath: function () { return ENDPOINT; }
  };
})(window);
