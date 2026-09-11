package snippet

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
)

// ExportHandler 提供 GET /me/export。
//
// 它存在的理由是 ADR-075 的降级生命周期：数据在 60 天后会被永久删除，而在此之前
// 用户必须有办法把它整批拿走。逐条查看不算「拿得走」—— 一个有几万条碎片的人，
// 那等于没有出口。
type ExportHandler struct {
	exp *Exporter
}

func NewExportHandler(exp *Exporter) *ExportHandler { return &ExportHandler{exp: exp} }

func (h *ExportHandler) RegisterUser(g *gin.RouterGroup) {
	g.GET("/export", h.export)
}

func (h *ExportHandler) export(c *gin.Context) {
	id := auth.MustIdentity(c)

	name := fmt.Sprintf("squyrrl-cloud-%s.json", time.Now().Format("20060102-1504"))
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	// 不设 Content-Length：整份内容是流式产出的，长度要等写完才知道，
	// 而先算一遍长度就等于把「流式」这件事本身取消掉。
	c.Status(http.StatusOK)

	if err := h.exp.Export(c.Request.Context(), id.UserID, c.Writer); err != nil {
		// 响应头与部分正文已经发出去了，这里改不了状态码。
		// 记日志并中断连接：一个**被截断的 JSON** 在客户端一定解析失败，
		// 那正是我们要的结果 —— 用户拿到一个明确的错误，而不是一份静默残缺的备份。
		slog.Error("云端数据导出失败", "user_id", id.UserID, "err", err)
		c.Abort()
	}
}
