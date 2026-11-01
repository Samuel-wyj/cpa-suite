package clientguard

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// 内置识别的请求头（规范化名称）。规则里不带 header: 前缀的特征会对这些头和 extra_headers 逐个匹配。
var builtinHeaders = []string{"User-Agent", "Originator", "X-Title", "Http-Referer", "Referer", "X-Client-Name"}

// 永远不读取、不记录的敏感头。
var sensitiveHeaders = map[string]bool{
	"Authorization": true, "Proxy-Authorization": true, "X-Api-Key": true, "Cookie": true, "Set-Cookie": true,
	"X-Goog-Api-Key": true, "Api-Key": true, "X-Management-Key": true,
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]{1,64}$`)

func canonHeader(name string) string { return http.CanonicalHeaderKey(strings.TrimSpace(name)) }

// ClientHeaders 是一次请求里用于识别客户端的请求头（不含任何敏感头）。
type ClientHeaders struct {
	UA          string            `json:"ua"`
	Originator  string            `json:"originator"`
	XTitle      string            `json:"x_title,omitempty"`
	HTTPReferer string            `json:"http_referer,omitempty"`
	Referer     string            `json:"referer,omitempty"`
	XClientName string            `json:"x_client_name,omitempty"`
	Custom      map[string]string `json:"custom,omitempty"`
}

// get 按（不区分大小写的）头名取值。
func (c ClientHeaders) get(name string) string {
	switch canonHeader(name) {
	case "User-Agent":
		return c.UA
	case "Originator":
		return c.Originator
	case "X-Title":
		return c.XTitle
	case "Http-Referer":
		return c.HTTPReferer
	case "Referer":
		return c.Referer
	case "X-Client-Name":
		return c.XClientName
	}
	return c.Custom[canonHeader(name)]
}

// values 返回所有非空头值（固定顺序，便于“命中”展示稳定）。
func (c ClientHeaders) values() []string {
	out := []string{}
	for _, v := range []string{c.UA, c.Originator, c.XTitle, c.HTTPReferer, c.Referer, c.XClientName} {
		if v != "" {
			out = append(out, v)
		}
	}
	keys := make([]string, 0, len(c.Custom))
	for k := range c.Custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c.Custom[k] != "" {
			out = append(out, c.Custom[k])
		}
	}
	return out
}

func clip(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 300 {
		v = v[:300]
	}
	return v
}

// extractHeaders 读取内置头 + extra 列表里的自定义头。extra 中的敏感头会被忽略。
func extractHeaders(h http.Header, extra []string) ClientHeaders {
	c := ClientHeaders{}
	if h == nil {
		return c
	}
	g := func(n string) string { return clip(h.Get(n)) }
	c.UA, c.Originator, c.XTitle = g("User-Agent"), g("Originator"), g("X-Title")
	c.HTTPReferer, c.Referer, c.XClientName = g("HTTP-Referer"), g("Referer"), g("X-Client-Name")
	for _, n := range extra {
		cn := canonHeader(n)
		if cn == "" || sensitiveHeaders[cn] || isBuiltinHeader(cn) {
			continue
		}
		if v := g(cn); v != "" {
			if c.Custom == nil {
				c.Custom = map[string]string{}
			}
			if len(c.Custom) < 10 {
				c.Custom[cn] = v
			}
		}
	}
	return c
}

func isBuiltinHeader(cn string) bool {
	for _, b := range builtinHeaders {
		if b == cn {
			return true
		}
	}
	return false
}
