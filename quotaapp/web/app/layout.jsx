import "./globals.css";

export const metadata = {
  title: "对象存储配额中心",
  description: "按团队计费的占用 / 预留 / 可回收空间管理",
};

export default function RootLayout({ children }) {
  return (
    <html lang="zh-CN">
      <body>
        <div className="container">
          <div className="topbar">
            <div className="brand">对象存储 · 配额中心<span className="dot">_</span></div>
            <div className="subtle">
              Next.js 展示 · Go/Gin 配额服务 · PostgreSQL 账本 · 本地 MinIO
            </div>
          </div>
          {children}
        </div>
      </body>
    </html>
  );
}
