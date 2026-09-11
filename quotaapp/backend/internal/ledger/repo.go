package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Repo struct{ DB *sql.DB }

func New(db *sql.DB) *Repo { return &Repo{DB: db} }

// Reserve 在一个事务里完成：锁住团队行 -> 校验容量 -> 写预留流水 -> 建会话。
// 并发争抢时行锁把两个预留串行化，第二个看到的余额已经是扣减后的，
// 因此两个并发上传抢最后一点空间时，恰好一个成功、一个 quota exceeded。
func (r *Repo) Reserve(ctx context.Context, teamID int64, key, objectKey, contentType string,
	declareSize, partSize int64, totalParts, retentionSeconds int, ttl time.Duration) (*Session, error) {

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// 幂等优先：同键重放（重试/回调乱序）必须先命中原会话，
	// 即使此刻容量已被自己的预留占满，也不能返回 quota_exceeded。
	var existingID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM upload_sessions WHERE idempotency_key=$1`, key).Scan(&existingID)
	if err == nil {
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		return r.GetSession(ctx, existingID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var occupied, reserved, quota int64
	// FOR UPDATE：同一团队的预留/提交/释放全部在此排队
	err = tx.QueryRowContext(ctx,
		`SELECT occupied_bytes, reserved_bytes, quota_bytes FROM teams WHERE id=$1 FOR UPDATE`,
		teamID).Scan(&occupied, &reserved, &quota)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if occupied+reserved+declareSize > quota {
		return nil, fmt.Errorf("%w: 需要 %d, 配额 %d, 已占用 %d, 已预留 %d, 剩余可用 %d",
			ErrQuotaExceeded, declareSize, quota, occupied, reserved, quota-occupied-reserved)
	}

	expiresAt := time.Now().Add(ttl)
	var s Session
	var content sql.NullString
	if contentType != "" {
		content = sql.NullString{String: contentType, Valid: true}
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO upload_sessions
		  (team_id, idempotency_key, object_key, content_type, declare_size,
		   part_size, total_parts, status, retention_seconds, expires_at)
		VALUES ($1,$2,$3,COALESCE($4,'application/octet-stream'),$5,$6,$7,'reserved',$8,$9)
		RETURNING id, created_at, expires_at`,
		teamID, key, objectKey, content, declareSize, partSize, totalParts, retentionSeconds, expiresAt,
	).Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		// idempotency_key 唯一冲突 = 同键重试，直接返回已有会话（不重复预留）
		if isUniqueViolationErr(err) {
			return r.GetSessionByKey(ctx, key)
		}
		return nil, err
	}

	if err := insertEntry(ctx, tx, teamID, &s.ID, nil, "reserve", declareSize, 0, key,
		fmt.Sprintf("预留上传 %s (%d bytes)", objectKey, declareSize)); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.GetSession(ctx, s.ID)
}

