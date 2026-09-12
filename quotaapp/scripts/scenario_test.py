#!/usr/bin/env python3
"""
配额服务场景复现脚本（仅标准库）。

用很小的配额复现：
  1. 两个并发上传抢最后一点空间 —— 恰好一个成功、一个 quota_exceeded
  2. 上传失败/取消 —— 预留释放；失败的完成回调不会把占用记进去
  3. 回调乱序 —— 已完成后重复 complete / abort，绝不重复入账
  4. 保留期 —— 未到期删除被后端拒绝（HTTP 423），到期后 sweeper 自动回收
  5. 会话 TTL —— 预留超时自动释放，MinIO multipart 被补偿中止
  6. 全程可追溯 —— 每次用量变化都能在 ledger 追到 session/object

用法: python3 scenario_test.py [api_base]
环境需要：Go 服务已启动（SESSION_TTL_SECONDS 建议 30, SWEEP_INTERVAL_MS=3000）。
"""
import json
import os
import sys
import time
import threading
import urllib.request
import urllib.error
import uuid

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"
PASS, FAIL = 0, 0
TEAM_ID = None

# 每次运行一个唯一前缀：测试要验证“同键重试返回同一会话”，所以仍需固定键，
# 但不能与历史运行残留的会话撞键（idempotency_key 全局唯一是正确的服务端行为）。
RUN_ID = uuid.uuid4().hex[:12]


def K(name):
    return f"{RUN_ID}:{name}"


def call(method, path, body=None, idem=None, raw=False):
    url = path if path.startswith("http") else BASE + path
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if idem is not None:
        headers["Idempotency-Key"] = idem
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req) as r:
            payload = r.read()
            return r.status, (payload if raw else json.loads(payload or b"{}"))
    except urllib.error.HTTPError as e:
        payload = e.read()
        try:
            return e.code, json.loads(payload)
        except Exception:
            return e.code, payload


def put_bytes(url, data: bytes) -> str:
    """模拟浏览器直传 MinIO 预签 URL，返回 ETag。凭据从未出现在此调用里。"""
    req = urllib.request.Request(url, data=data, method="PUT")
    with urllib.request.urlopen(req) as r:
        return r.headers["ETag"]


def check(name, cond, detail=""):
    global PASS, FAIL
    if cond:
        PASS += 1
        print(f"  PASS  {name}")
    else:
        FAIL += 1
        print(f"  FAIL  {name}  {detail}")


def team():
    return call("GET", f"/api/teams/{TEAM_ID}")[1]


def ledger():
    return call("GET", f"/api/teams/{TEAM_ID}/ledger")[1]["entries"]


def new_team(name, quota):
    st, t = call("POST", "/api/teams", {"name": name, "quota_bytes": uuid.uuid4().hex[:8] + "-" + name} if False else
                 {"name": name + "-" + uuid.uuid4().hex[:8], "quota_bytes": quota})
    assert st == 201, t
    return t["id"]


def reserve(team_id, size, key=None, retention=0, ttl=None):
    body = {"object_key": f"obj-{uuid.uuid4().hex[:8]}.bin", "declare_size": size,
            "retention_seconds": retention}
    if ttl:
        body["ttl_seconds"] = ttl
    return call("POST", f"/api/teams/{team_id}/uploads", body, idem=key or uuid.uuid4().hex)


def upload_full(team_id, size, key=None, retention=0, data_byte=0x5a, ttl=600):
    """预留 -> 预签 -> 直传 -> 完成 的完整单分片上传。

    ttl 默认 600s：场景只关心完成/乱序语义，不应在高负载（如并行跑无头浏览器）
    时被 30s 默认 TTL 抢先回收；专门验证 TTL 的场景会自行传短 ttl。
    """
    st, r = reserve(team_id, size, key=key, retention=retention, ttl=ttl)
    assert st == 201, r
    sid = r["session"]["id"]
    st, sig = call("POST", f"/api/uploads/{sid}/parts/1", idem=uuid.uuid4().hex)
    assert st == 200, sig
    data = bytes([data_byte]) * size
    etag = put_bytes(sig["url"], data)
    st, done = call("POST", f"/api/uploads/{sid}/complete",
                    {"parts": [{"part_number": 1, "etag": etag}]}, idem=key or uuid.uuid4().hex)
    return st, done, sid


