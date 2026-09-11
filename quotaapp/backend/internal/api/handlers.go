package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"quotaapp/internal/ledger"
	"quotaapp/internal/store"
)

type Handler struct {
	repo        *ledger.Repo
	st          *store.Store
	sweeper     SweepTrigger
	sessionTTL  time.Duration
	minPartSize int64
}

type SweepTrigger interface {
	Tick(ctx context.Context) SweepResult
}

type SweepResult struct {
	SessionsExpired  int   `json:"sessions_expired"`
	ObjectsReclaimed int   `json:"objects_reclaimed"`
	BytesReclaimed   int64 `json:"bytes_reclaimed"`
}

func New(repo *ledger.Repo, st *store.Store, sw SweepTrigger, sessionTTL time.Duration, minPartSize int64) *Handler {
	return &Handler{repo: repo, st: st, sweeper: sw, sessionTTL: sessionTTL, minPartSize: minPartSize}
}

func (h *Handler) Register(r gin.IRouter) {
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	api := r.Group("/api")
	{
		api.GET("/teams", h.listTeams)
		api.POST("/teams", h.createTeam)
		api.GET("/teams/:id", h.getTeam)
		api.GET("/teams/:id/sessions", h.listSessions)
		api.GET("/teams/:id/objects", h.listObjects)
		api.GET("/teams/:id/ledger", h.listLedger)

		api.POST("/teams/:id/uploads", idempotency, h.reserve)
		api.GET("/uploads/:sid", h.getSession)
		api.POST("/uploads/:sid/parts/:part", idempotency, h.signPart)
		api.POST("/uploads/:sid/parts/:part/etag", h.reportETag) // 可选：浏览器直传后登记分片
		api.POST("/uploads/:sid/complete", idempotency, h.complete)
		api.POST("/uploads/:sid/abort", idempotency, h.abort)

		api.DELETE("/objects/:oid", idempotency, h.deleteObject)
		api.GET("/objects/:oid/download", h.downloadObject)

		api.POST("/admin/sweep", h.sweep)
	}
}

// idempotency 中间件：所有写操作必须带 Idempotency-Key，
// 浏览器重试/用户连点/回调乱序重放都用同一把去重钥匙。
func idempotency(c *gin.Context) {
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		key = c.Query("idempotency_key")
	}
	if key == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "缺少 Idempotency-Key 请求头"})
		return
	}
	if len(key) > 200 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key 过长"})
		return
	}
	c.Set("idem", key)
	c.Next()
}

func idemKey(c *gin.Context) string { return c.GetString("idem") }

func fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ledger.ErrQuotaExceeded):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "quota_exceeded"})
	case errors.Is(err, ledger.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "state_conflict"})
	case errors.Is(err, ledger.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "资源不存在", "code": "not_found"})
	case errors.Is(err, ledger.ErrNameTaken):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "name_taken"})
	default:
		var re *ledger.RetentionError
		if errors.As(err, &re) {
			c.JSON(http.StatusLocked, gin.H{"error": err.Error(), "code": "retention_locked", "retry_after": re.Until})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// ---- teams ----------------------------------------------------------------

func (h *Handler) listTeams(c *gin.Context) {
	teams, err := h.repo.ListTeams(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"teams": teams})
}

type createTeamReq struct {
	Name       string `json:"name" binding:"required"`
	QuotaBytes int64  `json:"quota_bytes" binding:"required"`
}

func (h *Handler) createTeam(c *gin.Context) {
	var req createTeamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.QuotaBytes < 0 {
		c.JSON(400, gin.H{"error": "配额不能为负"})
		return
	}
	t, err := h.repo.CreateTeam(c.Request.Context(), strings.TrimSpace(req.Name), req.QuotaBytes)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(201, t)
}

type teamDetail struct {
	*ledger.Team
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	AvailableBytes   int64 `json:"available_bytes"`
}

