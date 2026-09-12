"use client";

import { useEffect, useState, useCallback } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { api, fmtBytes, fmtTime } from "../../../lib/api";
import UploadPanel from "../../../components/UploadPanel";

const TABS = [
  ["objects", "对象"],
  ["sessions", "上传会话"],
  ["ledger", "配额账本流水"],
];

export default function TeamPage() {
  const params = useParams();
  const tid = Number(params.id);
  const [tab, setTab] = useState("objects");
  const [team, setTeam] = useState(null);
  const [objects, setObjects] = useState([]);
  const [sessions, setSessions] = useState([]);
  const [entries, setEntries] = useState([]);
  const [err, setErr] = useState("");

  const refresh = useCallback(async () => {
    try {
      const t = await api("GET", `/api/teams/${tid}`);
      setTeam(t);
      const [o, s, l] = await Promise.all([
        api("GET", `/api/teams/${tid}/objects?include_deleted=1`),
        api("GET", `/api/teams/${tid}/sessions`),
        api("GET", `/api/teams/${tid}/ledger`),
      ]);
      setObjects(o.objects || []);
      setSessions(s.sessions || []);
      setEntries(l.entries || []);
    } catch (e) {
      setErr(e.message);
    }
  }, [tid]);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [refresh]);

  if (err) return <div className="error">{err}</div>;
  if (!team) return <div className="muted">加载中…</div>;

  const reclaimable = objects
    .filter((o) => !o.deleted_at && o.reclaimable)
    .reduce((a, o) => a + o.size_bytes, 0);
  const liveOcc = team.occupied_bytes;
  const pct = (n) => (team.quota_bytes ? (n / team.quota_bytes) * 100 : 0);

  return (
    <>
      <div style={{ marginBottom: 18 }}>
        <Link href="/">← 团队列表</Link>
        <h1 style={{ margin: "8px 0 2px", fontSize: 22 }}>{team.name}</h1>
        <div className="subtle mono">team #{team.id} · 配额 {fmtBytes(team.quota_bytes)}</div>
      </div>

      <div className="grid grid-4" style={{ marginBottom: 20 }}>
        <div className="card stat">
          <div className="label">占用（已落盘对象）</div>
          <div className="value" style={{ color: "var(--accent)" }}>{fmtBytes(liveOcc)}</div>
          <div className="sub">{pct(liveOcc).toFixed(1)}% 配额</div>
        </div>
        <div className="card stat">
          <div className="label">预留（进行中会话）</div>
          <div className="value" style={{ color: "var(--amber)" }}>{fmtBytes(team.reserved_bytes)}</div>
          <div className="sub">完成转占用，取消/超时释放</div>
        </div>
        <div className="card stat">
          <div className="label">可回收（保留期已过）</div>
          <div className="value" style={{ color: "var(--purple)" }}>{fmtBytes(reclaimable)}</div>
          <div className="sub">等待清扫器删除后释放占用</div>
        </div>
        <div className="card stat">
          <div className="label">仍可申请</div>
          <div className="value" style={{ color: "var(--green)" }}>
            {fmtBytes(Math.max(0, team.quota_bytes - team.occupied_bytes - team.reserved_bytes))}
          </div>
          <div className="sub">occupied + reserved ≤ quota</div>
        </div>
      </div>

      <div className="card" style={{ marginBottom: 20 }}>
        <div className="bar">
          <span className="occ" style={{ width: `${pct(liveOcc)}%` }} title="占用" />
          <span className="res" style={{ width: `${pct(team.reserved_bytes)}%` }} title="预留" />
          <span className="reclaim" style={{ width: `${pct(reclaimable)}%` }} title="可回收" />
        </div>
        <div className="legend">
          <span><i style={{ background: "var(--accent)" }} />占用</span>
          <span><i style={{ background: "var(--amber)" }} />预留</span>
          <span><i style={{ background: "var(--purple)" }} />可回收</span>
          <span><i style={{ background: "var(--panel2)" }} />未使用</span>
        </div>
      </div>

      <UploadPanel teamId={tid} quota={team.quota_bytes - team.occupied_bytes - team.reserved_bytes} onChanged={refresh} />

      <div className="tabs">
        {TABS.map(([k, label]) => (
          <a key={k} className={tab === k ? "active" : ""} href="#" onClick={(e) => { e.preventDefault(); setTab(k); }}>
            {label}
          </a>
        ))}
      </div>

      {tab === "objects" && <ObjectsTable teamId={tid} objects={objects} onChanged={refresh} />}
      {tab === "sessions" && <SessionsTable sessions={sessions} />}
      {tab === "ledger" && <LedgerTable entries={entries} />}
    </>
  );
}