def scenario_concurrent():
    global TEAM_ID
    print("\n[1] 两个并发上传抢最后空间（配额 1 MiB，各申请 700 KiB）")
    TEAM_ID = new_team("race", 1024 * 1024)
    results = {}

    def worker(tag):
        st, r = reserve(TEAM_ID, 700 * 1024, key=f"race-{tag}-{uuid.uuid4().hex}")
        results[tag] = (st, r)

    t1, t2 = threading.Thread(target=worker, args=("A",)), threading.Thread(target=worker, args=("B",))
    t1.start(); t2.start(); t1.join(); t2.join()
    codes = sorted(results[t][0] for t in results)
    check("恰好一个 201、一个 409", codes == [201, 409], str(codes))
    loser = [t for t in results if results[t][0] == 409][0]
    check("失败者返回 quota_exceeded", results[loser][1].get("code") == "quota_exceeded",
          str(results[loser][1]))
    t = team()
    check("预留合计 700KiB（绝无超额）", t["reserved_bytes"] == 700 * 1024, str(t))
    check("余额自洽：占用+预留+可用=配额",
          t["occupied_bytes"] + t["reserved_bytes"] + t["available_bytes"] == t["quota_bytes"])
    # 失败请求同键重试：已有会话存在且仍在活跃期，返回同一个预留而非新扣一次
    winner = [t_ for t_ in results if results[t_][0] == 201][0]
    st0, r0 = results[winner]
    st1, r1 = call("POST", f"/api/teams/{TEAM_ID}/uploads",
                   {"object_key": "x", "declare_size": 700 * 1024},
                   idem=r0["session"]["idempotency_key"])
    check("同键重试不重复预留", st1 == 201 and r1["session"]["id"] == r0["session"]["id"])
    check("重试后预留仍是 700KiB", team()["reserved_bytes"] == 700 * 1024)


def scenario_failure_abort():
    print("\n[2] 上传失败与取消：失败的 complete 不入账，abort 释放预留")
    tid = new_team("fail", 1024 * 1024)
    # 显式长 TTL，避免演示环境 30s 默认 TTL 在断言前正常回收
    st, r = reserve(tid, 400 * 1024, key=K("fail-session-1"), ttl=600)
    sid = r["session"]["id"]
    check("预留 400KiB 成功", st == 201)
    # 领取分片 URL（触发 MinIO multipart 创建），但不直传任何字节，直接 complete -> 存储失败
    st, sig = call("POST", f"/api/uploads/{sid}/parts/1", idem=K("fail-sign-1"))
    st, done = call("POST", f"/api/uploads/{sid}/complete",
                    {"parts": [{"part_number": 1, "etag": '"deadbeef"'}]}, idem=K("fail-complete-1"))
    check("存储侧失败 complete 返回非 2xx", st >= 400, f"status={st} {done}")
    t = call("GET", f"/api/teams/{tid}")[1]
    check("失败回调没有记任何占用", t["occupied_bytes"] == 0, str(t))
    check("预留仍挂 400KiB（等待 abort/TTL）", t["reserved_bytes"] == 400 * 1024, str(t))
    es = call("GET", f"/api/teams/{tid}/ledger")[1]["entries"]
    check("账本只有一条 reserve，无 commit", [e["entry_type"] for e in es] == ["reserve"])
    # 用户取消
    st, ab = call("POST", f"/api/uploads/{sid}/abort", idem=K("fail-abort-1"))
    check("abort 释放成功 released=true", st == 200 and ab.get("released") is True, str(ab))
    t = call("GET", f"/api/teams/{tid}")[1]
    check("预留归零、余额恢复", t["reserved_bytes"] == 0 and t["available_bytes"] == t["quota_bytes"])
    # 乱序：abort 后又来一个 abort（或先 complete 后 abort）——不重复释放
    st, ab2 = call("POST", f"/api/uploads/{sid}/abort", idem=K("fail-abort-dup"))
    check("重复 abort 幂等 released=false", ab2.get("released") is False, str(ab2))
    check("余额保持不变", call("GET", f"/api/teams/{tid}")[1]["reserved_bytes"] == 0)


