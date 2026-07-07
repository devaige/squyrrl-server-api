package parser

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// 构造与 server.go 相同的注册顺序，验证导出清单的两个核心不变量。
func testRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewYouTubeProvider())
	r.Register(NewGistProvider())
	r.Register(NewRedditProvider())
	r.Register(NewGenericOGProvider())
	return r
}

// 通用 OG 兜底匹配任意 http(s)，属「暂存/enrich」而非「支持解析」，绝不能进清单。
func TestManifestExcludesGenericFallback(t *testing.T) {
	m := testRegistry().Manifest()

	got := map[string]bool{}
	for _, e := range m.Providers {
		got[e.ID] = true
	}
	for _, want := range []string{"youtube", "gist", "reddit"} {
		if !got[want] {
			t.Errorf("清单缺少专用 provider %q", want)
		}
	}
	if got["url"] {
		t.Error("清单不应包含通用 OG 兜底（id=url）")
	}
	if len(m.Providers) != 3 {
		t.Errorf("期望 3 个专用 provider，实得 %d", len(m.Providers))
	}
}

// 单一真源：导出的 patterns 必须与各 provider Match 用的正则一致——
// 用导出的正则对已知命中链接做 hasMatch，结果应与 Match 一致。
func TestManifestPatternsMatchProviderBehavior(t *testing.T) {
	cases := map[string]string{
		"youtube": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"gist":    "https://gist.github.com/octocat/6cff45a4a3b1a3b1c0ff",
		"reddit":  "https://www.reddit.com/r/golang/comments/abc123/title",
	}
	m := testRegistry().Manifest()
	for _, e := range m.Providers {
		uri, ok := cases[e.ID]
		if !ok {
			continue
		}
		matched := false
		for _, pat := range e.Patterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				t.Fatalf("provider %s 的导出正则无法编译: %v", e.ID, err)
			}
			if re.MatchString(uri) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("provider %s 的导出正则未命中已知链接 %q", e.ID, uri)
		}
	}
}

// 混淆是对称的：反混淆 = base64 解码后再跑一次 manifestXOR，应还原明文；
// 且输出不得含明文片段（防一眼抓包）。前后端固化的这套逻辑须逐字节一致。
func TestManifestObfuscationRoundTrip(t *testing.T) {
	plain := []byte(`{"version":"abc","providers":[{"id":"youtube","patterns":["youtu\\.be/x"]}]}`)

	wire := obfuscateManifest(plain)
	if strings.Contains(wire, "youtube") || strings.Contains(wire, "version") {
		t.Error("混淆输出不应含明文片段")
	}

	raw, err := base64.StdEncoding.DecodeString(wire)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	got := manifestXOR(raw) // XOR 对称，再跑一次即还原
	if string(got) != string(plain) {
		t.Errorf("反混淆未还原明文\n want %s\n got  %s", plain, got)
	}
}

// 版本号是内容哈希：同内容稳定、非空。
func TestManifestVersionStable(t *testing.T) {
	v1 := testRegistry().Manifest().Version
	v2 := testRegistry().Manifest().Version
	if v1 == "" {
		t.Fatal("version 不应为空")
	}
	if v1 != v2 {
		t.Errorf("同内容应得相同 version，得到 %q vs %q", v1, v2)
	}
}
