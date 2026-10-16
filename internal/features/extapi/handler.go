package extapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterInternal 挂在 /internal/admin 下，受 X-Internal-Token 头保护，供管理后台调用。
func (h *Handler) RegisterInternal(g *gin.RouterGroup) {
	g.GET("/api-accounts", h.listAccounts)
	g.POST("/api-accounts", h.createAccount)
	g.PATCH("/api-accounts/:id", h.updateAccount)
	g.GET("/api-accounts/:id/ledger", h.listLedger)
	g.POST("/api-accounts/:id/topup", h.topup)

	g.GET("/api-endpoints", h.list)
	g.POST("/api-endpoints", h.create)
	g.GET("/api-endpoints/:id", h.get)
	g.PATCH("/api-endpoints/:id", h.update)
	g.GET("/api-endpoints/:id/samples", h.listSamples)
	g.POST("/api-endpoints/:id/samples", h.addSample)

	g.GET("/api-adapters", h.adapters)
}

func (h *Handler) listAccounts(c *gin.Context) {
	items, err := h.svc.ListAccounts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "vendors": Vendors()})
}

func (h *Handler) createAccount(c *gin.Context) {
	var in CreateAccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	acc, err := h.svc.CreateAccount(c.Request.Context(), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, acc)
}

func (h *Handler) updateAccount(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in UpdateAccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.UpdateAccount(c.Request.Context(), id, in); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// listLedger 默认只列充值与调账；?calls=1 连调用成本一起列。
func (h *Handler) listLedger(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	items, err := h.svc.ListLedger(c.Request.Context(), id, c.Query("calls") == "1")
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) topup(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in TopupInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.Topup(c.Request.Context(), id, in.AmountMicros, in.Note); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) list(c *gin.Context) {
	items, err := h.svc.ListEndpoints(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	ep, err := h.svc.GetEndpoint(c.Request.Context(), id)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, ep)
}

func (h *Handler) create(c *gin.Context) {
	var in CreateEndpointInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ep, err := h.svc.CreateEndpoint(c.Request.Context(), in)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, ep)
}

func (h *Handler) update(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in UpdateEndpointInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.UpdateEndpoint(c.Request.Context(), id, in); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) adapters(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": h.svc.Adapters()})
}

func (h *Handler) listSamples(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	items, err := h.svc.ListSamples(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) addSample(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var in CreateSampleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.svc.AddSample(c.Request.Context(), id, in); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func parseID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return uuid.Nil, false
	}
	return id, true
}

// writeErr 把业务错误映射成 4xx，其余按 500。后台需要的是能直接看懂的原因。
func writeErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case isUniqueViolation(err):
		c.JSON(http.StatusConflict, gin.H{"error": "slug 已存在"})
	case errors.Is(err, ErrUnknownVendor), errors.Is(err, ErrUnknownAdapter), errors.Is(err, ErrVendorMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
