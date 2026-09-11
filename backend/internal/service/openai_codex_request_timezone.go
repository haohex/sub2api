package service

// klno 请求时区替换（账号级开关 accounts.extra["codex_request_timezone"]）。
//
// 背景：Codex 客户端把本机时区与本地日期写进请求体里的 <environment_context> 块，
// 随 input 一起发给上游（codex-rs core/src/session/turn_context.rs 的
// local_time_context() → iana_time_zone::get_timezone()，渲染见
// core/src/context/world_state/environment.rs）：
//
//	<environment_context>
//	  <current_date>2026-06-20</current_date>
//	  <timezone>America/Los_Angeles</timezone>
//	  ...
//	</environment_context>
//
// 于是"美区账号 + 亚洲时区客户端"这种组合会把两套地理信号同时交给上游。本开关把
// 这两个元素改写成账号出口（代理出口 IP）所在时区，让出站身份与账号地区自洽。
//
// 取值：
//
//	缺省 / "" / "off"   —— 关闭，出站与上游行为完全一致（默认）。
//	"auto" / "proxy"     —— 跟随账号代理出口 IP 反查到的时区。
//	"Asia/Shanghai" 等   —— 固定时区（IANA 名，需能 LoadLocation）。
//
// 只改写已存在的元素：客户端没发过 <timezone> 时保持原样，不凭空造块。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	// 嵌入一份 IANA 时区库：本特性要按名字解析时区，而 scratch/alpine 等镜像未必装了
	// 系统 tzdata，缺了它 LoadLocation 会失败、整个改写静默失效（内部验收时踩到过）。
	_ "time/tzdata"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexRequestTimezoneExtraKey = "codex_request_timezone"

// codexEnvironmentContextMarker 是环境块的开标签；也是热路径的快速否定条件。
const codexEnvironmentContextMarker = "<environment_context>"

// codexEnvironmentContextEscapedMarker 是 Go 系 JSON 编码器（encoding/json）对 `<` 的
// 转义形态：这类客户端发来的体里没有字面量 `<environment_context>`，快速否定必须同时认。
const codexEnvironmentContextEscapedMarker = `\u003cenvironment_context\u003e`

// codexBodyMentionsEnvironmentContext 是热路径的廉价前置判断：两次子串扫描，
// 命中才去解析 input（也才可能触发代理时区探测）。
func codexBodyMentionsEnvironmentContext(body []byte) bool {
	return bytes.Contains(body, []byte(codexEnvironmentContextMarker)) ||
		bytes.Contains(body, []byte(codexEnvironmentContextEscapedMarker))
}

type codexRequestTimezoneMode int

const (
	codexRequestTimezoneOff codexRequestTimezoneMode = iota
	// codexRequestTimezoneAuto 跟随账号代理出口 IP 反查到的时区。
	codexRequestTimezoneAuto
	// codexRequestTimezoneFixed 使用管理员显式配置的 IANA 时区。
	codexRequestTimezoneFixed
)

// codexTimezoneNamePattern 只接受 Area/Location 形态与 UTC，拒绝任意字节：
// 该值会被写进出站请求体，且要交给 time.LoadLocation。
var codexTimezoneNamePattern = regexp.MustCompile(`^(?:UTC|[A-Za-z][A-Za-z0-9_+-]*(?:/[A-Za-z0-9_+-]+)+)$`)

// normalizeCodexTimezoneName 校验 IANA 时区名，非法返回空串。
func normalizeCodexTimezoneName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" || len(name) > 64 || !codexTimezoneNamePattern.MatchString(name) {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

type codexRequestTimezoneSetting struct {
	mode codexRequestTimezoneMode
	name string
}

// codexRequestTimezoneFromAccount 读取账号级开关。仅 OAuth 类 OpenAI 账号生效。
func codexRequestTimezoneFromAccount(account *Account) codexRequestTimezoneSetting {
	if account == nil || !account.IsOpenAIOAuthLike() || account.Extra == nil {
		return codexRequestTimezoneSetting{}
	}
	switch value := account.Extra[codexRequestTimezoneExtraKey].(type) {
	case bool:
		if value {
			return codexRequestTimezoneSetting{mode: codexRequestTimezoneAuto}
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "off", "false", "0", "none":
			return codexRequestTimezoneSetting{}
		case "auto", "proxy", "follow", "follow-proxy", "true", "1":
			return codexRequestTimezoneSetting{mode: codexRequestTimezoneAuto}
		}
		if name := normalizeCodexTimezoneName(value); name != "" {
			return codexRequestTimezoneSetting{mode: codexRequestTimezoneFixed, name: name}
		}
	}
	return codexRequestTimezoneSetting{}
}