def scenario_out_of_order():
    print("\n[3] 回调乱序：成功完成后 complete/abort 重放都不多记一次")
    tid = new_team("order", 1024 * 1024)
    key = K("order-up-1")
    st, done, sid = upload_full(tid, 300 * 1024, key=key)
    oid = done["object_id"]
    check("首次完成 200", st == 200, str(done))
    # 同键 complete 重放
    st2, done2 = call("POST", f"/api/uploads/{sid}/complete",
                      {"parts": [{"part_number": 1, "etag": "x"}]}, idem=key)
    check("complete 重放幂等", st2 == 200 and done2.get("idempotent_replay"), str(done2))
    # 乱序 abort
    st3, ab = call("POST", f"/api/uploads/{sid}/abort", idem=K("order-abort-late"))
    check("完成后迟到的 abort 不释放", ab.get("released") is False, str(ab))
    t = call("GET", f"/api/teams/{tid}")[1]
    check("占用仍是 300KiB、预留 0", t["occupied_bytes"] == 300 * 1024 and t["reserved_bytes"] == 0)
    es = call("GET", f"/api/teams/{tid}/ledger")[1]["entries"]
    types_ = [e["entry_type"] for e in es]
    check("流水恰为 reserve/release/commit 各一条",
          sorted(types_) == ["commit", "release", "reserve"], str(types_))
    # 所有条目都能追到 session，commit 还能追到 object
    check("流水全部关联到上传会话", all(e["session_id"] == sid for e in es))
    check("commit 条目关联到对象", next(e for e in es if e["entry_type"] == "commit")["object_id"] == oid)


def scenario_retention():
    print("\n[4] 保留期：未到期后端拒绝删除（管理界面/API 同一道校验），到期自动回收")
    tid = new_team("ret", 4 * 1024 * 1024)
    st, done, sid = upload_full(tid, 256 * 1024, key=K("ret-obj-long"), retention=3600)
    oid = done["object_id"]
    check("带保留期对象上传成功", st == 200, str(done))
    st, d = call("DELETE", f"/api/objects/{oid}?team_id={tid}", idem=K("ret-del-1"))
    check("未到期删除被拒绝 423", st == 423, f"status={st} {d}")
    check("错误码 retention_locked", isinstance(d, dict) and d.get("code") == "retention_locked", str(d))
    t = call("GET", f"/api/teams/{tid}")[1]
    check("拒绝删除后占用不变", t["occupied_bytes"] == 256 * 1024)
    # 短保留期对象：2s 后应被 sweeper 回收
    st2, done2, sid2 = upload_full(tid, 128 * 1024, key=K("ret-obj-short"), retention=2, data_byte=0x33)
    oid2 = done2["object_id"]
    check("短保留期对象上传成功", st2 == 200)
    print("    等待 sweeper（约 8s）...")
    reclaimed = False
    for _ in range(12):
        time.sleep(1)
        st, r = call("POST", "/api/admin/sweep")
        o = call("GET", f"/api/teams/{tid}/objects?include_deleted=1")[1]["objects"]
        target = next((x for x in o if x["id"] == oid2), None)
        if target and target.get("deleted_at"):
            reclaimed = True
            break
    check("到期对象被自动软删除", reclaimed)
    t = call("GET", f"/api/teams/{tid}")[1]
    check("回收后占用只剩 256KiB 长保留对象", t["occupied_bytes"] == 256 * 1024, str(t))
    objs = call("GET", f"/api/teams/{tid}/objects")[1]["objects"]
    check("存活对象列表里已无到期对象", all(x["id"] != oid2 for x in objs))
    # 长保留对象仍受保护
    st, d = call("DELETE", f"/api/objects/{oid}?team_id={tid}", idem=K("ret-del-2"))
    check("长保留对象仍不可删", st == 423)
    # 可追溯：回收动作为 ledger 中的 delete 条目，且关联会话与对象
    es = call("GET", f"/api/teams/{tid}/ledger")[1]["entries"]
    rec = [e for e in es if e.get("object_id") == oid2 and e["entry_type"] == "delete"]
    check("回收动作入流水且可追到会话/对象", len(rec) == 1 and rec[0]["session_id"] == sid2, str(rec))


