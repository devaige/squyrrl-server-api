package parser

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
	"github.com/squyrrl/api/internal/features/wallet"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Register(g *gin.RouterGroup) {
	g.POST("/parse", h.parse)
}

func (h *Handler) parse(c *gin.Context) {
	id := auth.MustIdentity(c)
	var in ParseInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.svc.Parse(c.Request.Context(), id.UserID, in.URI)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoParser):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, wallet.ErrInsufficientCredits):
			c.JSON(http.StatusPaymentRequired, gin.H{"error": "余额不足，请充值后重试"})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
