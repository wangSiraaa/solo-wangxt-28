"use client";

import { useState, useRef } from "react";
import { api, multipartUpload, fmtBytes } from "../lib/api";

export default function UploadPanel({ teamId, quota, onChanged }) {
  const [file, setFile] = useState(null);
  const [retention, setRetention] = useState("0");
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState(null);
  const [msg, setMsg] = useState(null);
  const [err, setErr] = useState("");
  const sessionKeyRef = useRef(null);

  // 演示环境文件小：用 1 MiB 分片；生产可调大
  const PART_SIZE = 1024 * 1024;

  function pick(f) {
    setFile(f);
    setMsg(null);
    setErr("");
    setProgress(null);
  }

  async function start() {
    if (!file) return;
    setBusy(true);
    setErr("");
    setMsg(null);
    // 同一次选择使用固定会话键：断网重试不会二次预留
    if (!sessionKeyRef.current) sessionKeyRef.current = crypto.randomUUID ? crypto.randomUUID() : String(Date.now());
    try {
      const done = await multipartUpload({
        teamId,
        file,
        retentionSeconds: Number(retention),
        partSize: PART_SIZE,
        sessionKey: sessionKeyRef.current,
        onProgress: (p, n, total) => setProgress({ p, n, total }),
      });
      setMsg({
        kind: "ok",
        text: `完成：会话 #${done.session.id} → 对象 #${done.object_id}，实际入账 ${fmtBytes(
          done.final_size
        )} 字节（占用）`,
      });
      setFile(null);
      sessionKeyRef.current = null;
      setProgress(null);
      onChanged?.();
    } catch (e) {
      setErr(`${e.message}${e.code === "quota_exceeded" ? "（预留被拒绝，未发生扣费）" : ""}`);
    } finally {
      setBusy(false);
    }
  }

  async function abortOnly() {
    // 演示：单纯申请一笔预留再取消，观察 reserved 回落
    setBusy(true);
    setErr("");
    const key = "manual-reserve-" + (crypto.randomUUID ? crypto.randomUUID() : Date.now());
    try {
      const r = await api("POST", `/api/teams/${teamId}/uploads`, {
        object_key: "cancel-demo.bin",
        declare_size: 256 * 1024,
        retention_seconds: 0,
      }, key);
      const sid = r.session.id;
      await api("POST", `/api/uploads/${sid}/abort`, {}, "abort-" + key);
      setMsg({ kind: "ok", text: `演示完成：会话 #${sid} 预留 256 KiB 后已立即释放` });
      onChanged?.();
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card">
      <h2>上传文件（先预留、完成转占用）</h2>
      <div className="form-row" style={{ marginBottom: 10 }}>
        <input type="file" onChange={(e) => pick(e.target.files?.[0] || null)} />
      </div>
      <div className="form-row" style={{ marginBottom: 12 }}>
        <label className="muted">对象保留期</label>
        <select value={retention} onChange={(e) => setRetention(e.target.value)}>
          <option value="0">永久保留（0=不过期）</option>
          <option value="8">8 秒（演示到期回收）</option>
          <option value="60">1 分钟</option>
          <option value="3600">1 小时</option>
          <option value="86400">1 天</option>
        </select>
        <button onClick={start} disabled={!file || busy}>
          {busy ? "处理中…" : "预留并上传"}
        </button>
        <button className="secondary" onClick={abortOnly} disabled={busy}>演示：预留后取消</button>
      </div>
      {file && (
        <div className="subtle">
          {file.name} · {fmtBytes(file.size)} · 共 {Math.max(1, Math.ceil(file.size / PART_SIZE))} 分片
        </div>
      )}
      {progress && (
        <div style={{ marginTop: 10 }}>
          <div className="progress"><span style={{ width: `${progress.p * 100}%` }} /></div>
          <div className="subtle" style={{ marginTop: 6 }}>分片 {progress.n}/{progress.total} 已直传 MinIO（预签 URL）</div>
        </div>
      )}
      {msg && <div className="flash ok">{msg.text}</div>}
      {err && <div className="flash err">{err}</div>}
      <div className="lock-note">
        浏览器不会收到任何 MinIO 凭据；分片通过后端签发的短期 URL 直传。
        超过 {fmtBytes(quota)} 可用额度的预留会被后端以 409 拒绝。
      </div>
    </div>
  );
}