const (
	// codexTimezoneCacheTTL 出口 IP 的归属地变化很慢，缓存久一点避免频繁探测。
	codexTimezoneCacheTTL = 6 * time.Hour
	// codexTimezoneFailureCacheTTL 探测失败（代理拦截 ip-api、超时等）短缓存，
	// 既不打爆代理，也能在故障恢复后自愈。
	codexTimezoneFailureCacheTTL = 10 * time.Minute
	// codexTimezoneProbeTimeout 后台探测超时。探测不在请求路径上等结果。
	codexTimezoneProbeTimeout = 15 * time.Second
)

type codexTimezoneCacheEntry struct {
	name      string
	expiresAt time.Time
}

// CodexRequestTimezoneResolver 把"账号代理出口 IP → IANA 时区"解析结果缓存起来。
//
// 请求路径上只读缓存：缓存缺失或过期时立即返回旧值（可能为空串即不改写），
// 同时异步触发一次经代理的探测。这样代理探测再慢也不会拖慢用户请求。
type CodexRequestTimezoneResolver struct {
	prober ProxyExitInfoProber

	mu       sync.Mutex
	entries  map[string]codexTimezoneCacheEntry
	inflight map[string]struct{}

	now          func() time.Time
	ttl          time.Duration
	failureTTL   time.Duration
	probeTimeout time.Duration
}

// NewCodexRequestTimezoneResolver 构造解析器；prober 为 nil 时恒返回空串（不改写）。
func NewCodexRequestTimezoneResolver(prober ProxyExitInfoProber) *CodexRequestTimezoneResolver {
	return &CodexRequestTimezoneResolver{
		prober:       prober,
		entries:      make(map[string]codexTimezoneCacheEntry),
		inflight:     make(map[string]struct{}),
		now:          time.Now,
		ttl:          codexTimezoneCacheTTL,
		failureTTL:   codexTimezoneFailureCacheTTL,
		probeTimeout: codexTimezoneProbeTimeout,
	}
}

// TimezoneForAccount 返回该账号本次请求应使用的目标时区，空串表示不改写。
func (r *CodexRequestTimezoneResolver) TimezoneForAccount(account *Account) string {
	setting := codexRequestTimezoneFromAccount(account)
	switch setting.mode {
	case codexRequestTimezoneOff:
		return ""
	case codexRequestTimezoneFixed:
		return setting.name
	case codexRequestTimezoneAuto:
	default:
		return ""
	}
	if r == nil || r.prober == nil || account == nil {
		return ""
	}
	proxy := account.Proxy
	if proxy == nil {
		return ""
	}
	proxyURL := strings.TrimSpace(proxy.URL())
	if proxyURL == "" {
		return ""
	}

	key := codexTimezoneProxyCacheKey(proxy, proxyURL)
	now := r.now()
	r.mu.Lock()
	entry, cached := r.entries[key]
	r.mu.Unlock()
	if cached && now.Before(entry.expiresAt) {
		return entry.name
	}
	r.refreshAsync(key, proxyURL)
	return entry.name
}

// codexTimezoneProxyCacheKey 以代理 ID + URL 摘要为键：URL 含凭据，不落在键里。
func codexTimezoneProxyCacheKey(proxy *Proxy, proxyURL string) string {
	sum := sha256.Sum256([]byte(proxyURL))
	return fmt.Sprintf("%d:%s", proxy.ID, hex.EncodeToString(sum[:8]))
}

func (r *CodexRequestTimezoneResolver) refreshAsync(key, proxyURL string) {
	r.mu.Lock()
	if _, busy := r.inflight[key]; busy {
		r.mu.Unlock()
		return
	}
	r.inflight[key] = struct{}{}
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.inflight, key)
			r.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), r.probeTimeout)
		defer cancel()
		name, err := r.probeProxyTimezone(ctx, proxyURL)
		ttl := r.ttl
		if err != nil || name == "" {
			ttl = r.failureTTL
		}
		r.mu.Lock()
		r.entries[key] = codexTimezoneCacheEntry{name: name, expiresAt: r.now().Add(ttl)}
		r.mu.Unlock()
	}()
}

