package ledger

import (
	"database/sql"
	"errors"
	"time"
)

var ErrQuotaExceeded = errors.New("quota exceeded")
var ErrConflict = errors.New("state conflict")

type Team struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	QuotaBytes    int64     `json:"quota_bytes"`
	OccupiedBytes int64     `json:"occupied_bytes"`
	ReservedBytes int64     `json:"reserved_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

type Session struct {
	ID               int64             `json:"id"`
	TeamID           int64             `json:"team_id"`
	IdempotencyKey   string            `json:"idempotency_key"`
	ObjectKey        string            `json:"object_key"`
	ContentType      string            `json:"content_type"`
	DeclareSize      int64             `json:"declare_size"`
	FinalSize        sql.NullInt64     `json:"-"`
	FinalSizeV       *int64            `json:"final_size,omitempty"`
	PartSize         int64             `json:"part_size"`
	TotalParts       int               `json:"total_parts"`
	Status           string            `json:"status"`
	RetentionSeconds int64             `json:"retention_seconds"`
	ExpiresAt        time.Time         `json:"expires_at"`
	UploadID         sql.NullString    `json:"-"`
	UploadIDV        string            `json:"upload_id,omitempty"`
	CompletedAt      sql.NullTime      `json:"-"`
	CompletedAtV     *time.Time        `json:"completed_at,omitempty"`
	AbortedAt        sql.NullTime      `json:"-"`
	AbortedAtV       *time.Time        `json:"aborted_at,omitempty"`
	ObjectID         sql.NullInt64     `json:"-"`
	ObjectIDV        *int64            `json:"object_id,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
}

type Object struct {
	ID             int64      `json:"id"`
	TeamID         int64      `json:"team_id"`
	SessionID      int64      `json:"session_id"`
	ObjectKey      string     `json:"object_key"`
	SizeBytes      int64      `json:"size_bytes"`
	ContentType    string     `json:"content_type"`
	RetentionUntil *time.Time `json:"retention_until,omitempty"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type Entry struct {
	ID             int64     `json:"id"`
	TeamID         int64     `json:"team_id"`
	SessionID      *int64    `json:"session_id,omitempty"`
	ObjectID       *int64    `json:"object_id,omitempty"`
	EntryType      string    `json:"entry_type"`
	DeltaReserved  int64     `json:"delta_reserved"`
	DeltaOccupied  int64     `json:"delta_occupied"`
	ReservedAfter  int64     `json:"reserved_after"`
	OccupiedAfter  int64     `json:"occupied_after"`
	IdempotencyKey string    `json:"idempotency_key"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}
