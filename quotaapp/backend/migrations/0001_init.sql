-- 配额账本：所有余额变化都以不可变流水表达
-- 用量模型：
--   occupied  已落盘对象的真实字节
--   reserved  上传会话预占的字节
--   reclaimable 已过保留期、等待清理释放的对象字节（仅用于看板区分，不再计入占用）
-- 约束：occupied + reserved <= quota_bytes

CREATE TABLE teams (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    quota_bytes     BIGINT NOT NULL CHECK (quota_bytes >= 0),
    occupied_bytes  BIGINT NOT NULL DEFAULT 0 CHECK (occupied_bytes >= 0),
    reserved_bytes  BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE upload_status AS ENUM ('reserved', 'initiated', 'uploading', 'completed', 'aborted', 'expired');
-- reserved:  仅在账本预占，MinIO multipart 尚未创建
-- initiated: MinIO multipart 已创建
-- uploading: 至少一个分片预签URL已发放（纯展示状态，不影响记账）
-- completed: 全部完成，预留已转占用
-- aborted:   用户取消，预留已释放
-- expired:   TTL 到期被清扫器释放

CREATE TABLE upload_sessions (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id           BIGINT NOT NULL REFERENCES teams(id),
    idempotency_key   TEXT NOT NULL UNIQUE,           -- 客户端去重键，重试/回调乱序不会多记
    object_key        TEXT NOT NULL,
    content_type      TEXT NOT NULL DEFAULT 'application/octet-stream',
    declare_size      BIGINT NOT NULL CHECK (declare_size >= 0), -- 申请预留的字节
    final_size        BIGINT CHECK (final_size >= 0),            -- 完成时以 MinIO 实测为准
    part_size         BIGINT NOT NULL DEFAULT 0,
    total_parts       INT  NOT NULL DEFAULT 0,
    status            upload_status NOT NULL DEFAULT 'reserved',
    retention_seconds BIGINT NOT NULL DEFAULT 0,      -- 对象保留期（0=不过期）
    expires_at        TIMESTAMPTZ NOT NULL,           -- 会话/预留 TTL
    upload_id         TEXT,                           -- MinIO multipart upload id
    completed_at      TIMESTAMPTZ,
    aborted_at        TIMESTAMPTZ,
    object_id         BIGINT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sessions_team ON upload_sessions(team_id, created_at DESC);
CREATE INDEX idx_sessions_expiry ON upload_sessions(expires_at) WHERE status IN ('reserved', 'initiated', 'uploading');

CREATE TABLE objects (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id         BIGINT NOT NULL REFERENCES teams(id),
    session_id      BIGINT NOT NULL REFERENCES upload_sessions(id),
    object_key      TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type    TEXT NOT NULL,
    retention_until TIMESTAMPTZ,                    -- 保留截止时间；NULL = 永久保留
    deleted_at      TIMESTAMPTZ,                    -- 软删除（清扫器真正删 MinIO 后置位）
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 同一 key 同时只允许一个存活对象
CREATE UNIQUE INDEX uq_objects_live_key ON objects(team_id, object_key) WHERE deleted_at IS NULL;

CREATE TYPE ledger_entry_type AS ENUM ('reserve', 'commit', 'release', 'delete', 'reclaim');

CREATE TABLE ledger_entries (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id       BIGINT NOT NULL REFERENCES teams(id),
    session_id    BIGINT REFERENCES upload_sessions(id),
    object_id     BIGINT REFERENCES objects(id),
    entry_type    ledger_entry_type NOT NULL,
    -- reserve: reserved +delta / commit: reserved -declare, occupied +final / release: reserved -delta
    -- delete:  occupied -size / reclaim: occupied -size（保留到期进入可回收视角）
    delta_reserved BIGINT NOT NULL,
    delta_occupied BIGINT NOT NULL,
    reserved_after BIGINT NOT NULL,
    occupied_after BIGINT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    reason         TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_ledger_team_time ON ledger_entries(team_id, created_at DESC);
CREATE INDEX idx_ledger_session ON ledger_entries(session_id);
CREATE INDEX idx_ledger_object ON ledger_entries(object_id);

-- 单条 MinIO 分片登记（完成时回传做校验，也是分片级审计线索）
CREATE TABLE session_parts (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id   BIGINT NOT NULL REFERENCES upload_sessions(id) ON DELETE CASCADE,
    part_number  INT NOT NULL,
    etag         TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, part_number)
);
