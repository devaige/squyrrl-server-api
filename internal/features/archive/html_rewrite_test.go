package archive

import (
	"net/url"
	"strings"
	"testing"
)

func TestRewriteAttr_ImgSrc(t *testing.T) {
	html := `<p>hi</p><img src="logo.png" alt="x"><img src='/a/b.png'><img class="x" src=foo.png>`
	out := rewriteAttr(html, "img", "src", func(s string) string {
		return "data:image/png;base64,AAAA"
	})
	if !strings.Contains(out, `src="data:image/png;base64,AAAA"`) {
		t.Fatalf("rewrite missing: %s", out)
	}
	// 非 img 标签不应被改
	if !strings.Contains(out, "<p>hi</p>") {
		t.Fatalf("unrelated tag changed")
	}
}

func TestInlineStylesheets(t *testing.T) {
	base, _ := url.Parse("https://example.com/page")
	html := `<link rel="stylesheet" href="/main.css"><div>x</div>`
	out := inlineStylesheets(html, base, func(abs string) ([]byte, bool) {
		if abs == "https://example.com/main.css" {
			return []byte("body{color:red}"), true
		}
		return nil, false
	})
	if !strings.Contains(out, "<style>body{color:red}</style>") {
		t.Fatalf("inline failed: %s", out)
	}
}

func TestResolveURL(t *testing.T) {
	base, _ := url.Parse("https://example.com/a/b")
	cases := map[string]string{
		"x.png":              "https://example.com/a/x.png",
		"/c.png":             "https://example.com/c.png",
		"https://cdn/y.png":  "https://cdn/y.png",
		"data:image/png,abc": "data:image/png,abc",
		"":                   "",
	}
	for in, want := range cases {
		got := resolveURL(base, in)
		if got != want {
			t.Errorf("resolveURL(%q) = %q, want %q", in, got, want)
		}
	}
}
