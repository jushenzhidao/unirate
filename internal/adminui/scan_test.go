package adminui

// 资产的静态扫描辅助。
//
// 为什么需要真正剥掉注释与字符串，而不是直接对全文 strings.Contains：
// 本包的注释里大量出现「为什么不这么做」的说明（例如「绝不能走 innerHTML」、
// 「obs 端口 29091 已否决」）。对全文匹配会把这些说明本身判成违规 ——
// 那种检查只会逼着人删注释，而不是修代码，等于把最有价值的决策记录赶走。
// 所以扫描器必须能区分「代码里真的这么写了」和「注释里说了这个词」。

// stripJSCode 剥掉 JS 的注释与字符串字面量内容，只留下可执行结构。
//
// 手写小扫描器而不是用正则：正则处理不了 "http://x" 里的 // 与
// 注释中出现的引号这类交叉情况，会把后面整段代码误判掉。
func stripJSCode(src string) string {
	var out []byte
	const (
		code = iota
		lineComment
		blockComment
		single
		double
		tmpl
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		next := byte(0)
		if i+1 < len(src) {
			next = src[i+1]
		}
		switch state {
		case code:
			if c == '/' && next == '/' {
				state = lineComment
				i++
			} else if c == '/' && next == '*' {
				state = blockComment
				i++
			} else if c == '\'' {
				state = single
				out = append(out, c)
			} else if c == '"' {
				state = double
				out = append(out, c)
			} else if c == '`' {
				state = tmpl
				out = append(out, c)
			} else {
				out = append(out, c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out = append(out, c)
			}
		case blockComment:
			if c == '*' && next == '/' {
				state = code
				i++
			}
		case single, double, tmpl:
			// 字符串内容不参与结构判断，但保留引号以便看出「这里是个字符串」
			if c == '\\' {
				i++ // 跳过转义字符，避免把 \" 当成结束引号
				continue
			}
			if (state == single && c == '\'') || (state == double && c == '"') ||
				(state == tmpl && c == '`') {
				state = code
				out = append(out, c)
			}
		}
	}
	return string(out)
}

// stripJSComments 只剥注释，**保留字符串字面量内容**。
//
// 与 stripJSCode 的分工要分清，用错会得到假结论：
//   - stripJSCode 连字符串内容一起剥 → 用于「代码里是否真的调用了某个危险
//     API」这类结构判断（如 .innerHTML=），此时字符串里出现同名词不算违规；
//   - stripJSComments 保留字符串 → 用于「某个字面量是否存在」这类判断
//     （如端点路径 'admin/metrics'、路由 '#/rules?biz='、UI 文案）。
//
// 我第一版把后者也用 stripJSCode 扫，结果三条断言全部误报失败 ——
// 它们要找的东西本身就在字符串里，被剥掉了自然找不到。两者都不含注释，
// 所以「注释里提到某词」在任何一种下都不会造成误判。
func stripJSComments(src string) string {
	var out []byte
	const (
		code = iota
		lineComment
		blockComment
		single
		double
		tmpl
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		next := byte(0)
		if i+1 < len(src) {
			next = src[i+1]
		}
		switch state {
		case code:
			switch {
			case c == '/' && next == '/':
				state = lineComment
				i++
			case c == '/' && next == '*':
				state = blockComment
				i++
			case c == '\'':
				state = single
				out = append(out, c)
			case c == '"':
				state = double
				out = append(out, c)
			case c == '`':
				state = tmpl
				out = append(out, c)
			default:
				out = append(out, c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out = append(out, c)
			}
		case blockComment:
			if c == '*' && next == '/' {
				state = code
				i++
			}
		case single, double, tmpl:
			// 与 stripJSCode 的唯一区别：这里把字符串内容写出去
			out = append(out, c)
			if c == '\\' && i+1 < len(src) {
				out = append(out, src[i+1])
				i++
				continue
			}
			if (state == single && c == '\'') || (state == double && c == '"') ||
				(state == tmpl && c == '`') {
				state = code
			}
		}
	}
	return string(out)
}

// stripCSSComments 剥掉 CSS 的块注释
func stripCSSComments(src string) string {
	var out []byte
	for i := 0; i < len(src); i++ {
		if src[i] == '/' && i+1 < len(src) && src[i+1] == '*' {
			for i += 2; i < len(src); i++ {
				if src[i] == '*' && i+1 < len(src) && src[i+1] == '/' {
					i++
					break
				}
			}
			continue
		}
		out = append(out, src[i])
	}
	return string(out)
}
