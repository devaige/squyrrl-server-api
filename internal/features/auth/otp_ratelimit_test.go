package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/infra/ratelimit"
)

func newLimitedEngine(limit int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", OTPRateLimit(ratelimit.New(limit, time.Minute)), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	return r
}

func do(r *gin.Engine, headers map[string]string, remoteAddr string) int {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestOTPRateLimit_BlocksAfterLimit(t *testing.T) {
	r := newLimitedEngine(2)
	h := map[string]string{"X-Real-IP": "203.0.113.7"}
	for i := 1; i <= 2; i++ {
		if got := do(r, h, ""); got != http.StatusOK {
			t.Fatalf("第 %d 次应放行，得到 %d", i, got)
		}
	}
	if got := do(r, h, ""); got != http.StatusTooManyRequests {
		t.Fatalf("超限应为 429，得到 %d", got)
	}
}

// 429 而非 401：Flutter 的 dio 拦截器见 401 会强制登出（ADR-068 踩过的坑），
// 限流误伤绝不该把用户踢下线。
func TestOTPRateLimit_UsesTooManyRequestsNotUnauthorized(t *testing.T) {
	r := newLimitedEngine(1)
	h := map[string]string{"X-Real-IP": "203.0.113.8"}
	do(r, h, "")
	if got := do(r, h, ""); got == http.StatusUnauthorized {
		t.Fatal("绝不能返回 401")
	}
}

// 这条是选用 X-Real-IP 而非 c.ClientIP() 的**全部理由**：
// XFF 是可追加的链，攻击者塞什么进去都不该影响分桶。若改回 c.ClientIP()，
// 下面每次换一个伪造的 XFF 就能拿到一个全新的桶，限流形同虚设。
func TestOTPRateLimit_ForgedXFFCannotResetBucket(t *testing.T) {
	r := newLimitedEngine(2)
	forged := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}
	codes := make([]int, 0, len(forged))
	for _, f := range forged {
		codes = append(codes, do(r, map[string]string{
			"X-Real-IP":       "203.0.113.9", // nginx 覆盖写入，真实来源
			"X-Forwarded-For": f,             // 客户端伪造，每次都不同
		}, ""))
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("前两次应放行，实际 %v", codes)
	}
	if codes[2] != http.StatusTooManyRequests || codes[3] != http.StatusTooManyRequests {
		t.Fatalf("伪造 XFF 不应换到新桶，后两次应为 429，实际 %v", codes)
	}
}

// dev 直连没有 nginx，也就没有 X-Real-IP，必须退回对端地址而不是退化成同一个桶。
func TestOTPRateLimit_FallsBackToRemoteIPWithoutHeader(t *testing.T) {
	r := newLimitedEngine(1)
	if got := do(r, nil, "198.51.100.10:5555"); got != http.StatusOK {
		t.Fatalf("首次应放行，得到 %d", got)
	}
	if got := do(r, nil, "198.51.100.10:6666"); got != http.StatusTooManyRequests {
		t.Fatalf("同一 IP 不同端口应算同一桶，应 429，得到 %d", got)
	}
	if got := do(r, nil, "198.51.100.11:5555"); got != http.StatusOK {
		t.Fatalf("不同 IP 应独立计数，得到 %d", got)
	}
}
