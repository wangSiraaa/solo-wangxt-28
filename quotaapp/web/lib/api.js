const BASE = process.env.NEXT_PUBLIC_API_BASE || "http://127.0.0.1:8080";

export function uuid() {
  if (typeof crypto !== "undefined" && crypto.randomUUID) return crypto.randomUUID();
  return "id-" + Math.random().toString(16).slice(2) + Date.now().toString(16);
}

export async function api(method, path, body, idemKey) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (idemKey) headers["Idempotency-Key"] = idemKey;
  const res = await fetch(BASE + path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    cache: "no-store",
  });
  const text = await res.text();
  let data = {};
  try {
    data = text ? JSON.parse(text) : {};
  } catch {
    data = { raw: text };
  }
  if (!res.ok) {
    const err = new Error(data.error || `HTTP ${res.status}`);
    err.status = res.status;
    err.code = data.code;
    err.retryAfter = data.retry_after;
    throw err;
  }
  return data;
}

export const fmtBytes = (n) => {
  if (n === null || n === undefined) return "-";
  if (n === 0) return "0 B";
  // 负向流水（释放预留 / 删除回收）必须保持符号，Math.log(负数)=NaN 会把单位算坏
  const sign = n < 0 ? "-" : "";
  const abs = Math.abs(n);
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(abs) / Math.log(1024)));
  return `${sign}${(abs / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
};

export const fmtTime = (t) => {
  if (!t) return "-";
  const d = new Date(t);
  return d.toLocaleString("zh-CN", { hour12: false });
};

/**
 * 大文件分片上传：浏览器全程只与预签 URL 打交道，
 * 不会拿到 MinIO access/secret key。
 *
 * 预留与分片使用稳定的幂等键（sessionKey / partKey#n），
 * 网络重试或用户重复点“开始”都不会重复预留/重复入账。
 */
export async function multipartUpload({
  teamId,
  file,
  retentionSeconds,
  partSize,
  onProgress,
  signal,
  sessionKey,
}) {
  const key = sessionKey || uuid();
  const reserved = await api(
    "POST",
    `/api/teams/${teamId}/uploads`,
    {
      object_key: file.name,
      content_type: file.type || "application/octet-stream",
      declare_size: file.size,
      part_size: partSize,
      retention_seconds: retentionSeconds || 0,
    },
    key
  );
  const sid = reserved.session.id;
  const totalParts = reserved.total_parts;
  const parts = [];

  for (let n = 1; n <= totalParts; n++) {
    if (signal?.aborted) throw new DOMException("aborted", "AbortError");
    const start = (n - 1) * partSize;
    const end = Math.min(start + partSize, file.size);
    const blob = file.slice(start, end);

    // 固定键：预签接口重试返回同一组 URL
    const signed = await api("POST", `/api/uploads/${sid}/parts/${n}`, undefined, `part-sign:${key}:${n}`);
    const etag = await putWithRetry(signed.url, blob, 3);
    parts.push({ part_number: n, etag });
    onProgress?.(n / totalParts, n, totalParts);
  }

  const done = await api("POST", `/api/uploads/${sid}/complete`, { parts }, `complete:${key}`);
  return done;
}

async function putWithRetry(url, blob, tries) {
  let lastErr;
  for (let i = 0; i < tries; i++) {
    try {
      const res = await fetch(url, { method: "PUT", body: blob });
      if (!res.ok) throw new Error(`MinIO PUT 失败 HTTP ${res.status}`);
      return res.headers.get("ETag");
    } catch (e) {
      lastErr = e;
      await new Promise((r) => setTimeout(r, 300 * (i + 1)));
    }
  }
  throw lastErr;
}
