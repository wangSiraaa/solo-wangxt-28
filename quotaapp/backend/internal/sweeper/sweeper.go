package sweeper

import (
	"context"
	"log"
	"time"

	"quotaapp/internal/ledger"
	"quotaapp/internal/store"
)

type Sweeper struct {
	repo   *ledger.Repo
	store  *store.Store
	every  time.Duration
	now    func() time.Time
}

func New(repo *ledger.Repo, st *store.Store, every time.Duration) *Sweeper {
	return &Sweeper{repo: repo, store: st, every: every, now: time.Now}
}

func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	s.Tick(ctx) // 启动即扫一次
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick 一轮清扫：
// 1) 超过 TTL 的上传会话 -> 释放预留（账本），补偿中止 MinIO multipart
// 2) 过了保留期的对象 -> 走与界面删除完全相同的后端校验路径删除
func (s *Sweeper) Tick(ctx context.Context) TickResult {
	var res TickResult
	now := s.now()

	expired, err := s.repo.ExpireReservations(ctx, now)
	if err != nil {
		log.Printf("sweeper: expire reservations: %v", err)
	}
	for _, e := range expired {
		res.SessionsExpired++
		sess, err := s.repo.GetSession(ctx, e.ID)
		if err != nil {
			continue
		}
		if sess.UploadIDV != "" {
			fullKey, err := store.SafeKey(e.TeamID, sess.ObjectKey)
			if err == nil {
				if err := s.store.AbortMultipart(ctx, fullKey, sess.UploadIDV); err != nil {
					log.Printf("sweeper: abort multipart session=%d upload=%s: %v", e.ID, sess.UploadIDV, err)
				}
			}
		}
	}

	objs, err := s.repo.ListReclaimable(ctx, now)
	if err != nil {
		log.Printf("sweeper: list reclaimable: %v", err)
		return res
	}
	for _, o := range objs {
		key := "sweep-object-" + time.Now().Format("20060102T150405.000000") + "-" + itoa(o.ID)
		// 保留期已过，DeleteObject 的后端校验会放行；界面删除走同一函数，不存在绕过通道
		deleted, err := s.repo.DeleteObject(ctx, o.TeamID, o.ID, key, false)
		if err != nil {
			log.Printf("sweeper: mark delete object=%d: %v", o.ID, err)
			continue
		}
		fullKey, err := store.SafeKey(o.TeamID, deleted.ObjectKey)
		if err != nil {
			log.Printf("sweeper: key object=%d: %v", o.ID, err)
			continue
		}
		if err := s.store.DeleteObject(ctx, fullKey); err != nil {
			log.Printf("sweeper: delete from minio object=%d: %v", o.ID, err)
			continue
		}
		res.ObjectsReclaimed++
		res.BytesReclaimed += deleted.SizeBytes
	}
	return res
}

type TickResult struct {
	SessionsExpired int
	ObjectsReclaimed int
	BytesReclaimed  int64
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
