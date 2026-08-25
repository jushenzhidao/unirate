// Package adminui 内嵌管理控制台的静态资产。
//
// 落地方式：go:embed 打进二进制，挂在 admin 端口（与 /admin/* API 同源）。
// 同源是刻意的 —— 跨端口取数要开 CORS，等于把管理面数据暴露给任意网页。
//
// 零构建 / 零 npm / 零 CDN 外链：内网离线环境必须可用，因此图标是内联 SVG
// sprite、图表是纯手绘 SVG，不引入任何前端包。
package adminui

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// Assets 返回静态资产的子文件系统
func Assets() (fs.FS, error) { return fs.Sub(assetsFS, "assets") }

// buildTime 用于 Last-Modified。取进程启动时刻而非零值：
// 零值时间会让部分代理把响应判定为「永久陈旧」而反复回源。
var buildTime = time.Now()

// Handler 静态资产处理器。
//
// 只服务白名单内的文件名，不做目录遍历式查找 —— 这里刻意不用
// http.FileServer：它会对目录返回索引页、对 "/x/../y" 做重定向，
// 在管理面上多一类不必要的行为面。白名单是可穷举的（8 个文件），
// 直接查表更简单也更可控。
type Handler struct {
	files map[string][]byte
	// gz 预压缩副本。只对文本类型且压缩确有收益的资产存在，
	// 因此取值前必须判断存在性 —— 不能假设每个 files 键都有对应的 gz。
	gz map[string][]byte
}

// index 是未命中白名单时的回退文件（SPA 用 hash 路由，无需服务端路由表）
const index = "index.html"

// NewHandler 读出全部资产到内存。
//
// 资产总量在百 KB 级，一次性读入避免每个请求都走 embed.FS 的解压路径；
// 且内容在编译期固定，不存在需要热更新的场景。
func NewHandler() (*Handler, error) {
	sub, err := Assets()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(sub, p)
		if rerr != nil {
			return rerr
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 预压缩：启动时压一次，运行期直接吐字节，每请求零压缩开销。
	gz := map[string][]byte{}
	for name, raw := range files {
		if len(raw) < gzipMinSize || !compressible(name) {
			continue
		}
		enc, cerr := gzipBytes(raw)
		if cerr != nil {
			// 压缩失败不该让控制台不可用 —— 退回原文照样能服务，
			// 只是体积大些。可用性优先于传输优化。
			continue
		}
		// 压完反而变大就丢掉：这种情况下发压缩版是纯亏
		if len(enc) >= len(raw) {
			continue
		}
		gz[name] = enc
	}
	return &Handler{files: files, gz: gz}, nil
}

// contentType 显式查表而非依赖 mime.TypeByExtension 的系统注册表。
//
// Windows 上 .js 的注册值可能是 text/plain（注册表被其他软件改写），
// 那会让浏览器拒绝执行脚本，整个控制台白屏 —— 这是实测存在过的坑，
// 不能把可用性交给宿主机的 mime 配置。
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		if t := mime.TypeByExtension(path.Ext(name)); t != "" {
			return t
		}
		return "application/octet-stream"
	}
}

// resolve 把请求路径映射到资产名。
//
// 只接受单层文件名：资产是平铺的，任何带 "/" 的路径都不可能命中，
// 直接落回 index 而不是去拼路径 —— 从根上消除路径遍历。
func (h *Handler) resolve(urlPath string) string {
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		return index
	}
	if strings.Contains(name, "/") {
		return index
	}
	if _, ok := h.files[name]; ok {
		return name
	}
	return index
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := h.resolve(r.URL.Path)
	body, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	hd := w.Header()
	hd.Set("Content-Type", contentType(name))
	// 资产随二进制版本走，但内网升级后不能让用户按 Ctrl+F5 才看到新版，
	// 所以用 no-cache（允许缓存但每次校验）而不是 immutable。
	hd.Set("Cache-Control", "no-cache")
	// 管理面不该被任何页面嵌套或被外部引用，逐条收紧：
	// CSP 的 'unsafe-inline' 是必需的 —— index.html 里有一段必须在首绘前
	// 同步执行的主题脚本（防白闪），拆成外链文件就失去了同步性。
	// 其余方向全部锁死：无外链、无 eval、无框架嵌套、无表单外发。
	hd.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self'; "+
			"img-src 'self' data:; connect-src 'self'; font-src 'self'; "+
			"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	// 管理页面不得进入搜索引擎或任何缓存归档
	hd.Set("X-Robots-Tag", "noindex, nofollow")

	// Vary 必须无条件设置，而不是只在实际压缩时设置。
	//
	// 同一个 URL 会因 Accept-Encoding 不同而返回不同字节。若缺 Vary，
	// 中间层缓存（企业代理是内网部署的常态）会把某次压缩响应回放给
	// 不支持 gzip 的客户端，页面直接乱码 —— 而且是间歇性的、极难复现。
	// 只在压缩分支设置同样有这个洞：未压缩的那次响应会被当成
	// 「与编码无关」而缓存下来，再喂给支持 gzip 的客户端也就罢了，
	// 反过来则致命。所以放在分支之前，两条路径都带上。
	hd.Add("Vary", "Accept-Encoding")

	if enc, ok := h.gz[name]; ok && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		hd.Set("Content-Encoding", "gzip")
		// 这里刻意不走 ServeContent：它会按传入内容算 Range 与 ETag，
		// 而 Range 的字节区间语义应针对**解码后**的实体。让它对压缩字节
		// 做 Range 会返回错误的片段。预压缩资产不需要 Range 支持，
		// 直接整体写出，并显式声明不支持 Range 以免客户端误发。
		hd.Set("Content-Length", strconv.Itoa(len(enc)))
		hd.Set("Accept-Ranges", "none")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(enc)
		}
		return
	}

	// bytes.Reader 而非 strings.NewReader(string(body))：后者会为每个请求
	// 复制一份资产副本，白复制百 KB。ServeContent 需要 ReadSeeker 以支持 Range。
	http.ServeContent(w, r, name, buildTime, bytes.NewReader(body))
}
