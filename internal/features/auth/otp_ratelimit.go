package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/infra/ratelimit"
)

// OTPRateLimit 是挂在 POST /auth/email/request 上的按 IP 限流（三层防滥用的第 ② 层）。
//
// 它挡的是**枚举不同邮箱**——按邮箱冷却（第 ① 层）对那一类完全无效，因为每个请求
// 用的都是新邮箱，冷却窗口永远命不中。
//
// 返回 429 而不是像成功路径那样返回 200：这一层拦的是明确的滥用，不涉及邮箱存在性，
// 没有需要隐藏的信息，而真实客户端需要这个状态码才能实现退避。
// 特意**不是** 401 —— Flutter 的 dio 拦截器见 401 会强制登出（同 ADR-068 那个坑）。
func OTPRateLimit(l *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !l.Allow(clientIP(c)) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				gin.H{"error": "请求过于频繁，请稍后再试"})
			return
		}
		c.Next()
	}
}

// clientIP 显式读 X-Real-IP，**刻意不用 gin 的 c.ClientIP()**。
//
// c.ClientIP() 默认优先信 X-Forwarded-For，而 XFF 是可追加的链：客户端自己塞一段假地址，
// nginx 的 $proxy_add_x_forwarded_for 会把真实地址追加在其后，于是最左侧那项由攻击者控制
// —— 按它分桶等于把限流器交给攻击者随意重置。
//
// X-Real-IP 则由 nginx 用 $remote_addr **覆盖**写入（不是追加），而 $remote_addr 经
// realip 模块改写后就是 Cloudflare 的 CF-Connecting-IP（ADR-074），伪造不了。
// 生产上 api 只监听 127.0.0.1、只能经 nginx 到达，所以这个头可信；
// dev 直连没有这个头，退回 RemoteIP。
func clientIP(c *gin.Context) string {
	if ip := strings.TrimSpace(c.GetHeader("X-Real-IP")); ip != "" {
		return ip
	}
	return c.RemoteIP()
}
