package clientguard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 模式
const (
	ModeInherit = "inherit" // 仅用于单个 key：沿用全局默认
	ModeOff     = "off"
	ModeLog     = "log-only"
	ModeEnforce = "enforce"
)

func validMode(m string, allowInherit bool) bool {
	switch m {
	case ModeOff, ModeLog, ModeEnforce:
		return true
	case ModeInherit:
		return allowInherit
	}
	return false
}

// Rule 是全局默认或单个 key 的规则。
type Rule struct {
	Label string   `json:"label,omitempty"`
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
}

// RuleSet 是可持久化的规则集合，键为 CPA 的 caller_scope（API Key 的加盐 SHA-256，插件看不到明文 key）。
type RuleSet struct {
	Default Rule            `json:"default"`
	Keys    map[string]Rule `json:"keys"`
}

// Preset 内置客户端特征。
// Confidence: "source" 源码确认；"doc" 官方文档/社区抓包一致；"guess" 推测，需按日志核实。
// 0.1.1 及以前用过 "confirmed"，页面按 "source" 处理。
type Preset struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Category   string   `json:"category"`
	Patterns   []string `json:"patterns"`
	Confidence string   `json:"confidence"`
	Note       string   `json:"note"`
	Warning    string   `json:"warning,omitempty"` // 非空 = 不建议用于放行
	Generic    bool     `json:"generic,omitempty"` // 泛化预设：识别标签时排在具体预设之后
}