func (h *Handler) getTeam(c *gin.Context) {
	id, ok := int64Param(c, "id")
	if !ok {
		return
	}
	t, err := h.repo.GetTeam(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	reclaim, _ := h.repo.ReclaimableBytes(c.Request.Context(), id)
	c.JSON(200, teamDetail{
		Team:             t,
		ReclaimableBytes: reclaim,
		AvailableBytes:   t.QuotaBytes - t.OccupiedBytes - t.ReservedBytes,
	})
}

func (h *Handler) listSessions(c *gin.Context) {
	id, ok := int64Param(c, "id")
	if !ok {
		return
	}
	ss, err := h.repo.ListSessions(c.Request.Context(), id, 200)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"sessions": ss})
}

func (h *Handler) listObjects(c *gin.Context) {
	id, ok := int64Param(c, "id")
	if !ok {
		return
	}
	includeDeleted := c.Query("include_deleted") == "1"
	objs, err := h.repo.ListObjects(c.Request.Context(), id, includeDeleted, 200)
	if err != nil {
		fail(c, err)
		return
	}
	now := time.Now()
	type objView struct {
		ledger.Object
		Reclaimable bool `json:"reclaimable"`
	}
	out := make([]objView, 0, len(objs))
	for _, o := range objs {
		rc := o.DeletedAt != nil || (o.RetentionUntil != nil && now.After(*o.RetentionUntil))
		out = append(out, objView{Object: o, Reclaimable: rc})
	}
	c.JSON(200, gin.H{"objects": out})
}

func (h *Handler) listLedger(c *gin.Context) {
	id, ok := int64Param(c, "id")
	if !ok {
		return
	}
	es, err := h.repo.ListEntries(c.Request.Context(), id, 300)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"entries": es})
}

// ---- upload lifecycle -----------------------------------------------------

type reserveReq struct {
	ObjectKey        string `json:"object_key" binding:"required"`
	ContentType      string `json:"content_type"`
	DeclareSize      int64  `json:"declare_size" binding:"required"`
	PartSize         int64  `json:"part_size"`
	RetentionSeconds int    `json:"retention_seconds"`
	TTLSeconds       int    `json:"ttl_seconds"` // 可选，演示用；会被钳制
}

func (h *Handler) reserve(c *gin.Context) {
	teamID, ok := int64Param(c, "id")
	if !ok {
		return
	}
	var req reserveReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.DeclareSize <= 0 {
		c.JSON(400, gin.H{"error": "declare_size 必须为正数"})
		return
	}
	if _, err := store.SafeKey(teamID, req.ObjectKey); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	ttl := h.sessionTTL
	if req.TTLSeconds > 0 {
		ttl = clampTTL(req.TTLSeconds)
	}
	partSize := req.PartSize
	if partSize <= 0 {
		partSize = h.minPartSize
	}
	totalParts := int((req.DeclareSize + partSize - 1) / partSize)
	if totalParts == 0 {
		totalParts = 1
	}

	s, err := h.repo.Reserve(c.Request.Context(), teamID, idemKey(c), req.ObjectKey,
		req.ContentType, req.DeclareSize, partSize, totalParts, req.RetentionSeconds, ttl)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(201, gin.H{"session": s, "part_size": partSize, "total_parts": totalParts})
}

func (h *Handler) getSession(c *gin.Context) {
	sid, ok := int64Param(c, "sid")
	if !ok {
		return
	}
	s, err := h.repo.GetSession(c.Request.Context(), sid)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"session": s})
}

type signPartResp struct {
	SessionID  int64  `json:"session_id"`
	PartNumber int    `json:"part_number"`
	UploadID   string `json:"upload_id"`
	URL        string `json:"url"`
	ExpiresIn  int    `json:"expires_in"`
}

