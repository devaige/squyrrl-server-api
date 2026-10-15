package entry

import (
	"net/http/httptest"
	"testing"
)

func TestOf(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
		want  Entry
	}{
		{"没有头", "", false, Global},
		{"大陆中继", "cn", true, CN},
		// 只认 nginx 写出的精确值；其它写法都不是中继块产出的
		{"大写", "CN", true, Global},
		{"带空白", " cn", true, Global},
		{"空值", "", true, Global},
		{"其它值", "ww", true, Global},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/files/x/ticket", nil)
		if c.set {
			r.Header.Set(Header, c.value)
		}
		if got := Of(r); got != c.want {
			t.Errorf("%s：Of = %v，want %v", c.name, got, c.want)
		}
	}
}
