package replica

import (
	"errors"

	"github.com/gin-gonic/gin/binding"
	"github.com/google/uuid"

	"github.com/squyrrl/api/internal/features/page"
	"github.com/squyrrl/api/internal/features/quota"
	"github.com/squyrrl/api/internal/features/snippet"
	"github.com/squyrrl/api/internal/features/tag"
)

const (
	EntitySnippet = "snippet"
	EntityPage    = "page"
	EntityTag     = "tag"

	// OpPut 新建或覆盖。碎片只覆盖正文类字段，并核对 base_version；页面、标签以后到达的为准。
	OpPut = "put"
	// OpDelete 删除。碎片核对 base_version，别处先改了正文就拒绝删除。
	OpDelete = "delete"
	// OpMove 只用于碎片：移进 page_id，null = 移出页面。
	OpMove = "move"
	// OpTags 只用于碎片：加上 add、摘掉 remove。
	OpTags = "tags"
)

// Mutation 是客户端发件箱里的一条改动。客户端按实体合并后再发：
// 同一条碎片改了五次只发最后的样子，新建后又删掉就什么都不发。
// 页面、标签的新建应排在引用它们的碎片之前。
type Mutation struct {
	Entity string    `json:"entity"`
	Op     string    `json:"op"`
	ID     uuid.UUID `json:"id"`

	// BaseVersion 是这条碎片的改动所基于的服务端版本，0 = 本机新建。只用于碎片的 put / delete。
	BaseVersion int64 `json:"base_version"`

	Snippet *snippet.PutInput `json:"snippet,omitempty"` // snippet put
	PageID  *uuid.UUID        `json:"page_id"`           // snippet move；null = 移出页面
	Add     []uuid.UUID       `json:"add,omitempty"`     // snippet tags
	Remove  []uuid.UUID       `json:"remove,omitempty"`  // snippet tags
	Page    *page.PutInput    `json:"page,omitempty"`    // page put
	Tag     *tag.PutInput     `json:"tag,omitempty"`     // tag put
}

func (m *Mutation) validate() error {
	if m.ID == uuid.Nil {
		return errors.New("缺少 id")
	}
	switch m.Entity {
	case EntitySnippet:
		switch m.Op {
		case OpPut:
			if m.Snippet == nil {
				return errors.New("snippet put 缺少 snippet")
			}
			return binding.Validator.ValidateStruct(m.Snippet)
		case OpDelete, OpMove, OpTags:
			return nil
		}
	case EntityPage:
		switch m.Op {
		case OpPut:
			if m.Page == nil {
				return errors.New("page put 缺少 page")
			}
			return binding.Validator.ValidateStruct(m.Page)
		case OpDelete:
			return nil
		}
	case EntityTag:
		switch m.Op {
		case OpPut:
			if m.Tag == nil {
				return errors.New("tag put 缺少 tag")
			}
			return binding.Validator.ValidateStruct(m.Tag)
		case OpDelete:
			return nil
		}
	default:
		return errors.New("未知的 entity：" + m.Entity)
	}
	return errors.New("未知的 op：" + m.Entity + "/" + m.Op)
}

const (
	// StatusOK 已应用，或服务端本来就是这个样子。客户端采用返回的状态、移出发件箱。
	StatusOK = "ok"
	// StatusConflict 这次改动没有生效：服务端那份保留，客户端的内容已存进冲突收件箱
	// （loser_id，随下一次拉取到达）；或删除因别处先改了正文而被拒。
	// 客户端采用返回的状态、移出发件箱。
	StatusConflict = "conflict"
	// StatusRejected 要等用户处理才可能成功（额度、受限、数据不合法）。
	// 客户端把这条标成失败、保留在本机，不要自动重试。
	StatusRejected = "rejected"
	// StatusRetry 暂时失败，稍后原样重试。
	StatusRetry = "retry"
)

const (
	CodeQuota       = "quota"        // 带 quota 字段，形状同 402 响应体
	CodeInvalid     = "invalid"      // 请求不合法
	CodeIDTaken     = "id_taken"     // id 已被别的账号占用
	CodeFileMissing = "file_missing" // 引用的附件文件在服务端不存在（还没上传完）
	CodeReadOnly    = "read_only"    // 系统页面不可删除
)

// Result 是一条改动的结果，与请求里的 mutations 一一对应。
type Result struct {
	Status string       `json:"status"`
	Code   string       `json:"code,omitempty"`
	Error  string       `json:"error,omitempty"`
	Quota  *quota.Error `json:"quota,omitempty"`

	// 改动之后这个实体在服务端的样子；Deleted 时三者都为空。
	Snippet *snippet.Snippet `json:"snippet,omitempty"`
	Page    *page.Page       `json:"page,omitempty"`
	Tag     *tag.Tag         `json:"tag,omitempty"`
	Deleted bool             `json:"deleted,omitempty"`
	LoserID *uuid.UUID       `json:"loser_id,omitempty"`
}

type PushInput struct {
	Mutations []Mutation `json:"mutations" binding:"required,max=100"`
}

type PushOutput struct {
	Results []Result `json:"results"`
}