// loadActiveSession 取会话并确保 MinIO multipart 已创建（惰性创建：
// 预留先于存储，避免存储侧存在没有额度背书的分片）。
func (h *Handler) loadActiveSession(c *gin.Context, sid int64) (*ledger.Session, string, bool) {
	s, err := h.repo.GetSession(c.Request.Context(), sid)
	if err != nil {
		fail(c, err)
		return nil, "", false
	}
	if !isActive(s.Status) {
		c.JSON(http.StatusConflict, gin.H{"error": "会话已终态: " + s.Status, "code": "state_conflict"})
		return nil, "", false
	}
	if time.Now().After(s.ExpiresAt) {
		c.JSON(http.StatusConflict, gin.H{"error": "会话已过 TTL", "code": "session_expired"})
		return nil, "", false
	}
	fullKey, err := store.SafeKey(s.TeamID, s.ObjectKey)
	if err != nil {
		fail(c, err)
		return nil, "", false
	}
	if s.UploadIDV == "" {
		fk, uid, err := h.st.BeginMultipart(c.Request.Context(), s.TeamID, s.ObjectKey, s.ContentType)
		if err != nil {
			// 存储创建失败必须立即释放预留，不留挂账
			_, _, _ = h.repo.Release(c.Request.Context(), sid, "MinIO CreateMultipartUpload 失败: "+err.Error())
			fail(c, err)
			return nil, "", false
		}
		fullKey = fk
		if err := h.repo.AttachUploadID(c.Request.Context(), sid, uid); err != nil {
			_ = h.st.AbortMultipart(c.Request.Context(), fk, uid)
			fail(c, err)
			return nil, "", false
		}
		s.UploadIDV = uid
	}
	h.repo.MarkUploading(c.Request.Context(), sid)
	return s, fullKey, true
}

func (h *Handler) signPart(c *gin.Context) {
	sid, ok := int64Param(c, "sid")
	if !ok {
		return
	}
	part, ok := intParam(c, "part")
	if !ok {
		return
	}
	s, fullKey, ok := h.loadActiveSession(c, sid)
	if !ok {
		return
	}
	if part < 1 || part > s.TotalParts {
		c.JSON(400, gin.H{"error": fmt.Sprintf("分片号须在 1..%d", s.TotalParts)})
		return
	}
	urlStr, err := h.st.PresignPutPart(c.Request.Context(), fullKey, s.UploadIDV, part)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, signPartResp{SessionID: sid, PartNumber: part, UploadID: s.UploadIDV, URL: urlStr, ExpiresIn: 900})
}

type etagReq struct {
	ETag      string `json:"etag" binding:"required"`
	SizeBytes int64  `json:"size_bytes"`
}

func (h *Handler) reportETag(c *gin.Context) {
	sid, ok := int64Param(c, "sid")
	if !ok {
		return
	}
	part, ok := intParam(c, "part")
	if !ok {
		return
	}
	var req etagReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if err := h.repo.RecordPart(c.Request.Context(), sid, part, req.ETag, req.SizeBytes); err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

type completeReq struct {
	Parts []partInput `json:"parts"`
}
type partInput struct {
	PartNumber int    `json:"part_number" binding:"required"`
	ETag       string `json:"etag" binding:"required"`
}

func (h *Handler) complete(c *gin.Context) {
	sid, ok := int64Param(c, "sid")
	if !ok {
		return
	}
	var req completeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(req.Parts) == 0 {
		c.JSON(400, gin.H{"error": "parts 不能为空"})
		return
	}

	s, err := h.repo.GetSession(c.Request.Context(), sid)
	if err != nil {
		fail(c, err)
		return
	}
	// 已完成的回调乱序重放：幂等返回原结果
	if s.Status == "completed" {
		c.JSON(200, gin.H{"session": s, "object_id": s.ObjectIDV, "idempotent_replay": true})
		return
	}
	if !isActive(s.Status) {
		fail(c, ledger.ErrConflict)
		return
	}
	if s.UploadIDV == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "尚未领取任何分片，无法完成", "code": "no_parts"})
		return
	}
	fullKey, err := store.SafeKey(s.TeamID, s.ObjectKey)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	parts := make([]store.CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, store.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag})
	}

	key := idemKey(c)
	// 在账本事务内完成 MinIO：回调失败则账本回滚，绝不出现“记了占用但对象不存在”。
	done, err := h.repo.Commit(c.Request.Context(), sid, key, s.DeclareSize, func(tx *sql.Tx, finalSize *int64) error {
		size, cerr := h.st.CompleteMultipart(c.Request.Context(), fullKey, s.UploadIDV, parts)
		if cerr != nil {
			return cerr
		}
		*finalSize = size // 以 MinIO HeadObject 实测字节为准
		return nil
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"session": done, "object_id": done.ObjectIDV, "final_size": done.FinalSizeV})
}