var presets = []Preset{
	{ID: "codex", Name: "Codex 全家（CLI / 桌面版 / IDE / SDK）", Category: "Codex", Patterns: []string{CodexFamilyPattern}, Confidence: "source", Generic: true,
		Note: "Codex 官方 originator / User-Agent 以 codex_、codex- 或 \"Codex \" 开头（codex_cli_rs、codex-tui、codex_exec、codex_vscode、codex_sdk_ts、Codex Desktop…），不区分大小写。0.6 起包含空格写法：旧的 re:^codex[_-] 不匹配 Codex Desktop 的真实 UA。"},
	{ID: "codex-cli", Name: "Codex CLI", Category: "Codex", Patterns: []string{"re:^codex[_ -](cli_rs|tui|exec|cli)([/ ;(]|$)"}, Confidence: "source",
		Note: "openai/codex 源码：默认 originator codex_cli_rs；exec / TUI 分别为 codex_exec、codex-tui。"},
	{ID: "codex-desktop", Name: "Codex 桌面版", Category: "Codex", Patterns: []string{"re:^codex[_ -]desktop([/ ;(]|$)"}, Confidence: "source",
		Note: "实测 UA：Codex Desktop/0.162.0-alpha.17.2 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1007.21434)，originator 为 Codex Desktop；旧写法 codex_desktop 也匹配。"},
	{ID: "codex-vscode", Name: "Codex IDE 插件", Category: "Codex", Patterns: []string{"re:^codex[_ -](vscode|vs code|ide)([/ ;(]|$)"}, Confidence: "source",
		Note: "VS Code / Cursor / Windsurf 里的 Codex 插件。"},
	{ID: "claude-code", Name: "Claude Code", Category: "编码 CLI", Patterns: []string{"re:^claude-cli/"}, Confidence: "doc",
		Note: "UA 形如 claude-cli/1.0.80 (external, cli)。闭源，社区抓包一致。"},
	{ID: "gemini-cli", Name: "Gemini CLI", Category: "编码 CLI", Patterns: []string{"re:^(GeminiCLI|CloudCodeVSCode)([/ -]|$)"}, Confidence: "source",
		Note: "UA 形如 GeminiCLI/0.9.0/gemini-2.5-pro (darwin; arm64; terminal)。"},
	{ID: "qwen-code", Name: "Qwen Code", Category: "编码 CLI", Patterns: []string{"re:^QwenCode/"}, Confidence: "source", Note: "UA 形如 QwenCode/0.0.14 (darwin; arm64)。"},
	{ID: "opencode", Name: "OpenCode", Category: "编码 CLI", Patterns: []string{"re:^opencode/"}, Confidence: "source", Note: "UA 形如 opencode/latest/0.15.0/cli。"},
	{ID: "deepseek-harness", Name: "DeepSeek Harness", Category: "编码 CLI", Patterns: []string{"re:^deepseek-harness/"}, Confidence: "source",
		Note: "UA 形如 deepseek-harness/0.2.0-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)。"},
	{ID: "continue-cli", Name: "Continue CLI", Category: "编码 CLI", Patterns: []string{"re:^Continue-CLI/"}, Confidence: "source", Note: "仅 CLI；Continue IDE 插件的 UA 未确认。"},
	{ID: "cline", Name: "Cline", Category: "IDE 插件", Patterns: []string{"re:^Cline/"}, Confidence: "source", Note: "UA 形如 Cline/3.x；同时带 X-Title: Cline。"},
	{ID: "roo-code", Name: "Roo Code", Category: "IDE 插件", Patterns: []string{"re:^roo-code([/ ]|$)"}, Confidence: "source",
		Note: "只有选「OpenAI 原生」provider 时 UA 才是 roo-code/…；用「OpenAI 兼容」provider 时是 OpenAI/JS，识别不了。"},
	{ID: "kilo-code", Name: "Kilo Code", Category: "IDE 插件", Patterns: []string{"re:^kilo(-code)?/"}, Confidence: "source", Note: "UA 形如 kilo/latest/1.0.3/vscode；部分版本为 Kilo-Code/4.x。"},
	{ID: "cherry-studio", Name: "Cherry Studio", Category: "桌面客户端", Patterns: []string{"header:X-Title=re:^Cherry Studio$", "header:HTTP-Referer=cherry-ai.com", "header:Originator=re:^cherry-studio$"}, Confidence: "source",
		Note: "Cherry Studio 默认带 X-Title: Cherry Studio 和 HTTP-Referer: https://cherry-ai.com，Codex 类供应商还带 originator: cherry-studio。UA 不可靠。"},
	{ID: "claude-desktop", Name: "Claude Desktop（需自定义头）", Category: "桌面客户端", Patterns: []string{"header:X-Client=re:^claude-desktop$"}, Confidence: "doc",
		Note: "Claude Desktop 的 UA 未知。请在 Claude Desktop 的第三方推理配置里加 inferenceCustomHeaders，例如 {\"X-Client\": \"claude-desktop\"}，插件读 X-Client 头识别。"},
	{ID: "workbuddy", Name: "WorkBuddy / CodeBuddy", Category: "桌面客户端", Patterns: []string{}, Confidence: "guess",
		Note: "待日志确认：自定义模型时本机直连，但 UA 未知。先把相关 Key 设为「仅记录」，用它发一次请求，再在下方客户端里点「加入允许列表」。"},
	{ID: "debug", Name: "调试工具（curl / HTTPie / Postman / Apifox）", Category: "调试工具", Patterns: []string{"re:^curl/", "re:^HTTPie/", "re:^PostmanRuntime/", "re:^Apifox/"}, Confidence: "doc",
		Note: "命令行和 API 调试工具，测试用。"},
	{ID: "openai-sdk", Name: "OpenAI SDK（通用）", Category: "SDK", Patterns: []string{"re:^OpenAI/(Python|JS)"}, Confidence: "doc", Generic: true,
		Warning: "不建议用于放行：Aider、LobeChat、Dify、Roo Code（兼容模式）等大量工具都用这个 SDK，放行它等于放行一大片。",
		Note:    "openai-python / openai-node 的默认 UA。"},
}

// CodexFamilyPattern 是「Codex 全家」预设与默认允许列表的特征。
const CodexFamilyPattern = "re:^codex[_ -]"

