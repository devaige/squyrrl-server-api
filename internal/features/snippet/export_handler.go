package snippet

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/squyrrl/api/internal/features/auth"
)

// ExportHandler 提供 GET /me/export。
//
// 它存在的理由是 ADR-075 的降级生命周期：数据在 60 天后会被永久删除，而在此之前
// 用户必须有办法把它整批拿走。逐条查看不算「拿得走」—— 一个有几万条碎片的人，
// 那等于没有出口。
//
// 代价也正来自这一点：一次调用就是一次全表流式扫描 + 等量的源站出网，上限是
// 至尊档的 1000 万条碎片。所以它是本仓库里唯一一个**按用户冷却**的读端点。
type ExportHandler struct {
	exp      *Exporter
	cooldown time.Duration
}

func NewExportHandler(exp *Exporter, cooldown time.Duration) *ExportHandler {
	return &ExportHandler{exp: exp, cooldown: cooldown}
}

func (h *ExportHandler) RegisterUser(g *gin.RouterGroup) {
	g.GET("/export", h.export)
}

func (h *ExportHandler) export(c *gin.Context) {
	id := auth.MustIdentity(c)

	// 占名额必须在写出任何响应头之前：流一旦开始，状态码就改不了了（见下面那段注释）。
	ok, wait, err := h.exp.ClaimExportSlot(c.Request.Context(), id.UserID, h.cooldown)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		secs := int(wait.Seconds())
		// Retry-After 走**头**，因为退避是传输层的事，dio / fetch 的错误分支不一定会去解 body。
		// body 里那份是给界面做倒计时用的，两者是不同的消费者，不是重复。
		c.Header("Retry-After", strconv.Itoa(secs))
		// 429 而不是 402：这不是「你的档位不够」，升级也不会让它立刻可用。
		// 也刻意不是 401 —— Flutter 的 dio 拦截器见 401 会强制登出（ADR-068 那个坑）。
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":               "导出过于频繁，请稍后再试",
			"retry_after_seconds": secs,
		})
		return
	}

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
		//
		// 名额不退（见 ClaimExportSlot 的说明）：字节与全表扫描在失败之前就发生了。
		slog.Error("云端数据导出失败", "user_id", id.UserID, "err", err)
		c.Abort()
	}
}
