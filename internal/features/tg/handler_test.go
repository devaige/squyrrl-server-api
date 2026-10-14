package tg

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// /files/check 是静态段，/files/:id/thumbnail 是参数段，同一个 POST 方法下并存。
// 路由冲突在 gin 里是注册期 panic —— 也就是生产启动即崩，值得一条测试钉住。
func TestRegisterInternalRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewHandler(nil, nil).RegisterInternal(r.Group("/internal/tg"))

	want := map[string]bool{
		"POST /internal/tg/files/check":         false,
		"POST /internal/tg/files":               false,
		"POST /internal/tg/files/:id/thumbnail": false,
	}
	for _, rt := range r.Routes() {
		k := rt.Method + " " + rt.Path
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("缺少路由 %s", k)
		}
	}
}