func (r *CodexRequestTimezoneResolver) probeProxyTimezone(ctx context.Context, proxyURL string) (string, error) {
	info, _, err := r.prober.ProbeProxy(ctx, proxyURL)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", fmt.Errorf("proxy probe returned no exit info")
	}
	return normalizeCodexTimezoneName(info.Timezone), nil
}

var (
	codexRequestTimezoneMu       sync.RWMutex
	codexRequestTimezoneResolver *CodexRequestTimezoneResolver
)

// SetCodexRequestTimezoneResolver 发布进程级解析器快照（由装配层注入）。
// 出站改写发生在无法注入配置的纯函数热路径上，故与规范 UA 一样走进程级快照。
func SetCodexRequestTimezoneResolver(resolver *CodexRequestTimezoneResolver) {
	codexRequestTimezoneMu.Lock()
	defer codexRequestTimezoneMu.Unlock()
	codexRequestTimezoneResolver = resolver
}

// ProvideCodexRequestTimezoneResolver 装配并发布请求时区解析器。
func ProvideCodexRequestTimezoneResolver(prober ProxyExitInfoProber) *CodexRequestTimezoneResolver {
	resolver := NewCodexRequestTimezoneResolver(prober)
	SetCodexRequestTimezoneResolver(resolver)
	return resolver
}

func resolveCodexRequestTimezone(account *Account) string {
	codexRequestTimezoneMu.RLock()
	resolver := codexRequestTimezoneResolver
	codexRequestTimezoneMu.RUnlock()
	if resolver == nil {
		// 未装配解析器时（裁剪构建 / 单测）固定时区仍然可用：它不需要访问代理。
		// auto 依赖出口 IP 探测，没有解析器就无法解析，按"不改写"处理。
		if setting := codexRequestTimezoneFromAccount(account); setting.mode == codexRequestTimezoneFixed {
			return setting.name
		}
		return ""
	}
	return resolver.TimezoneForAccount(account)
}

var (
	codexEnvironmentTimezoneElement = regexp.MustCompile(`<timezone>[^<]*</timezone>`)
	codexEnvironmentDateElement     = regexp.MustCompile(`<current_date>[^<]*</current_date>`)
)

// rewriteCodexEnvironmentContextTimezone 改写环境块内的 <timezone> 与 <current_date>。
//
// 日期跟着时区一起改：真客户端的日期本来就由本机时区推出（world_state.rs 的
// with_timezone(&chrono::Local)），只改时区会留下"时区与日期对不上"的新矛盾。
// 只在客户端确实发过 <timezone> 的块里改写，不改写就不新增。
func rewriteCodexEnvironmentContextTimezone(text, timezone string, now time.Time) (string, bool) {
	if text == "" || timezone == "" {
		return text, false
	}
	if !strings.Contains(text, codexEnvironmentContextMarker) || !strings.Contains(text, "<timezone>") {
		return text, false
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return text, false
	}
	next := codexEnvironmentTimezoneElement.ReplaceAllLiteralString(text, "<timezone>"+timezone+"</timezone>")
	next = codexEnvironmentDateElement.ReplaceAllLiteralString(next, "<current_date>"+now.In(location).Format("2006-01-02")+"</current_date>")
	return next, next != text
}

// applyCodexRequestTimezoneMap 是非透传路径的入口（请求体已解码为 map）。
func applyCodexRequestTimezoneMap(account *Account, body map[string]any) bool {
	if !codexMapHasEnvironmentContext(body) {
		return false
	}
	timezone := resolveCodexRequestTimezone(account)
	if timezone == "" {
		return false
	}
	return rewriteCodexRequestTimezoneMap(body, timezone, time.Now())
}

// applyCodexRequestTimezoneRaw 是透传 / WS 热路径的入口（原始字节）。
func applyCodexRequestTimezoneRaw(account *Account, body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !codexBodyMentionsEnvironmentContext(body) {
		return body, false, nil
	}
	timezone := resolveCodexRequestTimezone(account)
	if timezone == "" {
		return body, false, nil
	}
	return rewriteCodexRequestTimezoneRaw(body, timezone, time.Now())
}

