package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	// 测试环境未必带系统 tzdata，嵌入一份保证 LoadLocation 可用。
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubProxyExitInfoProber struct {
	calls    atomic.Int64
	timezone string
	err      error
	// release 非 nil 时，探测阻塞到该 channel 关闭，便于确定性地测试"请求路径不等待探测"。
	release chan struct{}
}

func (s *stubProxyExitInfoProber) ProbeProxy(context.Context, string) (*ProxyExitInfo, int64, error) {
	s.calls.Add(1)
	if s.release != nil {
		<-s.release
	}
	if s.err != nil {
		return nil, 0, s.err
	}
	return &ProxyExitInfo{IP: "203.0.113.7", Timezone: s.timezone}, 12, nil
}

func newTimezoneTestAccount(extra map[string]any, proxy *Proxy) *Account {
	account := newTestOAuthAccount(7, extra)
	if proxy != nil {
		account.Proxy = proxy
		account.ProxyID = &proxy.ID
	}
	return account
}

func timezoneTestBody(timezone, date string) map[string]any {
	text := "<environment_context>\n  <current_date>" + date + "</current_date>\n  <timezone>" + timezone + "</timezone>\n</environment_context>"
	return map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": text},
				},
			},
		},
	}
}

// --- 开关解析 ---

func TestCodexRequestTimezoneFromAccount(t *testing.T) {
	proxy := &Proxy{ID: 3, Protocol: "http", Host: "127.0.0.1", Port: 8080}
	tests := []struct {
		name string
		acc  *Account
		want codexRequestTimezoneSetting
	}{
		{"nil 账号", nil, codexRequestTimezoneSetting{}},
		{"非 OAuth 账号", &Account{Platform: PlatformOpenAI, Type: "api_key", Extra: map[string]any{codexRequestTimezoneExtraKey: "auto"}}, codexRequestTimezoneSetting{}},
		{"无 extra", newTimezoneTestAccount(nil, proxy), codexRequestTimezoneSetting{}},
		{"空值关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: ""}, proxy), codexRequestTimezoneSetting{}},
		{"off 关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "off"}, proxy), codexRequestTimezoneSetting{}},
		{"非法值关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "Not/AZone"}, proxy), codexRequestTimezoneSetting{}},
		{"路径注入关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "../../etc/passwd"}, proxy), codexRequestTimezoneSetting{}},
		{"数字关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: 1}, proxy), codexRequestTimezoneSetting{}},
		{"bool true 即 auto", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: true}, proxy), codexRequestTimezoneSetting{mode: codexRequestTimezoneAuto}},
		{"bool false 关闭", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: false}, proxy), codexRequestTimezoneSetting{}},
		{"auto", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, proxy), codexRequestTimezoneSetting{mode: codexRequestTimezoneAuto}},
		{"proxy 别名", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "PROXY"}, proxy), codexRequestTimezoneSetting{mode: codexRequestTimezoneAuto}},
		{"显式 IANA", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "America/Los_Angeles"}, proxy), codexRequestTimezoneSetting{mode: codexRequestTimezoneFixed, name: "America/Los_Angeles"}},
		{"UTC", newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "UTC"}, proxy), codexRequestTimezoneSetting{mode: codexRequestTimezoneFixed, name: "UTC"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, codexRequestTimezoneFromAccount(tt.acc))
		})
	}
}

// --- 文本改写 ---

