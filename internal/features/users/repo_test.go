package users

import "testing"

func TestContainsPattern(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通邮箱片段", "alice@", "%alice@%"},
		// 下划线不转义会匹配任意单字符：搜 john_doe 会把 johnxdoe 也带出来。
		{"下划线按字面匹配", "john_doe", `%john\_doe%`},
		{"百分号按字面匹配", "100%", `%100\%%`},
		// 反斜杠是 LIKE 的默认转义符，不先转义它，后面两条的转义会被它吞掉。
		{"反斜杠按字面匹配", `a\b`, `%a\\b%`},
		{"混合", `a\_%`, `%a\\\_\%%`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containsPattern(c.in); got != c.want {
				t.Errorf("containsPattern(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultLimit},
		{-5, DefaultLimit},
		{1, 1},
		{MaxLimit, MaxLimit},
		{MaxLimit + 1, MaxLimit},
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}
