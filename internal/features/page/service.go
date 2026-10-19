package page

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/squyrrl/api/internal/features/quota"
)

type Service struct {
	repo  *Repo
	quota *quota.Service
}

func NewService(repo *Repo, q *quota.Service) *Service {
	return &Service{repo: repo, quota: q}
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in *CreateInput) (*Page, error) {
	if err := s.quota.CheckPageCreate(ctx, userID); err != nil {
		return nil, err
	}
	// 隐藏页面是档位能力（standard 起），与页面总数是两条独立门槛：
	// 额度没满但档位不够时，要告诉用户「升级才能隐藏」而不是「页面太多」。
	if in.IsHidden {
		if err := s.quota.CheckHiddenPage(ctx, userID); err != nil {
			return nil, err
		}
	}
	return s.repo.Create(ctx, userID, in)
}

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (*Page, error) {
	return s.repo.Get(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Page, error) {
	return s.repo.List(ctx, userID)
}

func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in *UpdateInput) (*Page, error) {
	// 把已有页面改成隐藏，与新建一个隐藏页面是同一件事，门槛必须一致 ——
	// 只在 Create 上设卡的话，「先建普通页、再 PATCH 成隐藏」就是一条完整的绕过路径。
	// 反向（取消隐藏）不设限：降级用户应当能把自己的隐藏页改回可见。
	if in.IsHidden != nil && *in.IsHidden {
		if err := s.quota.CheckHiddenPage(ctx, userID); err != nil {
			return nil, err
		}
	}
	return s.repo.Update(ctx, userID, id, in)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.repo.SoftDelete(ctx, userID, id)
}

// Outcome 是一次同步写入之后，这个页面在服务端的样子。
type Outcome struct {
	Page    *Page // Deleted 时为 nil
	Deleted bool
}

// Put 把页面写成 in 的样子：没有就新建，有就覆盖（ADR-084：以后到达服务端的为准）。
// 已删除的页面不会被一次迟到的改名复活 —— 删除赢过归类上的修改，碎片本身不受影响。
// 系统页面（冲突收件箱）只接受调整顺序。
func (s *Service) Put(ctx context.Context, userID, id uuid.UUID, in *PutInput) (*Outcome, error) {
	cur, deleted, err := s.repo.getAny(ctx, userID, id)
	if errors.Is(err, ErrNotFound) {
		p, err := s.Create(ctx, userID, &CreateInput{
			ID: &id, Name: in.Name, IsHidden: in.IsHidden, OrderIdx: in.OrderIdx, CreatedAt: in.CreatedAt,
		})
		if isUniqueViolation(err) {
			// 同一次新建并发落了库就按已存在再走一遍；不是本账号的就是 id 被占用
			if _, _, gerr := s.repo.getAny(ctx, userID, id); errors.Is(gerr, ErrNotFound) {
				return nil, ErrIDTaken
			}
			return s.Put(ctx, userID, id, in)
		}
		if err != nil {
			return nil, err
		}
		return &Outcome{Page: p}, nil
	}
	if err != nil {
		return nil, err
	}
	if deleted {
		return &Outcome{Deleted: true}, nil
	}

	upd := &UpdateInput{}
	if !cur.IsSystem {
		if cur.Name != in.Name {
			upd.Name = &in.Name
		}
		if cur.IsHidden != in.IsHidden {
			upd.IsHidden = &in.IsHidden
		}
	}
	if cur.OrderIdx != in.OrderIdx {
		upd.OrderIdx = &in.OrderIdx
	}
	if upd.Name == nil && upd.IsHidden == nil && upd.OrderIdx == nil {
		return &Outcome{Page: cur}, nil
	}
	p, err := s.Update(ctx, userID, id, upd)
	if errors.Is(err, ErrNotFound) {
		return &Outcome{Deleted: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &Outcome{Page: p}, nil
}

// DeleteSync 删除页面；已经不在算成功。旗下碎片回到「全部碎片」。
func (s *Service) DeleteSync(ctx context.Context, userID, id uuid.UUID) (*Outcome, error) {
	err := s.repo.SoftDelete(ctx, userID, id)
	if err == nil || errors.Is(err, ErrNotFound) {
		return &Outcome{Deleted: true}, nil
	}
	return nil, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
