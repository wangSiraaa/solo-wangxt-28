#!/usr/bin/env python3
"""为浏览器截图准备一个含全部金额形态的团队：
  reserve +700KiB / release -700KiB / commit +700KiB（完成上传）
  abort 的 release -256KiB（取消释放）
  delete -300KiB（手动删除回收占用）
  到期回收 delete -128KiB（保留期过期 sweeper 回收）
  一笔仍在活跃的预留 +400KiB（长 TTL，截图时保持琥珀色）
"""
import json, time, uuid, urllib.request, urllib.error

BASE = "http://127.0.0.1:8080"


def call(method, path, body=None, idem=None):
    headers = {}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if idem:
        headers["Idempotency-Key"] = idem
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


def put(url, data):
    req = urllib.request.Request(url, data=data, method="PUT")
    with urllib.request.urlopen(req) as r:
        return r.headers["ETag"]


def full_upload(tid, size, name, retention=0, byte=0x5a, ttl=None):
    body = {"object_key": name, "declare_size": size, "retention_seconds": retention}
    if ttl:
        body["ttl_seconds"] = ttl
    st, r = call("POST", f"/api/teams/{tid}/uploads", body, idem=uuid.uuid4().hex)
    assert st == 201, r
    sid = r["session"]["id"]
    _, sig = call("POST", f"/api/uploads/{sid}/parts/1", idem=uuid.uuid4().hex)
    etag = put(sig["url"], bytes([byte]) * size)
    st, done = call("POST", f"/api/uploads/{sid}/complete",
                    {"parts": [{"part_number": 1, "etag": etag}]}, idem=uuid.uuid4().hex)
    assert st == 200, done
    return done["object_id"], sid


def main():
    name = "browser-demo"
    # 如已存在同名团队则换唯一名
    st, t = call("POST", "/api/teams", {"name": name, "quota_bytes": 8 * 1024 * 1024})
    if st != 201:
        name = name + "-" + uuid.uuid4().hex[:6]
        st, t = call("POST", "/api/teams", {"name": name, "quota_bytes": 8 * 1024 * 1024})
    tid = t["id"]
    print("team:", tid, name)

    # 1) 完成一个 700KiB 上传
    full_upload(tid, 700 * 1024, "report-700k.bin")
    # 2) 上传 300KiB 后手动删除 -> delete -300KiB
    oid, _ = full_upload(tid, 300 * 1024, "to-delete-300k.bin")
    st, d = call("DELETE", f"/api/objects/{oid}?team_id={tid}", idem=uuid.uuid4().hex)
    assert st == 200, d
    # 3) 短保留期 128KiB 对象 -> sweeper 到期回收
    full_upload(tid, 128 * 1024, "expires-soon-128k.bin", retention=2, byte=0x33)
    # 4) 预留 256KiB 后取消 -> release -256KiB
    st, r = call("POST", f"/api/teams/{tid}/uploads",
                 {"object_key": "aborted-256k.bin", "declare_size": 256 * 1024, "ttl_seconds": 600},
                 idem=uuid.uuid4().hex)
    sid = r["session"]["id"]
    call("POST", f"/api/uploads/{sid}/parts/1", idem=uuid.uuid4().hex)  # 创建 multipart
    st, ab = call("POST", f"/api/uploads/{sid}/abort", {}, idem=uuid.uuid4().hex)
    assert ab.get("released") is True, ab
    # 5) 一笔活跃预留 400KiB（长 TTL，截图时保持 reserved）
    st, r = call("POST", f"/api/teams/{tid}/uploads",
                 {"object_key": "inflight-400k.bin", "declare_size": 400 * 1024, "ttl_seconds": 600},
                 idem=uuid.uuid4().hex)
    print("active reserve session:", r["session"]["id"])

    # 等 sweeper 回收短保留期对象
    for _ in range(15):
        time.sleep(1)
        call("POST", "/api/admin/sweep")
        objs = call("GET", f"/api/teams/{tid}/objects?include_deleted=1")[1]["objects"]
        target = [o for o in objs if o["object_key"].endswith("expires-soon-128k.bin")][0]
        if target.get("deleted_at"):
            break

    st, team = call("GET", f"/api/teams/{tid}")
    print("team balance:", json.dumps({k: team[k] for k in
          ("quota_bytes", "occupied_bytes", "reserved_bytes", "reclaimable_bytes")}, ensure_ascii=False))
    print("TEAM_ID=" + str(tid))


if __name__ == "__main__":
    main()