func (h *Handler) abort(c *gin.Context) {
	sid, ok := int64Param(c, "sid")
	if !ok {
		return
	}
	s, err := h.repo.GetSession(c.Request.Context(), sid)
	if err != nil {
		fail(c, err)
		return
	}
	// 先释放账本（幂等，只有活跃态生效），再补偿中止 MinIO
	updated, changed, err := h.repo.Release(c.Request.Context(), sid, "用户取消上传")
	if err != nil {
		fail(c, err)
		return
	}
	if s.UploadIDV != "" {
		fullKey, _ := store.SafeKey(s.TeamID, s.ObjectKey)
		if abortErr := h.st.AbortMultipart(c.Request.Context(), fullKey, s.UploadIDV); abortErr != nil && changed {
			// MinIO 侧有 bucket 生命周期兜底，账本不回滚；记录给客户端
			c.JSON(200, gin.H{"session": updated, "released": changed,
				"idempotent_replay": false, "minio_warning": abortErr.Error()})
			return
		}
	}
	c.JSON(200, gin.H{"session": updated, "released": changed, "idempotent_replay": !changed})
}

// ---- objects ---------------------------------------------------------------

func (h *Handler) deleteObject(c *gin.Context) {
	oid, ok := int64Param(c, "oid")
	if !ok {
		return
	}
	// 必须带团队归属，防止跨团队删除
	teamID, ok := int64Query(c, "team_id")
	if !ok {
		c.JSON(400, gin.H{"error": "缺少 team_id 查询参数"})
		return
	}
	o, err := h.repo.DeleteObject(c.Request.Context(), teamID, oid, idemKey(c), false)
	if err != nil {
		fail(c, err)
		return
	}
	fullKey, err := store.SafeKey(o.TeamID, o.ObjectKey)
	if err != nil {
		fail(c, err)
		return
	}
	if err := h.st.DeleteObject(c.Request.Context(), fullKey); err != nil {
		// 账本已扣减但存储删除失败：sweeper/生命周期会继续兜底，明确告知
		c.JSON(200, gin.H{"object": o, "minio_warning": err.Error()})
		return
	}
	c.JSON(200, gin.H{"object": o, "deleted": true})
}

func (h *Handler) downloadObject(c *gin.Context) {
	oid, ok := int64Param(c, "oid")
	if !ok {
		return
	}
	o, err := h.repo.GetObject(c.Request.Context(), oid)
	if err != nil {
		fail(c, err)
		return
	}
	if o.DeletedAt != nil {
		c.JSON(404, gin.H{"error": "对象已删除"})
		return
	}
	fullKey, err := store.SafeKey(o.TeamID, o.ObjectKey)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	urlStr, err := h.st.PresignGetObject(c.Request.Context(), fullKey)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(200, gin.H{"url": urlStr, "expires_in": 900, "object": o})
}

func (h *Handler) sweep(c *gin.Context) {
	res := h.sweeper.Tick(c.Request.Context())
	c.JSON(200, res)
}

// ---- helpers ---------------------------------------------------------------

func isActive(st string) bool {
	return st == "reserved" || st == "initiated" || st == "uploading"
}

func clampTTL(secs int) time.Duration {
	if secs < 5 {
		secs = 5
	}
	if secs > 3600 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}