function ObjectsTable({ teamId, objects, onChanged }) {
  const [busyId, setBusyId] = useState(null);
  const [flash, setFlash] = useState("");

  async function del(o) {
    setBusyId(o.id);
    setFlash("");
    try {
      await api("DELETE", `/api/objects/${o.id}?team_id=${teamId}`, undefined, `del-${o.id}-${Date.now()}`);
      setFlash(`对象 #${o.id} 已删除，占用 ${fmtBytes(o.size_bytes)} 已释放`);
      onChanged();
    } catch (e) {
      if (e.code === "retention_locked") {
        setFlash(`⛔ ${e.message} —— 后端强制校验，管理界面无法绕过`);
      } else {
        setFlash("删除失败：" + e.message);
      }
    } finally {
      setBusyId(null);
    }
  }

  async function download(o) {
    const r = await api("GET", `/api/objects/${o.id}/download`);
    window.open(r.url, "_blank");
  }

  return (
    <div className="card">
      {flash && <div className={flash.startsWith("⛔") ? "flash err" : "flash ok"}>{flash}</div>}
      <table>
        <thead>
          <tr>
            <th>对象</th><th className="right">大小</th><th>来源会话</th>
            <th>保留截止</th><th>状态</th><th></th>
          </tr>
        </thead>
        <tbody>
          {objects.length === 0 && <tr><td colSpan={6} className="muted">暂无对象</td></tr>}
          {objects.map((o) => {
            const locked = o.retention_until && new Date(o.retention_until) > new Date() && !o.deleted_at;
            return (
              <tr key={o.id}>
                <td>
                  <div>{o.object_key.replace(/^.*\//, "")}</div>
                  <div className="subtle mono">#{o.id} · {o.content_type}</div>
                </td>
                <td className="right mono">{fmtBytes(o.size_bytes)}</td>
                <td className="mono"><Link href={`#`} title="可在上传会话页查看">#{o.session_id}</Link></td>
                <td className="nowrap">
                  {o.retention_until ? (
                    <span style={{ color: locked ? "var(--red)" : "var(--purple)" }}>
                      {fmtTime(o.retention_until)}{locked ? " 🔒 保留中" : " 已到期"}
                    </span>
                  ) : <span className="muted">永久</span>}
                </td>
                <td>{o.deleted_at ? <span className="tag delete">已删除 {fmtTime(o.deleted_at)}</span>
                    : o.reclaimable ? <span className="tag expired">可回收</span>
                    : <span className="tag completed">占用中</span>}</td>
                <td className="right nowrap">
                  {!o.deleted_at && (
                    <>
                      <button className="secondary" style={{ marginRight: 6 }} onClick={() => download(o)}>下载</button>
                      <button className="danger" disabled={busyId === o.id || locked}
                              title={locked ? "保留期内禁止删除（后端强制）" : ""}
                              onClick={() => del(o)}>
                        {locked ? "🔒" : "删除"}
                      </button>
                    </>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <div className="lock-note">
        保留期未到的对象删除按钮被禁用；即使直接调用 <span className="mono">DELETE /api/objects/:id</span>，
        后端同样返回 <span className="mono">423 retention_locked</span>。
      </div>
    </div>
  );
}

function SessionsTable({ sessions }) {
  return (
    <div className="card">
      <table>
        <thead>
          <tr>
            <th>#</th><th>对象</th><th>状态</th>
            <th className="right">预留字节</th><th className="right">实际字节</th>
            <th>TTL 到期</th><th>对象</th><th className="nowrap">创建</th>
          </tr>
        </thead>
        <tbody>
          {sessions.length === 0 && <tr><td colSpan={8} className="muted">暂无上传会话</td></tr>}
          {sessions.map((s) => (
            <tr key={s.id}>
              <td className="mono">{s.id}</td>
              <td>
                <div>{s.object_key.replace(/^.*\//, "")}</div>
                <div className="subtle mono">{s.idempotency_key.slice(0, 24)}…</div>
              </td>
              <td><span className={`tag ${s.status}`}>{s.status}</span></td>
              <td className="right mono" style={{ color: "var(--amber)" }}>{fmtBytes(s.declare_size)}</td>
              <td className="right mono">{s.final_size != null ? fmtBytes(s.final_size) : "—"}</td>
              <td className="nowrap">{fmtTime(s.expires_at)}</td>
              <td className="mono">{s.object_id ?? "—"}</td>
              <td className="nowrap subtle">{fmtTime(s.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="lock-note">
        同一 <span className="mono">Idempotency-Key</span> 的重试永远返回同一行会话；
        completed / aborted / expired 均为终态，迟到的回调不会改变用量。
      </div>
    </div>
  );
}

function LedgerTable({ entries }) {
  const label = { reserve: "预留", commit: "转占用", release: "释放预留", delete: "删除回收", reclaim: "到期回收" };
  // 带符号的金额：fmtBytes 已处理负号（释放/回收），正数补 +，0 显示 —
  const signed = (n) => (n ? (n > 0 ? "+" : "") + fmtBytes(n) : "—");
  return (
    <div className="card">
      <table>
        <thead>
          <tr>
            <th>#</th><th>动作</th><th className="right">Δ预留</th><th className="right">Δ占用</th>
            <th className="right">预留余额</th><th className="right">占用余额</th>
            <th>会话</th><th>对象</th><th>原因</th><th>时间</th>
          </tr>
        </thead>
        <tbody>
          {entries.length === 0 && <tr><td colSpan={10} className="muted">暂无流水</td></tr>}
          {entries.map((e) => (
            <tr key={e.id}>
              <td className="mono">{e.id}</td>
              <td><span className={`tag ${e.entry_type}`}>{label[e.entry_type] || e.entry_type}</span></td>
              <td className="right mono" style={{ color: e.delta_reserved ? "var(--amber)" : undefined }}>
                {signed(e.delta_reserved)}
              </td>
              <td className="right mono" style={{ color: e.delta_occupied ? (e.delta_occupied > 0 ? "var(--accent)" : "var(--purple)") : undefined }}>
                {signed(e.delta_occupied)}
              </td>
              <td className="right mono">{fmtBytes(e.reserved_after)}</td>
              <td className="right mono">{fmtBytes(e.occupied_after)}</td>
              <td className="mono">{e.session_id ?? "—"}</td>
              <td className="mono">{e.object_id ?? "—"}</td>
              <td className="subtle" style={{ maxWidth: 260 }}>{e.reason}</td>
              <td className="nowrap subtle">{fmtTime(e.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="lock-note">
        流水只增不改；teams 表余额恒等于流水求和。每条变化都可追到上传会话与对象。
      </div>
    </div>
  );
}