func TestRewriteCodexEnvironmentContextTimezone(t *testing.T) {
	now := time.Date(2026, 6, 20, 8, 30, 0, 0, time.UTC)

	t.Run("同时改写时区与日期", func(t *testing.T) {
		text := "<environment_context>\n  <current_date>2026-06-20</current_date>\n  <timezone>Asia/Shanghai</timezone>\n  <network enabled=\"true\" />\n</environment_context>"
		// UTC 02:00 在洛杉矶还是 06-19：日期必须跟着时区一起改，不能留下"时区与日期对不上"。
		next, changed := rewriteCodexEnvironmentContextTimezone(text, "America/Los_Angeles", time.Date(2026, 6, 20, 2, 0, 0, 0, time.UTC))
		require.True(t, changed)
		assert.Contains(t, next, "<current_date>2026-06-19</current_date>")
		assert.Contains(t, next, "<timezone>America/Los_Angeles</timezone>")
		assert.Contains(t, next, "<network enabled=\"true\" />", "环境块其它部分不动")
	})

	t.Run("幂等", func(t *testing.T) {
		text := "<environment_context>\n  <current_date>2026-06-20</current_date>\n  <timezone>America/Los_Angeles</timezone>\n</environment_context>"
		next, changed := rewriteCodexEnvironmentContextTimezone(text, "America/Los_Angeles", now)
		assert.False(t, changed)
		assert.Equal(t, text, next)
	})

	t.Run("没有环境块不改", func(t *testing.T) {
		text := "hello <timezone>Asia/Shanghai</timezone>"
		next, changed := rewriteCodexEnvironmentContextTimezone(text, "America/Los_Angeles", now)
		assert.False(t, changed)
		assert.Equal(t, text, next)
	})

	t.Run("客户端没发过时区就不新增", func(t *testing.T) {
		text := "<environment_context>\n  <current_date>2026-06-20</current_date>\n</environment_context>"
		next, changed := rewriteCodexEnvironmentContextTimezone(text, "America/Los_Angeles", now)
		assert.False(t, changed)
		assert.Equal(t, text, next)
	})

	t.Run("非法目标时区不改", func(t *testing.T) {
		text := "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"
		next, changed := rewriteCodexEnvironmentContextTimezone(text, "Not/AZone", now)
		assert.False(t, changed)
		assert.Equal(t, text, next)
	})
}

// --- map / raw 两条路径 ---

func TestRewriteCodexRequestTimezoneMap(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	t.Run("input 数组内文本项", func(t *testing.T) {
		body := timezoneTestBody("Asia/Shanghai", "2026-06-20")
		require.True(t, rewriteCodexRequestTimezoneMap(body, "America/Los_Angeles", now))
		text := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		assert.Contains(t, text, "<timezone>America/Los_Angeles</timezone>")
		assert.NotContains(t, text, "Asia/Shanghai")
	})

	t.Run("input 为字符串", func(t *testing.T) {
		body := map[string]any{"input": "<environment_context><timezone>Asia/Shanghai</timezone></environment_context>"}
		require.True(t, rewriteCodexRequestTimezoneMap(body, "America/Los_Angeles", now))
		assert.Contains(t, body["input"].(string), "<timezone>America/Los_Angeles</timezone>")
	})

	t.Run("无环境块返回 false", func(t *testing.T) {
		body := map[string]any{"input": []any{map[string]any{"content": []any{map[string]any{"text": "hello"}}}}}
		assert.False(t, rewriteCodexRequestTimezoneMap(body, "America/Los_Angeles", now))
	})
}

func TestRewriteCodexRequestTimezoneRaw(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	decodedText := func(t *testing.T, body []byte) string {
		t.Helper()
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded), "改写后仍是合法 JSON")
		assert.Equal(t, "gpt-5.4", decoded["model"], "其它字段不动")
		return decoded["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}

	t.Run("字面量形态（Codex CLI / serde_json）", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <current_date>2026-06-20</current_date>\n  <timezone>Asia/Shanghai</timezone>\n</environment_context>"}]}]}` + "\n")
		next, changed, err := rewriteCodexRequestTimezoneRaw(body, "America/Los_Angeles", now)
		require.NoError(t, err)
		require.True(t, changed)
		assert.True(t, strings.HasSuffix(string(next), "\n"), "尾部字节原样保留")
		text := decodedText(t, next)
		assert.Contains(t, text, "<timezone>America/Los_Angeles</timezone>")
		assert.NotContains(t, text, "Asia/Shanghai")
	})

	t.Run("HTML 转义形态（Go encoding/json）", func(t *testing.T) {
		body, err := json.Marshal(timezoneTestBody("Asia/Shanghai", "2026-06-20"))
		require.NoError(t, err)
		require.NotContains(t, string(body), "<environment_context>", "前提：该形态确实被转义")

		next, changed, err := rewriteCodexRequestTimezoneRaw(body, "America/Los_Angeles", now)
		require.NoError(t, err)
		require.True(t, changed, "转义形态也必须命中")
		assert.Contains(t, decodedText(t, next), "<timezone>America/Los_Angeles</timezone>")
	})

	t.Run("无环境块不改写", func(t *testing.T) {
		plain := []byte(`{"input":[{"content":[{"text":"hello"}]}]}`)
		out, changed, err := rewriteCodexRequestTimezoneRaw(plain, "America/Los_Angeles", now)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, plain, out)
	})
}

