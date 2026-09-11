# 对象存储配额管理（按团队计费）

区分 **占用（occupied）/ 预留（reserved）/ 可回收（reclaimable）** 三类空间的对象存储管理应用：

| 层 | 技术 | 职责 |
|---|---|---|
| 展示 | **Next.js 14** (App Router) | 团队用量、上传会话、配额流水；浏览器分片直传 |
| 配额服务 | **Go + Gin** | 预留/提交/释放/删除的全部记账与校验，签发预签 URL |
| 账本 | **PostgreSQL 15** | 只增流水 `ledger_entries` + 余额表 `teams`，每条变化可追溯 |
| 存储 | **本地 MinIO**（S3 兼容） | 分片上传（multipart）落盘，凭据**永不下发浏览器** |

## 用量模型与不变量

```
occupied + reserved ≤ quota_bytes        （强制，数据库行锁串行化）
teams 余额 ≡ SUM(ledger_entries 对应 delta) （流水重放恒等）
```

- **预留 reserved**：大文件分片上传前，客户端按声明大小 `declare_size` 申请。
- **转占用 occupied**：全部分片完成后，在**同一个账本事务**内调用 MinIO CompleteMultipartUpload，
  以 `HeadObject` 实测字节落账（释放 declare、占用实际值）。
- **释放**：用户取消、上传失败补偿、会话 TTL 到期（sweeper）三条路径，全部走同一个幂等释放。
- **可回收 reclaimable**：对象保留期（retention）已过、等待 sweeper 物理删除的字节，看板单独展示。

## 关键正确性设计

1. **并发抢最后空间**：`Reserve` 在事务内 `SELECT ... FOR UPDATE` 锁住团队行，两个并发申请被串行化，
   恰好一个成功、另一个收到 `409 quota_exceeded`，绝不可能超卖。
2. **重试不多记**：每个写请求必须带 `Idempotency-Key`；
   会话以幂等键唯一约束去重，流水以 `key:release` / `key:commit` 去重。
   同键重试**优先**命中原会话（即使自己的预留已把额度占满，也不会误报超额）。
3. **回调乱序不多记**：`completed/aborted/expired` 均为终态：
   - 完成后迟到的 complete → 返回原结果（`idempotent_replay: true`）
   - 完成后迟到的 abort → `released: false`，不动账
   - abort 后又 abort → 不产生第二条释放流水
4. **失败不留挂账**：MinIO CompleteMultipartUpload 在持锁事务内执行，失败则账本整体回滚；
   CreateMultipartUpload 失败会立即释放预留；TTL 到期释放账本后补偿 AbortMultipartUpload，
   bucket 生命周期（1 天中止未完成 MPU）再兜底。
5. **保留期后端强制**：删除唯一入口 `DELETE /api/objects/:id?team_id=` 走
   `ledger.DeleteObject`，未到 `retention_until` 一律 `423 retention_locked`；
   管理界面只是把按钮禁用，绕过界面直调 API 同样被拒。sweeper 到期删除复用同一个函数。
6. **凭据隔离**：浏览器只拿到 15 分钟有效的预签 PUT/GET URL（S3 SigV4 query 签名），
   前端产物中不存在任何 MinIO access/secret。
7. **团队隔离 / 路径安全**：对象实际 key 为 `teams/<team_id>/<user-key>`，
   删除校验对象归属（跨团队删除返回 404），key 拒绝 `..`、前导 `/`、反斜杠与控制字符。

## 目录

```
quotaapp/
├── backend/
│   ├── cmd/server/main.go          # 装配：迁移、MinIO、sweeper、Gin
│   ├── internal/
│   │   ├── db/                     # PG 连接 + 嵌入式迁移
│   │   ├── ledger/                 # 配额账本核心（repo.go 事务状态机）
│   │   ├── store/                  # MinIO/S3：multipart、预签 URL、key 安全
│   │   ├── api/                    # Gin handlers（Idempotency-Key 中间件）
│   │   └── sweeper/                # TTL 释放 + 到期回收
│   └── migrations/0001_init.sql
├── web/                            # Next.js 管理界面
│   ├── app/page.jsx                # 团队列表 + 四色用量
│   ├── app/teams/[id]/page.jsx     # 对象 / 上传会话 / 账本流水
│   ├── components/UploadPanel.jsx  # 浏览器分片直传
│   └── lib/api.js                  # 预签直传 + 稳定幂等键
├── scripts/scenario_test.py        # 6 组场景 / 40 项断言
└── run.sh                          # 一键拉起 PG + MinIO + Go + Next
```

## 快速开始

```bash
./run.sh                              # 启动全部组件
python3 scripts/scenario_test.py      # 复现全部场景（约 30s）
```

- 管理界面：http://127.0.0.1:3000 （建一个 1 MiB 的团队即可手动复现抢占）
- API：http://127.0.0.1:8080 ，所有写接口需要 `Idempotency-Key` 头
- 演示参数：会话 TTL 30s，sweeper 每 2s 一轮；上传时可把对象保留期选为「8 秒」观察到期回收。

## 场景测试覆盖（scripts/scenario_test.py）

1. **并发抢配额**：1 MiB 团队，两个线程各申请 700 KiB → 一成一败；同键重试不重复预留。
2. **上传失败/取消**：只领分片不直传就 complete → 存储失败、账本无 commit；abort 释放；重复 abort 幂等。
3. **回调乱序**：complete 重放、迟到 abort → 占用/预留与流水条数完全不变。
4. **保留期**：未到期删除 423；短保留对象被 sweeper 回收，长保留对象仍锁定；回收动作可追到会话与对象。
5. **会话 TTL**：只预留不上传 → 到期 expired、预留归零、MinIO multipart 被补偿中止，且只有一条释放流水。
6. **端到端追溯**：每笔余额变化都有 `session_id`（及 `object_id`），流水求和恒等于余额表。

## API 摘要

```
POST   /api/teams                         建团队 {name, quota_bytes}
GET    /api/teams/:id                     含 occupied/reserved/reclaimable/available
POST   /api/teams/:id/uploads             预留（Idempotency-Key）
POST   /api/uploads/:sid/parts/:part      取分片预签 PUT URL（惰性创建 MPU）
POST   /api/uploads/:sid/complete         完成 → 事务内转占用（Idempotency-Key）
POST   /api/uploads/:sid/abort            取消/失败释放（Idempotency-Key）
DELETE /api/objects/:oid?team_id=         删除（保留期后端校验）
GET    /api/objects/:oid/download         取预签 GET URL
GET    /api/teams/:id/ledger              配额流水（审计/追溯）
POST   /api/admin/sweep                   手动触发一轮清扫（演示用）
```

> 说明：本项目为本地演示，数据库与 MinIO 使用 trust/默认口令；生产化需替换为
> 强制鉴权（团队/用户 token）、TLS、非默认凭据、分片大小下限与更严格的 TTL 范围。
