package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Middleware 解析 Authorization: Bearer <token>，校验后将 Identity 注入 Gin Context
// 失败时直接 abort 出 401，下游 handler 不需要再判空
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "缺少 Bearer token"})
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "空 token"})
			return
		}

		id, err := s.ValidateAccessToken(c.Request.Context(), token)
		if err != nil {
			if errors.Is(err, ErrSessionRevoked) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 无效或已过期"})
				return
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		setIdentity(c, id)
		c.Next()
	}
}