func TestApplyCodexRequestTimezoneToAlphaSearchBody(t *testing.T) {
	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "America/Los_Angeles"}, nil)
	body := []byte(`{"id":"x","settings":{"search_context_size":"medium","user_location":{"type":"approximate","city":"Shanghai","country":"CN","timezone":"Asia/Shanghai"}}}`)

	next, changed, err := applyCodexRequestTimezoneToAlphaSearchBody(account, body)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Contains(t, string(next), `"timezone":"America/Los_Angeles"`)
	assert.Contains(t, string(next), `"city":"Shanghai"`, "位置本身不改")

	t.Run("客户端没给时区就不新增", func(t *testing.T) {
		noTimezone := []byte(`{"settings":{"user_location":{"city":"Shanghai"}}}`)
		next, changed, err := applyCodexRequestTimezoneToAlphaSearchBody(account, noTimezone)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, noTimezone, next)
	})

	t.Run("开关关闭时原样", func(t *testing.T) {
		off := newTimezoneTestAccount(nil, nil)
		next, changed, err := applyCodexRequestTimezoneToAlphaSearchBody(off, body)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, body, next)
	})

	t.Run("同值不改写", func(t *testing.T) {
		same := []byte(`{"settings":{"user_location":{"timezone":"America/Los_Angeles"}}}`)
		next, changed, err := applyCodexRequestTimezoneToAlphaSearchBody(account, same)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, same, next)
	})
}

// --- web_search user_location ---

func TestApplyCodexSearchLocationTimezone(t *testing.T) {
	t.Run("改写客户端已给的时区", func(t *testing.T) {
		location := map[string]any{"city": "Shanghai", "country": "CN", "timezone": "Asia/Shanghai"}
		require.True(t, applyCodexSearchLocationTimezone(location, "America/Los_Angeles"))
		assert.Equal(t, "America/Los_Angeles", location["timezone"])
		assert.Equal(t, "Shanghai", location["city"], "城市/地区是客户端的真实位置，不动")
	})

	t.Run("客户端没给时区就不新增", func(t *testing.T) {
		location := map[string]any{"city": "Shanghai", "country": "CN"}
		assert.False(t, applyCodexSearchLocationTimezone(location, "America/Los_Angeles"))
		assert.NotContains(t, location, "timezone")
	})

	t.Run("同值或空目标时区不改", func(t *testing.T) {
		location := map[string]any{"timezone": "America/Los_Angeles"}
		assert.False(t, applyCodexSearchLocationTimezone(location, "America/Los_Angeles"))
		assert.False(t, applyCodexSearchLocationTimezone(location, ""))
		assert.False(t, applyCodexSearchLocationTimezone(nil, "America/Los_Angeles"))
	})
}

func TestBuildOpenAIAlphaSearchResponsesWebSearchBodyTimezone(t *testing.T) {
	alphaBody := []byte(`{"query":"x","settings":{"search_context_size":"medium","user_location":{"type":"approximate","city":"Shanghai","country":"CN","timezone":"Asia/Shanghai"}}}`)

	var payload map[string]any
	body, err := buildOpenAIAlphaSearchResponsesWebSearchBody(alphaBody, "gpt-5.4", "America/Los_Angeles")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &payload))

	tool := payload["tools"].([]any)[0].(map[string]any)
	location := tool["user_location"].(map[string]any)
	assert.Equal(t, "America/Los_Angeles", location["timezone"], "工具侧位置时区被替换")
	assert.Equal(t, "Shanghai", location["city"])

	prompt := payload["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	assert.Contains(t, prompt, "America/Los_Angeles", "prompt 里那份 settings JSON 也要同值")
	assert.NotContains(t, prompt, "Asia/Shanghai")

	t.Run("开关未开时保持原样", func(t *testing.T) {
		body, err := buildOpenAIAlphaSearchResponsesWebSearchBody(alphaBody, "gpt-5.4", "")
		require.NoError(t, err)
		assert.Contains(t, string(body), "Asia/Shanghai")
		assert.NotContains(t, string(body), "America/Los_Angeles")
	})
}

