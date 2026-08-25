package adminui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func getEnc(t *testing.T, h *Handler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestGzipServedWhenAccepted 带 Accept-Encoding: gzip 时必须返回压缩响应，
// 且解压后必须与原文逐字节一致。
//
// 只断言 Content-Encoding 头是不够的：头对了但字节错了（压错文件、
// 压缩流截断）会让页面白屏，而头看起来完全正常。必须解压比对。
func TestGzipServedWhenAccepted(t *testing.T) {
	h := newH(t)
	for _, name := range []string{"/app.js", "/tokens.css", "/index.html", "/icons.svg"} {
		w := getEnc(t, h, name, "gzip")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s 期望 200，实际 %d", name, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("%s 期望 Content-Encoding: gzip，实际 %q", name, got)
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Errorf("%s 响应不是合法 gzip 流: %v", name, err)
			continue
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Errorf("%s gzip 流解压失败（可能被截断）: %v", name, err)
			continue
		}
		want := h.files[strings.TrimPrefix(name, "/")]
		if !bytes.Equal(plain, want) {
			t.Errorf("%s 解压后与原文不一致（%d vs %d 字节）", name, len(plain), len(want))
		}
	}
}

// TestIdentityServedWhenGzipNotAccepted 不带 Accept-Encoding 时必须返回原文。
// 给不支持 gzip 的客户端发压缩字节，页面直接乱码打不开。
func TestIdentityServedWhenGzipNotAccepted(t *testing.T) {
	h := newH(t)
	for _, name := range []string{"/app.js", "/index.html"} {
		w := getEnc(t, h, name, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s 期望 200，实际 %d", name, w.Code)
			continue
		}
		if enc := w.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("%s 未声明接受 gzip，却返回了 Content-Encoding: %q", name, enc)
		}
		want := h.files[strings.TrimPrefix(name, "/")]
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("%s 未压缩响应与原文不一致", name)
		}
	}
}

// TestVaryHeaderAlwaysPresent Vary: Accept-Encoding 在两条路径上都必须存在。
//
// 缺它会让中间层缓存把压缩响应回放给不支持 gzip 的客户端 ——
// 内网常有企业代理，这是真实会发生的故障，且间歇性、极难复现。
func TestVaryHeaderAlwaysPresent(t *testing.T) {
	h := newH(t)
	for _, accept := range []string{"gzip", "", "identity"} {
		w := getEnc(t, h, "/app.js", accept)
		vary := w.Header().Get("Vary")
		if !strings.Contains(vary, "Accept-Encoding") {
			t.Errorf("Accept-Encoding=%q 时缺少 Vary: Accept-Encoding（实际 %q）", accept, vary)
		}
	}
}

// TestGzipRejectedWhenQZero "gzip;q=0" 表示客户端明确拒绝 gzip。
//
// 这是 strings.Contains(header,"gzip") 式实现的经典错误：
// 子串匹配会把「拒绝」读成「接受」，然后给客户端发它读不了的字节。
func TestGzipRejectedWhenQZero(t *testing.T) {
	h := newH(t)
	for _, accept := range []string{"gzip;q=0", "gzip; q=0", "gzip;q=0.0", "deflate, gzip;q=0"} {
		w := getEnc(t, h, "/app.js", accept)
		if enc := w.Header().Get("Content-Encoding"); enc == "gzip" {
			t.Errorf("Accept-Encoding=%q 明确拒绝 gzip，却仍返回了压缩响应", accept)
		}
	}
}

// TestGzipAcceptedViaWildcardAndQValue 通配与带 q 值的正常形式都应接受
func TestGzipAcceptedViaQValueForms(t *testing.T) {
	h := newH(t)
	for _, accept := range []string{"*", "gzip;q=1.0", "br, gzip", "GZIP", "deflate, gzip;q=0.8"} {
		w := getEnc(t, h, "/app.js", accept)
		if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
			t.Errorf("Accept-Encoding=%q 应被视为接受 gzip，实际 Content-Encoding=%q", accept, enc)
		}
	}
}

