package parser

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// 构造与 server.go 相同的注册顺序，验证导出清单的核心不变量。
func testRegistry() *Registry {
	r := NewRegistry()
	r.Register(NewYouTubeProvider())
	r.Register(NewGistProvider())
	r.Register(NewRedditProvider())
	return r
}

// 清单只列此刻真有来源的平台。没接付费接口时 YouTube 没有任何来源（oEmbed 不算解析），
// 它的链接应当被客户端当普通链接暂存，而不是给一个注定失败的「解析」按钮。
func TestManifestOnlyListsPlatformsWithSources(t *testing.T) {
	svc := NewService(Deps{Registry: testRegistry()})
	m, err := svc.Manifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, e := range m.Providers {
		got[e.ID] = true
	}
	for _, want := range []string{"gist", "reddit"} {
		if !got[want] {
			t.Errorf("有内置免费来源的 %q 应在清单里", want)
		}
	}
	if got["youtube"] {
		t.Error("没有任何来源的 youtube 不应进清单")
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
	m := testRegistry().Manifest(nil, nil)
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
	v1 := testRegistry().Manifest(nil, nil).Version
	v2 := testRegistry().Manifest(nil, nil).Version
	if v1 == "" {
		t.Fatal("version 不应为空")
	}
	if v1 != v2 {
		t.Errorf("同内容应得相同 version，得到 %q vs %q", v1, v2)
	}
}
