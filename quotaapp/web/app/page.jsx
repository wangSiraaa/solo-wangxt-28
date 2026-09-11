"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { api, fmtBytes } from "../lib/api";

export default function Home() {
  const [teams, setTeams] = useState([]);
  const [name, setName] = useState("");
  const [quotaMiB, setQuotaMiB] = useState(10);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);

  async function refresh() {
    const r = await api("GET", "/api/teams");
    setTeams(r.teams || []);
    setLoading(false);
  }

  useEffect(() => {
    refresh().catch((e) => setErr(e.message));
    const t = setInterval(() => refresh().catch(() => {}), 4000);
    return () => clearInterval(t);
  }, []);

  async function createTeam(e) {
    e.preventDefault();
    setErr("");
    try {
      await api("POST", "/api/teams", {
        name: name.trim(),
        quota_bytes: Math.max(0, Number(quotaMiB) * 1024 * 1024),
      });
      setName("");
      await refresh();
    } catch (e2) {
      setErr(e2.message);
    }
  }

  return (
    <>
      <div className="card" style={{ marginBottom: 20 }}>
        <h2>新建团队配额</h2>
        <form className="form-row" onSubmit={createTeam}>
          <input placeholder="团队名称，如 data-platform" value={name} onChange={(e) => setName(e.target.value)} required />
          <input
            type="number" min="0" step="0.25" style={{ maxWidth: 180 }}
            value={quotaMiB} onChange={(e) => setQuotaMiB(e.target.value)}
          />
          <span className="muted">MiB 配额</span>
          <button type="submit">创建</button>
        </form>
        {err && <div className="error">{err}</div>}
        <div className="lock-note">小提示：想复现并发抢配额，可建一个 1 MiB 的团队。</div>
      </div>

      <div className="card">
        <h2>团队用量（占用 / 预留 / 可回收）</h2>
        {loading ? (
          <div className="muted">加载中…</div>
        ) : teams.length === 0 ? (
          <div className="muted">还没有团队，先创建一个。</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>团队</th>
                <th className="right">配额</th>
                <th className="right">占用</th>
                <th className="right">预留</th>
                <th className="right">可用</th>
                <th style={{ width: 260 }}>分布</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {teams.map((t) => {
                const occPct = (t.occupied_bytes / t.quota_bytes) * 100 || 0;
                const resPct = (t.reserved_bytes / t.quota_bytes) * 100 || 0;
                return (
                  <tr key={t.id}>
                    <td><strong>{t.name}</strong><div className="subtle mono">#{t.id}</div></td>
                    <td className="right mono">{fmtBytes(t.quota_bytes)}</td>
                    <td className="right mono">{fmtBytes(t.occupied_bytes)}</td>
                    <td className="right mono" style={{ color: "var(--amber)" }}>{fmtBytes(t.reserved_bytes)}</td>
                    <td className="right mono">{fmtBytes(t.quota_bytes - t.occupied_bytes - t.reserved_bytes)}</td>
                    <td>
                      <div className="bar" title={`占用 ${occPct.toFixed(1)}% · 预留 ${resPct.toFixed(1)}%`}>
                        <span className="occ" style={{ width: `${occPct}%` }} />
                        <span className="res" style={{ width: `${resPct}%` }} />
                      </div>
                    </td>
                    <td className="right"><Link href={`/teams/${t.id}`}>进入 →</Link></td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}