// AttachUploadID 记录 MinIO multipart upload id。
func (r *Repo) AttachUploadID(ctx context.Context, sessionID int64, uploadID string) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE upload_sessions SET upload_id=$2,
		   status = CASE WHEN status='reserved' THEN 'initiated' ELSE status END
		 WHERE id=$1`, sessionID, uploadID)
	return err
}

// MarkUploading 标记至少领取过分片 URL（展示用）。
func (r *Repo) MarkUploading(ctx context.Context, sessionID int64) {
	_, _ = r.DB.ExecContext(ctx,
		`UPDATE upload_sessions SET status='uploading' WHERE id=$1 AND status IN ('reserved','initiated')`,
		sessionID)
}

var activeStatuses = map[string]bool{"reserved": true, "initiated": true, "uploading": true}

// Commit 预留转占用。以实际字节（MinIO HeadObject）为准：
// 先释放 declare_size 预留，再占用 finalSize；容量不够直接回滚（客户端需 abort）。
// 幂等：同 idempotency_key 的完成/回调重放直接返回已完成会话，绝不二次入账。
//
// complete 回调在持有团队行锁的事务内执行，负责 MinIO CompleteMultipartUpload
// 并把服务端实测字节写回 *finalSize；回调返回错误则整个事务回滚，
// 不会出现“账记了占用但对象不存在”。
func (r *Repo) Commit(ctx context.Context, sessionID int64, key string, declaredSize int64,
	complete func(tx *sql.Tx, finalSize *int64) error) (*Session, error) {

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var s Session
	var status, objectKey, contentType string
	var teamID, declareSize int64
	var retentionSeconds int
	var uploadID sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, team_id, status, object_key, COALESCE(content_type,'application/octet-stream'),
		       declare_size, retention_seconds, expires_at, upload_id
		FROM upload_sessions WHERE id=$1 FOR UPDATE`, sessionID,
	).Scan(&s.ID, &teamID, &status, &objectKey, &contentType, &declareSize,
		&retentionSeconds, &s.ExpiresAt, &uploadID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case status == "completed":
		// 乱序/重试的完成回调：幂等返回，不重复记账
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		return r.GetSession(ctx, sessionID)
	case !activeStatuses[status]:
		return nil, fmt.Errorf("%w: 会话状态 %s 不可完成", ErrConflict, status)
	case time.Now().After(s.ExpiresAt):
		return nil, fmt.Errorf("%w: 会话已超过 TTL，预留将被清扫", ErrConflict)
	}

	var occupied, quota int64
	if err := tx.QueryRowContext(ctx,
		`SELECT occupied_bytes, quota_bytes FROM teams WHERE id=$1 FOR UPDATE`,
		teamID).Scan(&occupied, &quota); err != nil {
		return nil, err
	}

	finalSize := declaredSize
	// 回调钩子：MinIO CompleteMultipartUpload，放在持锁事务中，
	// 保证“完成 MinIO”与“入账”要么一起成功，要么一起回滚。
	if complete != nil {
		if err := complete(tx, &finalSize); err != nil {
			return nil, err
		}
	}
	if finalSize < 0 {
		return nil, fmt.Errorf("非法对象大小 %d", finalSize)
	}
	// 释放 declareSize 预留后，真实占用 finalSize 仍须在配额内
	if occupied+finalSize > quota {
		return nil, fmt.Errorf("%w: 实际上传 %d 字节超出配额（占用 %d / 配额 %d）。预留将自动释放，请重新申请",
			ErrQuotaExceeded, finalSize, occupied, quota)
	}

	now := time.Now()
	var retentionUntil sql.NullTime
	if retentionSeconds > 0 {
		retentionUntil = sql.NullTime{Time: now.Add(time.Duration(retentionSeconds) * time.Second), Valid: true}
	}
	var objectID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO objects (team_id, session_id, object_key, size_bytes, content_type, retention_until)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		teamID, sessionID, objectKey, finalSize, contentType, retentionUntil).Scan(&objectID); err != nil {
		return nil, err
	}

	sid := sessionID
	// 1) 释放整笔预留
	if err := insertEntry(ctx, tx, teamID, &sid, nil, "release", -declareSize, 0,
		key+":release", "完成上传，释放预留"); err != nil {
		return nil, err
	}
	// 2) 记录真实占用
	if err := insertEntry(ctx, tx, teamID, &sid, &objectID, "commit", 0, finalSize,
		key+":commit", fmt.Sprintf("对象 %s 实际上传 %d bytes", objectKey, finalSize)); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE upload_sessions
		   SET status='completed', final_size=$2, completed_at=$3, object_id=$4
		 WHERE id=$1`, sessionID, finalSize, now, objectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = uploadID
	return r.GetSession(ctx, sessionID)
}

// Release 取消/失败：无条件幂等地释放预留。只有活跃会话会真正写流水，
// completed/aborted/expired 的重复回调（乱序 abort）直接返回，不重复释放。
func (r *Repo) Release(ctx context.Context, sessionID int64, reason string) (*Session, bool, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var teamID, declareSize int64
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT team_id, declare_size, status FROM upload_sessions WHERE id=$1 FOR UPDATE`,
		sessionID).Scan(&teamID, &declareSize, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if !activeStatuses[status] {
		// 已终态：幂等无操作（例如 abort 与 complete 回调乱序到达）
		if err := tx.Rollback(); err != nil {
			return nil, false, err
		}
		s, err := r.GetSession(ctx, sessionID)
		return s, false, err
	}

	sid := sessionID
	if err := insertEntry(ctx, tx, teamID, &sid, nil, "release", -declareSize, 0,
		fmt.Sprintf("session-%d-release:%s", sessionID, reason), reason); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE upload_sessions SET status='aborted', aborted_at=now() WHERE id=$1`,
		sessionID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	s, err := r.GetSession(ctx, sessionID)
	return s, true, err
}

// ExpireReservations 清扫超时会话：逐行锁定，释放预留并置 expired。
// 返回被处理的会话 id，MinIO multipart 的中止由调用方在事务外补偿。
func (r *Repo) ExpireReservations(ctx context.Context, now time.Time) ([]expiredSession, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, team_id, declare_size, COALESCE(upload_id,''), status
		FROM upload_sessions
		WHERE expires_at < $1 AND status IN ('reserved','initiated','uploading')
		ORDER BY id FOR UPDATE SKIP LOCKED`, now)
	if err != nil {
		return nil, err
	}
	type row struct{ id, teamID, declare int64; uploadID, status string }
	var picked []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.teamID, &x.declare, &x.uploadID, &x.status); err != nil {
			rows.Close()
			return nil, err
		}
		picked = append(picked, x)
	}
	rows.Close()

	var out []expiredSession
	for _, p := range picked {
		tx, err := r.DB.BeginTx(ctx, nil)
		if err != nil {
			return out, err
		}
		sid := p.id
		if err := insertEntry(ctx, tx, p.teamID, &sid, nil, "release", -p.declare, 0,
			fmt.Sprintf("session-%d-expiry", p.id), "会话 TTL 到期，自动释放预留"); err != nil {
			_ = tx.Rollback()
			return out, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE upload_sessions SET status='expired', aborted_at=now() WHERE id=$1`, p.id); err != nil {
			_ = tx.Rollback()
			return out, err
		}
		if err := tx.Commit(); err != nil {
			return out, err
		}
		out = append(out, expiredSession{ID: p.id, TeamID: p.teamID, UploadID: p.uploadID})
	}
	return out, nil
}

type expiredSession struct {
	ID       int64
	TeamID   int64
	UploadID string
}

// DeleteObject 删除未保留对象。保留期未到一律拒绝（管理界面和 API 走同一道校验，无法绕过）。
func (r *Repo) DeleteObject(ctx context.Context, teamID, objectID int64, key string, bypassRetention bool) (*Object, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var o Object
	var retention sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, team_id, session_id, object_key, size_bytes,
		       COALESCE(content_type,'application/octet-stream'), retention_until
		FROM objects WHERE id=$1 FOR UPDATE`, objectID,
	).Scan(&o.ID, &o.TeamID, &o.SessionID, &o.ObjectKey, &o.SizeBytes, &o.ContentType, &retention)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if o.TeamID != teamID {
		return nil, ErrNotFound
	}
	if retention.Valid && time.Now().Before(retention.Time) && !bypassRetention {
		return nil, &RetentionError{Until: retention.Time}
	}

	sid := o.SessionID
	oid := o.ID
	if err := insertEntry(ctx, tx, teamID, &sid, &oid, "delete", 0, -o.SizeBytes, key,
		fmt.Sprintf("删除对象 %s，释放占用 %d bytes", o.ObjectKey, o.SizeBytes)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE objects SET deleted_at=now() WHERE id=$1`, objectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.RetentionUntil = nullTimePtr(retention)
	return &o, nil
}

// ReclaimExpiredObjects 返回保留期已过但尚未标记删除的对象（可回收空间），
// 真正落盘删除由 sweeper 调 DeleteObject 完成。
func (r *Repo) ListReclaimable(ctx context.Context, now time.Time) ([]Object, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, team_id, session_id, object_key, size_bytes,
		       COALESCE(content_type,'application/octet-stream'), retention_until
		FROM objects
		WHERE deleted_at IS NULL AND retention_until IS NOT NULL AND retention_until < $1
		ORDER BY retention_until`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		var retention sql.NullTime
		if err := rows.Scan(&o.ID, &o.TeamID, &o.SessionID, &o.ObjectKey, &o.SizeBytes,
			&o.ContentType, &retention); err != nil {
			return nil, err
		}
		o.RetentionUntil = nullTimePtr(retention)
		out = append(out, o)
	}
	return out, rows.Err()
}

type RetentionError struct{ Until time.Time }

func (e *RetentionError) Error() string {
	return fmt.Sprintf("对象处于保留期内，直到 %s 前不可删除", e.Until.Format(time.RFC3339))
}

var ErrNotFound = errors.New("not found")

func nullTimePtr(t sql.NullTime) *time.Time {
	if t.Valid {
		u := t.Time
		return &u
	}
	return nil
}