// applyCodexSearchLocationTimezone 把 web_search 的 user_location.timezone 同步成本次
// 请求的目标时区（与环境块改写同一个值），避免同一请求里两处地理信号互相矛盾。
//
// 只改客户端已经给出的时区：city / region / country 是客户端显式声明的真实位置，
// 就地补一个时区只会造出"上海 + 洛杉矶时区"这种新的自相矛盾。这三项一律不动。
func applyCodexSearchLocationTimezone(location map[string]any, timezone string) bool {
	if location == nil || timezone == "" {
		return false
	}
	current, ok := location["timezone"].(string)
	if !ok || strings.TrimSpace(current) == "" || strings.TrimSpace(current) == timezone {
		return false
	}
	location["timezone"] = timezone
	return true
}

// applyCodexRequestTimezoneToAlphaSearchBody 改写 standalone alpha/search 请求体里的
// settings.user_location.timezone（桥接路径见 buildOpenAIAlphaSearchResponsesWebSearchBody）。
// 与 user_location 的其它字段一致：只改客户端已经给出的时区。
func applyCodexRequestTimezoneToAlphaSearchBody(account *Account, body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"user_location"`)) {
		return body, false, nil
	}
	current := gjson.GetBytes(body, "settings.user_location.timezone")
	if current.Type != gjson.String || strings.TrimSpace(current.String()) == "" {
		return body, false, nil
	}
	timezone := resolveCodexRequestTimezone(account)
	if timezone == "" || strings.TrimSpace(current.String()) == timezone {
		return body, false, nil
	}
	next, err := sjson.SetBytes(body, "settings.user_location.timezone", timezone)
	if err != nil {
		return body, false, fmt.Errorf("splice alpha search timezone: %w", err)
	}
	return next, true, nil
}

func codexMapHasEnvironmentContext(body map[string]any) bool {
	if body == nil {
		return false
	}
	switch input := body["input"].(type) {
	case string:
		return strings.Contains(input, codexEnvironmentContextMarker)
	case []any:
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch content := item["content"].(type) {
			case string:
				if strings.Contains(content, codexEnvironmentContextMarker) {
					return true
				}
			case []any:
				for _, rawPart := range content {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					if text, ok := part["text"].(string); ok && strings.Contains(text, codexEnvironmentContextMarker) {
						return true
					}
				}
			}
		}
	}
	return false
}

func rewriteCodexRequestTimezoneMap(body map[string]any, timezone string, now time.Time) bool {
	if body == nil {
		return false
	}
	changed := false
	switch input := body["input"].(type) {
	case string:
		if rewritten, ok := rewriteCodexEnvironmentContextTimezone(input, timezone, now); ok {
			body["input"] = rewritten
			changed = true
		}
	case []any:
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch content := item["content"].(type) {
			case string:
				if rewritten, ok := rewriteCodexEnvironmentContextTimezone(content, timezone, now); ok {
					item["content"] = rewritten
					changed = true
				}
			case []any:
				for _, rawPart := range content {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					text, ok := part["text"].(string)
					if !ok {
						continue
					}
					if rewritten, ok := rewriteCodexEnvironmentContextTimezone(text, timezone, now); ok {
						part["text"] = rewritten
						changed = true
					}
				}
			}
		}
	}
	return changed
}

// rewriteCodexRequestTimezoneRaw 在原始字节上做同样的改写：只解出 input 里文本项，
// 其余字节原样保留（透传是热路径，禁止对可能数十 MB 的 body 做全量 Unmarshal）。
func rewriteCodexRequestTimezoneRaw(body []byte, timezone string, now time.Time) ([]byte, bool, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}
	input := root.Get("input")
	next := body
	changed := false

	setText := func(path string, text string) error {
		rewritten, ok := rewriteCodexEnvironmentContextTimezone(text, timezone, now)
		if !ok {
			return nil
		}
		updated, err := sjson.SetBytes(next, path, rewritten)
		if err != nil {
			return fmt.Errorf("splice environment timezone: %w", err)
		}
		next, changed = updated, true
		return nil
	}

	switch {
	case input.Type == gjson.String:
		if err := setText("input", input.String()); err != nil {
			return body, false, err
		}
	case input.IsArray():
		for index, item := range input.Array() {
			content := item.Get("content")
			switch {
			case content.Type == gjson.String:
				if err := setText(fmt.Sprintf("input.%d.content", index), content.String()); err != nil {
					return body, false, err
				}
			case content.IsArray():
				for partIndex, part := range content.Array() {
					text := part.Get("text")
					if text.Type != gjson.String {
						continue
					}
					if err := setText(fmt.Sprintf("input.%d.content.%d.text", index, partIndex), text.String()); err != nil {
						return body, false, err
					}
				}
			}
		}
	}
	return next, changed, nil
}
