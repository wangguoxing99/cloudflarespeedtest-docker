# CloudflareSpeedTest Web Manager (Docker)

[![Docker Build & Publish](https://github.com/YOUR_USERNAME/YOUR_REPO/actions/workflows/docker-publish.yml/badge.svg)](https://github.com/YOUR_USERNAME/YOUR_REPO/actions/workflows/docker-publish.yml)

一个轻量级的 Docker 容器，为 [CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest) 提供 Web 管理界面、定时任务调度以及自动更新 Cloudflare DNS 记录功能。

## ✨ 特性

- **极简镜像**：镜像体积极小（~15MB），仅包含 Web 管理器，不内置核心文件。
- **Web 管理**：通过浏览器上传/更新 `cfst` 可执行文件及 IP 库，无需重启容器。
- **灵活测速**：
  - 支持 IPv4、IPv6 或混合测速（自动合并结果）。
  - 支持指定地区码（如 `HKG`, `NRT`）进行过滤（自动开启 HTTPing）。
- **自动化**：内置 Cron 定时任务，测速后自动将最快的 IP 解析到指定域名。
- **多架构支持**：支持 AMD64 (x86_64) 和 ARM64 (树莓派/M1/NAS)。

## 🚀 快速部署

### 1. 使用 Docker Compose (推荐)

创建 `docker-compose.yml` 文件：

```yaml
version: '3'
services:
  cfst-web:
    # 如果你使用自己的镜像，请替换为 ghcr.io/你的用户名/你的仓库名:latest
    # 或者先本地构建: build: .
    image: ghcr.io/your-username/cfst-web:latest 
    container_name: cfst-web
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./data:/app/data  # 必须挂载，用于保存配置和上传的文件
    environment:
      - TZ=Asia/Shanghai
