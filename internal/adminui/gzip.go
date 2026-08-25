package adminui

import (
	"bytes"
	"compress/gzip"
	"path"
	"strconv"
	"strings"
)

// 静态资产的预压缩。
//
// 压缩时机：进程启动时压一次（NewHandler），不是每请求实时压。
// 资产在编译期就固定了，实时压缩纯属浪费 CPU —— 而网关的 CPU 要留给限流路径。
//
// 为什么不把 .gz 文件提交进仓库再 embed：那需要引入一个构建步骤
// （生成 .gz 并保证与源文件同步），而本项目的硬约束是零构建链。
// 一旦有人改了 js 却忘记重新生成 .gz，就会出现「压缩版是旧代码」这种
// 极难排查的故障。启动时压缩没有这个不一致风险，代价是启动时多几十毫秒，
// 运行期性质与预压缩完全相同（每请求零压缩开销）。

// gzipMinSize 小于该体积不压缩。
//
// gzip 有约 18 字节固定头尾开销，小文件压完可能反而更大；
// 且几百字节的响应压缩收益完全被 TCP 首包吃掉。
const gzipMinSize = 512

// compressible 只压缩文本类型。
//
// 已压缩格式（png/jpg/woff2/gz）再压一遍是净亏损：CPU 花了、体积没降、
// 有时还会变大。这里用白名单而非黑名单 —— 未知类型默认不压，
// 免得将来加了二进制资产被误压。
func compressible(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".css", ".js", ".svg", ".json", ".txt", ".map":
		return true
	default:
		return false
	}
}

// gzipBytes 用最高压缩级别压一次。
//
// 用 BestCompression 而不是 DefaultCompression：这是一次性成本，
// 换来的是每个请求都省下的传输量，没有理由不压到最狠。
func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// acceptsGzip 判定客户端是否真的接受 gzip。
//
// 这里刻意不用 strings.Contains(header, "gzip") —— 那是个真实的坑：
//   - "gzip;q=0" 表示客户端明确**拒绝** gzip，子串匹配会当成接受；
//   - "identity" 之外的 "*" 通配也代表接受，子串匹配会漏掉。
//
// 发错了的后果是给不支持的客户端喂压缩字节，页面直接乱码打不开，
// 所以这里按 RFC 9110 解析 q 值。
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	star := false
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		token, params, _ := strings.Cut(part, ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "gzip" && token != "*" {
			continue
		}
		// q=0 表示不可接受；缺省 q 视为 1
		acceptable := true
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(p, "=")
			if !ok || strings.ToLower(strings.TrimSpace(k)) != "q" {
				continue
			}
			if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q <= 0 {
				acceptable = false
			}
		}
		if token == "gzip" {
			// 显式提到 gzip 时以它为准，不再看通配
			return acceptable
		}
		if acceptable {
			star = true
		}
	}
	return star
}