def scenario_session_ttl():
    print("\n[5] 会话 TTL：只预留不上传，超时自动释放并中止 MinIO multipart")
    tid = new_team("ttl", 1024 * 1024)
    st, r = reserve(tid, 200 * 1024, key=K("ttl-session-1"), ttl=5)
    sid = r["session"]["id"]
    check("短 TTL 预留成功", st == 201)
    # 触发 multipart 创建，验证 sweeper 补偿中止
    call("POST", f"/api/uploads/{sid}/parts/1", idem=K("ttl-sign-1"))
    print("    等待 TTL+sweeper（约 9s）...")
    expired = False
    for _ in range(12):
        time.sleep(1)
        call("POST", "/api/admin/sweep")
        s = call("GET", f"/api/uploads/{sid}")[1]["session"]
        if s["status"] == "expired":
            expired = True
            break
    check("会话被标记 expired", expired)
    t = call("GET", f"/api/teams/{tid}")[1]
    check("预留自动释放、配额恢复", t["reserved_bytes"] == 0 and t["available_bytes"] == t["quota_bytes"], str(t))
    # 过期会话不允许再 complete
    st, d = call("POST", f"/api/uploads/{sid}/complete",
                 {"parts": [{"part_number": 1, "etag": "x"}]}, idem=K("ttl-complete-late"))
    check("过期会话 complete 被拒", st == 409, f"status={st}")
    es = call("GET", f"/api/teams/{tid}/ledger")[1]["entries"]
    rel = [e for e in es if e["entry_type"] == "release"]
    check("只有一次释放流水（TTL 与 abort 不会双释放）", len(rel) == 1, str([e["idempotency_key"] for e in rel]))


def scenario_trace_e2e():
    print("\n[6] 端到端可追溯：占用变化逐笔对应会话与对象")
    tid = new_team("trace", 10 * 1024 * 1024)
    st, d1, s1 = upload_full(tid, 100 * 1024, key=K("trace-up-1"))
    st, d2, s2 = upload_full(tid, 200 * 1024, key=K("trace-up-2"))
    call("DELETE", f"/api/objects/{d1['object_id']}?team_id={tid}", idem=K("trace-del-1"))
    es = call("GET", f"/api/teams/{tid}/ledger")[1]["entries"]
    by_session = {}
    for e in es:
        by_session.setdefault(e["session_id"], []).append(e)
    check("每个会话都有 reserve/release(/commit) 轨迹",
          sorted(e["entry_type"] for e in by_session[s1]) == ["commit", "delete", "release", "reserve"]
          and sorted(e["entry_type"] for e in by_session[s2]) == ["commit", "release", "reserve"])
    t = call("GET", f"/api/teams/{tid}")[1]
    # 流水重放余额 == teams 表余额
    sums = {"reserve": 0}
    occ = sum(e["delta_occupied"] for e in es)
    resv = sum(e["delta_reserved"] for e in es)
    check("流水重放余额与余额表一致", occ == t["occupied_bytes"] and resv == t["reserved_bytes"],
          f"ledger=({occ},{resv}) table=({t['occupied_bytes']},{t['reserved_bytes']})")
    check("最终占用 = 200KiB（对象1已删）", t["occupied_bytes"] == 200 * 1024, str(t))


def main():
    print(f"目标服务: {BASE}")
    st, _ = call("GET", "/health")
    if st != 200:
        print("服务不可达，请先启动 Go 后端")
        sys.exit(2)
    scenario_concurrent()
    scenario_failure_abort()
    scenario_out_of_order()
    scenario_retention()
    scenario_session_ttl()
    scenario_trace_e2e()
    print(f"\n结果: {PASS} passed, {FAIL} failed")
    sys.exit(1 if FAIL else 0)


if __name__ == "__main__":
    main()
