package quota

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// WriteIfQuota 在 err 是配额/门槛错误时写出 402 并返回 true，否则不碰响应、返回 false。
//
// 各 feature 的 writeErr 只需在开头加一句 `if quota.WriteIfQuota(c, err) { return }`，
// 不必各自记住 402 的响应形状 —— 那个形状（reason / limit / required_plan）是客户端
// 分流的依据，散落成多份迟早会漂移。
//
// **刻意不用 401**：Flutter 的 dio 拦截器见到 401 会强制登出（ADR-068 踩过的坑），
// 而「配额满了」绝不该把用户踢下线。
func WriteIfQuota(c *gin.Context, err error) bool {
	qe, ok := IsQuotaError(err)
	if !ok {
		return false
	}
	c.JSON(http.StatusPaymentRequired, qe)
	return true
}