// --- 入口门控与代理解析 ---

func TestApplyCodexRequestTimezoneDoesNotProbeWithoutEnvironmentContext(t *testing.T) {
	prober := &stubProxyExitInfoProber{timezone: "America/Los_Angeles"}
	SetCodexRequestTimezoneResolver(NewCodexRequestTimezoneResolver(prober))
	t.Cleanup(func() { SetCodexRequestTimezoneResolver(nil) })

	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, &Proxy{ID: 1, Protocol: "http", Host: "127.0.0.1", Port: 8080})

	body := []byte(`{"input":[{"content":[{"text":"hello"}]}]}`)
	out, changed, err := applyCodexRequestTimezoneRaw(account, body)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, body, out)
	assert.Zero(t, prober.calls.Load(), "没有环境块时不得触发代理探测")

	assert.False(t, applyCodexRequestTimezoneMap(account, map[string]any{"input": "hello"}))
	assert.Zero(t, prober.calls.Load())
}

func TestCodexRequestTimezoneResolverFixedModeNeedsNoProxy(t *testing.T) {
	prober := &stubProxyExitInfoProber{timezone: "Asia/Shanghai"}
	resolver := NewCodexRequestTimezoneResolver(prober)
	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "America/New_York"}, nil)
	assert.Equal(t, "America/New_York", resolver.TimezoneForAccount(account))
	assert.Zero(t, prober.calls.Load())
}

func TestCodexRequestTimezoneResolverFollowsProxyAndCaches(t *testing.T) {
	prober := &stubProxyExitInfoProber{timezone: "America/Los_Angeles", release: make(chan struct{})}
	resolver := NewCodexRequestTimezoneResolver(prober)
	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, &Proxy{ID: 9, Protocol: "socks5", Host: "10.0.0.9", Port: 1080})

	// 请求路径不等待探测：探测被卡住时调用必须立刻返回空串（不改写）。
	assert.Empty(t, resolver.TimezoneForAccount(account))
	require.Eventually(t, func() bool {
		return prober.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "后台探测应已发起")

	close(prober.release)
	require.Eventually(t, func() bool {
		return resolver.TimezoneForAccount(account) == "America/Los_Angeles"
	}, 2*time.Second, 10*time.Millisecond, "后台探测完成后应命中缓存")

	calls := prober.calls.Load()
	for i := 0; i < 5; i++ {
		assert.Equal(t, "America/Los_Angeles", resolver.TimezoneForAccount(account))
	}
	assert.Equal(t, calls, prober.calls.Load(), "缓存有效期内不得重复探测")
}

func TestCodexRequestTimezoneResolverFailOpen(t *testing.T) {
	prober := &stubProxyExitInfoProber{err: errors.New("proxy blocked ip-api")}
	resolver := NewCodexRequestTimezoneResolver(prober)
	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, &Proxy{ID: 11, Protocol: "http", Host: "10.0.0.11", Port: 8080})

	assert.Empty(t, resolver.TimezoneForAccount(account))
	require.Eventually(t, func() bool {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return len(resolver.entries) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Empty(t, resolver.TimezoneForAccount(account), "探测失败时保持不改写")

	t.Run("没有代理时不探测", func(t *testing.T) {
		bare := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, nil)
		before := prober.calls.Load()
		assert.Empty(t, resolver.TimezoneForAccount(bare))
		assert.Equal(t, before, prober.calls.Load())
	})
}

func TestCodexRequestTimezoneResolverRejectsUnknownTimezoneFromProbe(t *testing.T) {
	prober := &stubProxyExitInfoProber{timezone: "not a timezone"}
	resolver := NewCodexRequestTimezoneResolver(prober)
	account := newTimezoneTestAccount(map[string]any{codexRequestTimezoneExtraKey: "auto"}, &Proxy{ID: 13, Protocol: "http", Host: "10.0.0.13", Port: 8080})

	assert.Empty(t, resolver.TimezoneForAccount(account))
	require.Eventually(t, func() bool {
		resolver.mu.Lock()
		defer resolver.mu.Unlock()
		return len(resolver.entries) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Empty(t, resolver.TimezoneForAccount(account))
}
