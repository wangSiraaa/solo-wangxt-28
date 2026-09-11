package ledger

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func isUniqueViolationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

func insertEntry(ctx context.Context, tx *sql.Tx, teamID int64, sessionID *int64, objectID *int64,
	entryType string, deltaReserved, deltaOccupied int64, key, reason string) error {
	_, err := tx.ExecContext(ctx, `
		WITH upd AS (
			UPDATE teams SET
				reserved_bytes = reserved_bytes + $5,
				occupied_bytes = occupied_bytes + $6
			WHERE id = $1
			RETURNING reserved_bytes, occupied_bytes
		)
		INSERT INTO ledger_entries
		  (team_id, session_id, object_id, entry_type,
		   delta_reserved, delta_occupied, reserved_after, occupied_after,
		   idempotency_key, reason)
		SELECT $1,$2,$3,$4,$5,$6,u.reserved_bytes,u.occupied_bytes,$7,$8 FROM upd u`,
		teamID, sessionID, objectID, entryType, deltaReserved, deltaOccupied, key, reason)
	if err != nil {
		// 流水幂等键冲突（同一动作重放）：直接返回冲突，由上层转为幂等响应
		if isUniqueViolationErr(err) {
			return ErrAlreadyApplied
		}
		return err
	}
	return nil
}

// ErrAlreadyApplied 该幂等键的流水已存在
var ErrAlreadyApplied = errors.New("ledger entry already applied")

// ---- Team -----------------------------------------------------------------

func (r *Repo) CreateTeam(ctx context.Context, name string, quotaBytes int64) (*Team, error) {
	var t Team
	err := r.DB.QueryRowContext(ctx,
		`INSERT INTO teams(name, quota_bytes) VALUES($1,$2)
		 RETURNING id,name,quota_bytes,occupied_bytes,reserved_bytes,created_at`,
		name, quotaBytes).
		Scan(&t.ID, &t.Name, &t.QuotaBytes, &t.OccupiedBytes, &t.ReservedBytes, &t.CreatedAt)
	if err != nil {
		if isUniqueViolationErr(err) {
			return nil, ErrNameTaken
		}
		return nil, err
	}
	return &t, nil
}

var ErrNameTaken = errors.New("team name already exists")

func (r *Repo) GetTeam(ctx context.Context, id int64) (*Team, error) {
	var t Team
	err := r.DB.QueryRowContext(ctx,
		`SELECT id,name,quota_bytes,occupied_bytes,reserved_bytes,created_at FROM teams WHERE id=$1`, id).
		Scan(&t.ID, &t.Name, &t.QuotaBytes, &t.OccupiedBytes, &t.ReservedBytes, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (r *Repo) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT id,name,quota_bytes,occupied_bytes,reserved_bytes,created_at FROM teams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.QuotaBytes, &t.OccupiedBytes, &t.ReservedBytes, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReclaimableBytes 保留期已过、等待删除释放的对象字节（看板第三栏）。
func (r *Repo) ReclaimableBytes(ctx context.Context, teamID int64) (int64, error) {
	var n sql.NullInt64
	err := r.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size_bytes),0) FROM objects
		WHERE team_id=$1 AND deleted_at IS NULL
		  AND retention_until IS NOT NULL AND retention_until < now()`, teamID).Scan(&n)
	return n.Int64, err
}

// ---- Session --------------------------------------------------------------

func scanSession(sc interface {
	Scan(dest ...any) error
}) (*Session, error) {
	var s Session
	var status string
	var finalSize, objectID sql.NullInt64
	var uploadID sql.NullString
	var completedAt, abortedAt sql.NullTime
	err := sc.Scan(&s.ID, &s.TeamID, &s.IdempotencyKey, &s.ObjectKey, &s.ContentType,
		&s.DeclareSize, &finalSize, &s.PartSize, &s.TotalParts, &status,
		&s.RetentionSeconds, &s.ExpiresAt, &uploadID, &completedAt, &abortedAt,
		&objectID, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.Status = status
	s.FinalSizeV = nullInt64Ptr(finalSize)
	s.UploadIDV = derefStr(uploadID)
	s.CompletedAtV = nullTimePtr(completedAt)
	s.AbortedAtV = nullTimePtr(abortedAt)
	s.ObjectIDV = nullInt64Ptr(objectID)
	return &s, nil
}

const sessionCols = `id, team_id, idempotency_key, object_key, content_type,
	declare_size, final_size, part_size, total_parts, status::text,
	retention_seconds, expires_at, upload_id, completed_at, aborted_at, object_id, created_at`

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if v.Valid {
		x := v.Int64
		return &x
	}
	return nil
}

func derefStr(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

func (r *Repo) GetSession(ctx context.Context, id int64) (*Session, error) {
	return scanSession(r.DB.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM upload_sessions WHERE id=$1`, id))
}

