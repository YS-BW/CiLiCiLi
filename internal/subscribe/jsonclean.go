package subscribe

import "strings"

// 订阅内容清洗：部分共享配置带行注释、块注释、尾随逗号，字符串里还可能有裸换行
// 或制表符（TVBOX 客户端用的 fastjson 默认容忍，标准 JSON 会直接失败）。
// 这里在字符串外部剥离注释与尾随逗号、把字符串内的裸控制字符转义成合法序列，
// 字符串里的 `//`（如 https://）与 `\"` 不受影响。

// stripJSONArtifacts 先剥注释再剥尾随逗号。
// 顺序不能反：`[1, // 注释\n]` 里的逗号隔着注释，只有先删掉注释才能识别为尾随逗号。
func stripJSONArtifacts(text string) string {
	return stripTrailingCommas(stripComments(text))
}

// escapeControlChar 把字符串内的裸控制字符转成合法转义序列。
func escapeControlChar(code byte) string {
	switch code {
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	}
	return `\u00` + string(digits[code>>4]) + string(digits[code&0x0f])
}

// digits 是十六进制字符表，供 0x00-0x1f 控制字符转义使用。
const digits = "0123456789abcdef"

// stripComments 剥离字符串外部的行注释与块注释，并转义字符串内的裸控制字符。
func stripComments(text string) string {
	var out strings.Builder
	out.Grow(len(text))

	inString := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(text); i++ {
		ch := text[i]
		var next byte
		if i+1 < len(text) {
			next = text[i+1]
		}

		switch {
		case inLineComment:
			if ch == '\n' || ch == '\r' {
				inLineComment = false
				out.WriteByte(ch)
			}
		case inBlockComment:
			if ch == '*' && next == '/' {
				inBlockComment = false
				i++
			}
		case inString:
			// 裸控制字符（换行/制表符等）在 JSON 字符串里非法，转义后保留原意。
			if ch < 0x20 {
				out.WriteString(escapeControlChar(ch))
				continue
			}
			out.WriteByte(ch)
			if ch == '\\' {
				// 转义字符连同其后一位原样保留，避免 \" 被误判成字符串结束。
				if i+1 < len(text) {
					out.WriteByte(next)
					i++
				}
			} else if ch == '"' {
				inString = false
			}
		case ch == '"':
			inString = true
			out.WriteByte(ch)
		case ch == '/' && next == '/':
			inLineComment = true
			i++
		case ch == '/' && next == '*':
			inBlockComment = true
			i++
		default:
			out.WriteByte(ch)
		}
	}

	return out.String()
}

// stripTrailingCommas 剥离尾随逗号（`{"a":1,}` / `[1,2,]`）；调用前文本已不含注释。
func stripTrailingCommas(text string) string {
	var out strings.Builder
	out.Grow(len(text))

	inString := false
	for i := 0; i < len(text); i++ {
		ch := text[i]

		if inString {
			out.WriteByte(ch)
			if ch == '\\' {
				if i+1 < len(text) {
					out.WriteByte(text[i+1])
					i++
				}
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out.WriteByte(ch)
			continue
		}
		if ch == ',' {
			j := i + 1
			for j < len(text) && isJSONSpace(text[j]) {
				j++
			}
			if j < len(text) && (text[j] == '}' || text[j] == ']') {
				continue
			}
		}
		out.WriteByte(ch)
	}

	return out.String()
}

// isJSONSpace 判断是否为 JSON 里的空白字符。
func isJSONSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}