// TestContentLengthMatchesGzipBody Content-Length 必须与实际写出的字节数一致。
// 不一致会让客户端挂起等待剩余数据，或提前截断。
func TestContentLengthMatchesGzipBody(t *testing.T) {
	h := newH(t)
	w := getEnc(t, h, "/app.js", "gzip")
	cl := w.Header().Get("Content-Length")
	n, err := strconv.Atoi(cl)
	if err != nil {
		t.Fatalf("Content-Length 不是合法整数: %q", cl)
	}
	if n != w.Body.Len() {
		t.Errorf("Content-Length=%d 与实际响应体 %d 字节不一致", n, w.Body.Len())
	}
}

// TestHeadRequestSendsNoBody HEAD 必须只回头不回体，但头要与 GET 一致
func TestHeadRequestSendsNoBody(t *testing.T) {
	h := newH(t)
	r := httptest.NewRequest("HEAD", "/app.js", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Error("HEAD 响应的 Content-Encoding 应与 GET 一致")
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回响应体，实际 %d 字节", w.Body.Len())
	}
}

// TestSecurityHeadersPresentOnGzipPath 压缩路径不得漏掉任何安全头。
//
// 新增一条响应分支最容易犯的错就是漏设头 —— 压缩分支绕过了 ServeContent，
// 若安全头设置在错误的位置，gzip 请求会拿到一个没有 CSP 的响应。
func TestSecurityHeadersPresentOnGzipPath(t *testing.T) {
	h := newH(t)
	w := getEnc(t, h, "/index.html", "gzip")
	for _, k := range []string{
		"Content-Security-Policy", "X-Content-Type-Options",
		"X-Frame-Options", "Referrer-Policy", "X-Robots-Tag",
	} {
		if w.Header().Get(k) == "" {
			t.Errorf("gzip 路径缺少安全头 %s", k)
		}
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("gzip 路径的 Content-Type 应仍为原始类型，实际 %q", ct)
	}
}

// TestOnlyTextAssetsPrecompressed 只压文本类型；小文件不压。
func TestOnlyTextAssetsPrecompressed(t *testing.T) {
	h := newH(t)
	for name := range h.gz {
		if !compressible(name) {
			t.Errorf("非文本资产 %s 被预压缩了", name)
		}
		if len(h.files[name]) < gzipMinSize {
			t.Errorf("小文件 %s（%d 字节）被预压缩，收益为负", name, len(h.files[name]))
		}
		// 压完变大就不该保留
		if len(h.gz[name]) >= len(h.files[name]) {
			t.Errorf("%s 压缩后未变小（%d >= %d），不该保留压缩副本",
				name, len(h.gz[name]), len(h.files[name]))
		}
	}
}

// TestCompressionActuallyReducesPayload 压缩必须带来实质收益。
//
// 这条守的是「gzip 接上了但没起作用」：例如误用了 gzip.NoCompression，
// 或压缩层被某次重构绕过。总量降不到一半就说明配置错了。
func TestCompressionActuallyReducesPayload(t *testing.T) {
	h := newH(t)
	var raw, enc int
	for name, body := range h.files {
		raw += len(body)
		if g, ok := h.gz[name]; ok {
			enc += len(g)
		} else {
			enc += len(body)
		}
	}
	if raw == 0 {
		t.Fatal("没有任何资产")
	}
	ratio := float64(enc) / float64(raw)
	t.Logf("未压缩 %d 字节 → 压缩后 %d 字节（%.0f%%，压缩比 %.1fx）",
		raw, enc, ratio*100, 1/ratio)
	if ratio > 0.5 {
		t.Errorf("压缩后仍占原体积 %.0f%%，预期应低于 50%%", ratio*100)
	}
	// DESIGN.md §9.1 的目标：传输体积 < 120KB
	if enc > 120*1024 {
		t.Errorf("压缩后传输体积 %d 字节仍超过 120KB 目标", enc)
	}
}