// legacyPatterns：0.1.2 / 0.1–6 预设写进规则的旧特征 → 新特征。旧特征漏掉了
// 带空格的 Codex Desktop。保存的规则不会被自动改写，页面提示并提供一键更新。
var legacyPatterns = map[string]string{
	"re:^codex[_-]":             CodexFamilyPattern,
	"re:^codex_desktop([/ ]|$)": "re:^codex[_ -]desktop([/ ;(]|$)",
	"re:^codex_vscode([/ ]|$)":  "re:^codex[_ -](vscode|vs code|ide)([/ ;(]|$)",
	"re:^(codex_cli_rs|codex-tui|codex_exec|codex-cli)([/ ]|$)": "re:^codex[_ -](cli_rs|tui|exec|cli)([/ ;(]|$)",
	"re:^(GeminiCLI|CloudCodeVSCode)":                           "re:^(GeminiCLI|CloudCodeVSCode)([/ -]|$)",
	"re:^kilo/":                                                 "re:^kilo(-code)?/",
}

// MigrateLegacyPatterns 把与旧预设特征【完全相同】的条目替换为新特征（去重），
// 返回替换次数。自定义写法不会被改动（页面另有提示）。
func MigrateLegacyPatterns(rs RuleSet) (RuleSet, int) {
	n := 0
	fix := func(allow []string) []string {
		if len(allow) == 0 {
			return allow
		}
		out := make([]string, 0, len(allow))
		seen := map[string]bool{}
		for _, p := range allow {
			if r, ok := legacyPatterns[strings.TrimSpace(p)]; ok {
				p = r
				n++
			}
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
		return out
	}
	next := RuleSet{Default: rs.Default, Keys: make(map[string]Rule, len(rs.Keys))}
	next.Default.Allow = fix(rs.Default.Allow)
	for k, r := range rs.Keys {
		r.Allow = fix(r.Allow)
		next.Keys[k] = r
	}
	return next, n
}

// FamilyMismatch：某 Key 的允许列表看起来是给 Codex 准备的（有条目含 "codex"），
// 但一个被识别为 Codex 家族的客户端仍被它拦截——多半是自定义特征写窄了。
type FamilyMismatch struct {
	Scope   string `json:"scope"`
	Client  string `json:"client"`
	Sample  string `json:"sample_ua"`
	Preset  string `json:"preset"`
	Suggest string `json:"suggest"`
	Blocked int64  `json:"blocked"`
}

func codexFamily(h ClientHeaders) *Preset {
	p := presetMatch(h)
	if p == nil || p.Category != "Codex" {
		return nil
	}
	return p
}

func familyMismatches(c compiledRuleSet, rs RuleSet, seen map[string]map[string]*ClientStat) []FamilyMismatch {
	var out []FamilyMismatch
	for scope, m := range seen {
		allow := rs.Default.Allow
		if r, ok := rs.Keys[scope]; ok && r.Mode != "" && r.Mode != ModeInherit && len(r.Allow) > 0 {
			allow = r.Allow
		}
		wantsCodex := false
		for _, a := range allow {
			if strings.Contains(strings.ToLower(a), "codex") {
				wantsCodex = true
			}
		}
		if !wantsCodex {
			continue
		}
		for _, st := range m {
			if st.Sample == nil || st.Blocked == 0 {
				continue
			}
			p := codexFamily(*st.Sample)
			if p == nil {
				continue
			}
			if d := c.decideHeaders(scope, *st.Sample); d.Allowed {
				continue
			}
			out = append(out, FamilyMismatch{Scope: scope, Client: st.Client, Sample: st.SampleUA, Preset: p.Name, Suggest: CodexFamilyPattern, Blocked: st.Blocked})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Client < out[j].Client
	})
	return out
}

// OutdatedPattern 是保存的规则中一个需要更新的旧预设特征。
type OutdatedPattern struct {
	Scope       string `json:"scope"` // "__default" 或 Key scope
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

// OutdatedPatterns 列出规则里仍在使用的旧预设特征（不修改规则）。
func OutdatedPatterns(rs RuleSet) []OutdatedPattern {
	var out []OutdatedPattern
	add := func(scope string, allow []string) {
		for _, p := range allow {
			if r, ok := legacyPatterns[strings.TrimSpace(p)]; ok {
				out = append(out, OutdatedPattern{Scope: scope, Pattern: p, Replacement: r})
			}
		}
	}
	add("__default", rs.Default.Allow)
	scopes := make([]string, 0, len(rs.Keys))
	for k := range rs.Keys {
		scopes = append(scopes, k)
	}
	sort.Strings(scopes)
	for _, k := range scopes {
		add(k, rs.Keys[k].Allow)
	}
	return out
}

// compiledPattern：
//   - 普通文本：不区分大小写的子串，对所有识别的请求头逐个匹配；
//   - re: 开头：不区分大小写的正则（自动加 (?i)），对所有识别的请求头逐个匹配，^ 锚定每个头值的开头；
//   - header:名称=值：只匹配指定请求头，值同样支持普通文本和 re:。
type compiledPattern struct {
	raw    string
	header string // 规范化头名；空 = 任意识别的头
	sub    string
	re     *regexp.Regexp
}

func compileValue(raw, v string) (compiledPattern, error) {
	if strings.HasPrefix(strings.ToLower(v), "re:") {
		expr := strings.TrimSpace(v[3:])
		if expr == "" {
			return compiledPattern{}, fmt.Errorf("空正则")
		}
		re, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return compiledPattern{}, fmt.Errorf("正则 %q 无效: %v", expr, err)
		}
		return compiledPattern{raw: raw, re: re}, nil
	}
	return compiledPattern{raw: raw, sub: strings.ToLower(v)}, nil
}

func compilePattern(p string) (compiledPattern, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return compiledPattern{}, fmt.Errorf("空规则")
	}
	if strings.HasPrefix(strings.ToLower(p), "header:") {
		rest := p[len("header:"):]
		i := strings.Index(rest, "=")
		if i <= 0 {
			return compiledPattern{}, fmt.Errorf("%q 格式应为 header:请求头=值", p)
		}
		name := strings.TrimSpace(rest[:i])
		val := strings.TrimSpace(rest[i+1:])
		if !headerNameRe.MatchString(name) {
			return compiledPattern{}, fmt.Errorf("请求头名 %q 无效", name)
		}
		cn := canonHeader(name)
		if sensitiveHeaders[cn] {
			return compiledPattern{}, fmt.Errorf("不允许读取敏感请求头 %s", cn)
		}
		if val == "" {
			return compiledPattern{}, fmt.Errorf("%q 缺少要匹配的值", p)
		}
		cp, err := compileValue(p, val)
		if err != nil {
			return cp, err
		}
		cp.header = cn
		return cp, nil
	}
	return compileValue(p, p)
}