func (r *Repo) GetSessionByKey(ctx context.Context, key string) (*Session, error) {
	s, err := scanSession(r.DB.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM upload_sessions WHERE idempotency_key=$1`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func (r *Repo) ListSessions(ctx context.Context, teamID int64, limit int) ([]Session, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM upload_sessions WHERE team_id=$1 ORDER BY id DESC LIMIT $2`,
		teamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ---- Object ---------------------------------------------------------------

func scanObject(sc interface {
	Scan(dest ...any) error
}) (*Object, error) {
	var o Object
	var deleted sql.NullTime
	err := sc.Scan(&o.ID, &o.TeamID, &o.SessionID, &o.ObjectKey, &o.SizeBytes,
		&o.ContentType, &o.RetentionUntil, &deleted, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	o.DeletedAt = nullTimePtr(deleted)
	return &o, err
}

const objectCols = `id, team_id, session_id, object_key, size_bytes, content_type,
	retention_until, deleted_at, created_at`

func (r *Repo) GetObject(ctx context.Context, id int64) (*Object, error) {
	o, err := scanObject(r.DB.QueryRowContext(ctx,
		`SELECT `+objectCols+` FROM objects WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

func (r *Repo) ListObjects(ctx context.Context, teamID int64, includeDeleted bool, limit int) ([]Object, error) {
	q := `SELECT ` + objectCols + ` FROM objects WHERE team_id=$1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	q += ` ORDER BY id DESC LIMIT $2`
	rows, err := r.DB.QueryContext(ctx, q, teamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ---- Ledger ----------------------------------------------------------------

func (r *Repo) ListEntries(ctx context.Context, teamID int64, limit int) ([]Entry, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT id, team_id, session_id, object_id, entry_type::text,
		       delta_reserved, delta_occupied, reserved_after, occupied_after,
		       idempotency_key, COALESCE(reason,''), created_at
		FROM ledger_entries WHERE team_id=$1 ORDER BY id DESC LIMIT $2`, teamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var sid, oid sql.NullInt64
		if err := rows.Scan(&e.ID, &e.TeamID, &sid, &oid, &e.EntryType,
			&e.DeltaReserved, &e.DeltaOccupied, &e.ReservedAfter, &e.OccupiedAfter,
			&e.IdempotencyKey, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.SessionID = nullInt64Ptr(sid)
		e.ObjectID = nullInt64Ptr(oid)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Parts -----------------------------------------------------------------

func (r *Repo) RecordPart(ctx context.Context, sessionID int64, partNumber int, etag string, size int64) error {
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO session_parts(session_id, part_number, etag, size_bytes)
		VALUES($1,$2,$3,$4)
		ON CONFLICT (session_id, part_number) DO UPDATE SET etag=EXCLUDED.etag, size_bytes=EXCLUDED.size_bytes`,
		sessionID, partNumber, etag, size)
	return err
}

type Part struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	SizeBytes  int64  `json:"size_bytes"`
}

func (r *Repo) ListParts(ctx context.Context, sessionID int64) ([]Part, error) {
	rows, err := r.DB.QueryContext(ctx,
		`SELECT part_number, etag, size_bytes FROM session_parts WHERE session_id=$1 ORDER BY part_number`,
		sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.PartNumber, &p.ETag, &p.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Snapshot 返回任意时刻调试用的余额快照（依据流水重放，可校验 teams 表一致性）。
func (r *Repo) SnapshotFromLedger(ctx context.Context, teamID int64) (occupied, reserved int64, err error) {
	err = r.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(delta_occupied),0), COALESCE(SUM(delta_reserved),0)
		FROM ledger_entries WHERE team_id=$1`, teamID).Scan(&occupied, &reserved)
	return
}