func (c compiledPattern) matchValue(s string) bool {
	if s == "" {
		return false
	}
	if c.re != nil {
		return c.re.MatchString(s)
	}
	return strings.Contains(strings.ToLower(s), c.sub)
}

// match 兼容旧调用：只对单个字符串匹配（忽略 header 限定）。
func (c compiledPattern) match(s string) bool { return c.matchValue(s) }

func (c compiledPattern) matchHeaders(h ClientHeaders) bool {
	if c.header != "" {
		return c.matchValue(h.get(c.header))
	}
	for _, v := range h.values() {
		if c.matchValue(v) {
			return true
		}
	}
	return false
}

type compiledRule struct {
	Mode  string
	Allow []compiledPattern
}

type compiledRuleSet struct {
	def  compiledRule
	keys map[string]compiledRule
}

func normalizeRule(r Rule, isDefault bool) (Rule, error) {
	r.Label = strings.TrimSpace(r.Label)
	if len(r.Label) > 64 {
		r.Label = r.Label[:64]
	}
	r.Mode = strings.TrimSpace(strings.ToLower(r.Mode))
	if r.Mode == "" {
		if isDefault {
			r.Mode = ModeLog
		} else {
			r.Mode = ModeInherit
		}
	}
	if !validMode(r.Mode, !isDefault) {
		return r, fmt.Errorf("未知模式 %q", r.Mode)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range r.Allow {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if _, err := compilePattern(p); err != nil {
			return r, err
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > 50 {
		return r, fmt.Errorf("单条规则最多 50 个客户端特征")
	}
	r.Allow = out
	return r, nil
}

// NormalizeRuleSet 校验并清洗规则集。
func NormalizeRuleSet(rs RuleSet) (RuleSet, error) {
	def, err := normalizeRule(rs.Default, true)
	if err != nil {
		return rs, fmt.Errorf("默认规则: %w", err)
	}
	out := RuleSet{Default: def, Keys: map[string]Rule{}}
	if len(rs.Keys) > 500 {
		return rs, fmt.Errorf("key 规则过多")
	}
	for scope, r := range rs.Keys {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if !validScope(scope) {
			return rs, fmt.Errorf("无效的 key 标识 %q", scope)
		}
		nr, err := normalizeRule(r, false)
		if err != nil {
			return rs, fmt.Errorf("key %s: %w", shortScope(scope), err)
		}
		out.Keys[scope] = nr
	}
	return out, nil
}

func compileRuleSet(rs RuleSet) (compiledRuleSet, error) {
	rs, err := NormalizeRuleSet(rs)
	if err != nil {
		return compiledRuleSet{}, err
	}
	cr := compiledRuleSet{keys: map[string]compiledRule{}}
	cr.def = compileRule(rs.Default)
	for s, r := range rs.Keys {
		cr.keys[s] = compileRule(r)
	}
	return cr, nil
}

func compileRule(r Rule) compiledRule {
	c := compiledRule{Mode: r.Mode}
	for _, p := range r.Allow {
		if cp, err := compilePattern(p); err == nil {
			c.Allow = append(c.Allow, cp)
		}
	}
	return c
}

// Decision 是一次判定结果。
type Decision struct {
	Mode       string // 生效模式
	Matched    string // 命中的特征（空=未命中）
	Allowed    bool   // 客户端是否在允许列表内
	Block      bool   // 是否需要拦截
	EmptyAllow bool   // enforce 但允许列表为空（视为不限制）
}

// effective 计算某个 key 的生效规则：key 规则 mode=inherit 时用全局模式；key 允许列表为空时沿用全局列表。
func (c compiledRuleSet) effective(scope string) compiledRule {
	r, ok := c.keys[scope]
	if !ok || r.Mode == ModeInherit || r.Mode == "" {
		return c.def
	}
	if len(r.Allow) == 0 {
		return compiledRule{Mode: r.Mode, Allow: c.def.Allow}
	}
	return r
}

func (c compiledRuleSet) decide(scope, ua, originator string) Decision {
	return c.decideHeaders(scope, ClientHeaders{UA: ua, Originator: originator})
}

// referencedHeaders 返回规则里 header:名称= 引用到的非内置头，请求时需要额外读取。
func (c compiledRuleSet) referencedHeaders() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(r compiledRule) {
		for _, p := range r.Allow {
			if p.header != "" && !isBuiltinHeader(p.header) && !seen[p.header] {
				seen[p.header] = true
				out = append(out, p.header)
			}
		}
	}
	add(c.def)
	for _, r := range c.keys {
		add(r)
	}
	return out
}

func (c compiledRuleSet) decideHeaders(scope string, h ClientHeaders) Decision {
	r := c.effective(scope)
	d := Decision{Mode: r.Mode}
	for _, p := range r.Allow {
		if p.matchHeaders(h) {
			d.Matched = p.raw
			d.Allowed = true
			break
		}
	}
	if r.Mode == ModeEnforce {
		if len(r.Allow) == 0 {
			d.EmptyAllow = true
		} else if !d.Allowed {
			d.Block = true
		}
	}
	return d
}

func validScope(s string) bool {
	if s == anonymousScope {
		return true
	}
	if len(s) != 64 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

const anonymousScope = "anonymous"

func shortScope(s string) string {
	if len(s) > 12 {
		return s[:8] + "…" + s[len(s)-4:]
	}
	return s
}

// presetMatch 返回最匹配的内置预设：具体预设优先于泛化预设（Codex 全家、OpenAI SDK）。
func presetMatch(h ClientHeaders) *Preset {
	var generic *Preset
	for i := range presets {
		p := &presets[i]
		for _, raw := range p.Patterns {
			cp, err := compilePattern(raw)
			if err != nil || !cp.matchHeaders(h) {
				continue
			}
			if !p.Generic {
				return p
			}
			if generic == nil {
				generic = p
			}
			break
		}
	}
	return generic
}

// clientLabel 兼容旧调用。
func clientLabel(ua, originator string) string {
	return clientLabelHeaders(ClientHeaders{UA: ua, Originator: originator})
}

// clientLabelHeaders 把请求头归纳成友好的客户端名称（用于聚合统计）：先看预设，再取 UA 产品标识。
func clientLabelHeaders(h ClientHeaders) string {
	if p := presetMatch(h); p != nil {
		name := p.Name
		if p.ID == "debug" {
			if t := productToken(h.UA); t != "" {
				name = t
			}
		}
		if p.ID == "codex" {
			if t := productToken(firstNonEmpty(h.Originator, h.UA)); t != "" {
				name = "Codex（" + t + "）"
			}
		}
		return name
	}
	return legacyLabel(h)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// productToken 取 UA 里第一个产品名（"/"、空格、"(" 之前）。
func productToken(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.IndexAny(u, " ("); i > 0 {
		u = u[:i]
	}
	if j := strings.Index(u, "/"); j > 0 {
		u = u[:j]
	}
	return u
}

// suggestPattern 为一个看到的客户端生成精确的允许特征（页面「一键加入允许列表」用）。
func suggestPattern(h ClientHeaders) string {
	if p := presetMatch(h); p != nil && !p.Generic && p.Warning == "" && len(p.Patterns) > 0 {
		return p.Patterns[0]
	}
	if o := strings.TrimSpace(h.Originator); o != "" && len(o) <= 64 {
		return "header:Originator=re:^" + regexp.QuoteMeta(o) + "$"
	}
	keys := make([]string, 0, len(h.Custom))
	for k := range h.Custom {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := h.Custom[k]; v != "" && len(v) <= 64 {
			return "header:" + k + "=re:^" + regexp.QuoteMeta(v) + "$"
		}
	}
	if v := strings.TrimSpace(h.XClientName); v != "" && len(v) <= 64 {
		return "header:X-Client-Name=re:^" + regexp.QuoteMeta(v) + "$"
	}
	if v := strings.TrimSpace(h.XTitle); v != "" && len(v) <= 64 {
		return "header:X-Title=re:^" + regexp.QuoteMeta(v) + "$"
	}
	if t := productToken(h.UA); t != "" && !strings.EqualFold(t, "Mozilla") {
		return "header:User-Agent=re:^" + regexp.QuoteMeta(t) + "/"
	}
	return ""
}

func legacyLabel(h ClientHeaders) string {
	o := strings.TrimSpace(h.Originator)
	u := strings.TrimSpace(h.UA)
	head := productToken(u)
	if strings.EqualFold(head, "Mozilla") {
		// 浏览器 / Electron UA：取第一个不是浏览器内核的产品标识（例如 CherryStudio/1.5.3），否则取最后一个
		f := strings.Fields(u)
		head = "浏览器 / Electron"
		for _, tok := range f[1:] {
			name := productToken(tok)
			if strings.Contains(tok, "/") && !strings.Contains(tok, "://") && name != "" && !browserTokens[strings.ToLower(name)] {
				head = name
				break
			}
		}
	}
	var s string
	switch {
	case o != "" && head != "" && !strings.EqualFold(o, head):
		s = o + " | " + head
	case o != "":
		s = o
	case head != "":
		s = head
	case h.XTitle != "":
		s = h.XTitle
	case h.XClientName != "":
		s = h.XClientName
	default:
		s = "(无 User-Agent)"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

var browserTokens = map[string]bool{"mozilla": true, "applewebkit": true, "chrome": true, "safari": true, "electron": true, "gecko": true, "firefox": true, "version": true, "mobile": true, "edg": true, "khtml,": true}
