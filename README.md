# CloudflareSpeedTest Web Manager (Docker)

一个基于 Docker 的轻量级 Web 管理面板，用于自动化运行 [CloudflareSpeedTest](https://github.com/XIU2/CloudflareSpeedTest)，并将最优 IP 自动解析到 Cloudflare 托管的域名。

无需手动 SSH 连接服务器，全通过 Web 界面管理核心文件、配置参数、查看实时日志并自动更新 DNS。

## ✨ 主要特性

* **轻量化**：基于 Alpine Linux，镜像极小，占用资源极低。
* **Web 文件管理**：支持在网页端上传/更新 `cfst` 二进制文件及 `ip.txt`/`ipv6.txt`，无需重启容器，支持多架构（AMD64/ARM64）。
* **灵活测速配置**：
    * 支持 IPv4、IPv6 或混合测速。
    * 支持自定义下载测速地址 (`-url`)。
    * 支持指定测速端口 (`-tp`) 和延迟上下限 (`-tl`/`-tll`)。
    * 支持指定地区码（如 `HKG`, `NRT`）并自动开启 HTTPing。
* **智能 DNS 解析**：
    * **负载均衡模式**：单域名对应多个优选 IP（如 `speed.abc.com` 解析到最快的 10 个 IP）。
    * **1对1 分发模式**：多域名按速度排名一一对应解析（如 `Line1` 解析第1快，`Line2` 解析第2快...）。
    * **自动修正**：强制要求设置主域名，完美解决 Cloudflare API 导致的 `yx.abc.com.abc.com` 双重后缀问题。
    * **彻底清理**：每次更新前自动一次性获取并清理旧记录，防止记录残留。
* **任务自动化**：内置 Cron 定时任务，全自动测速并更新 DNS。
* **实时日志**：支持网页端查看实时滚动日志，并支持 **一键清除** 历史日志。

## 🛠️ 部署方式

你需要先安装 Docker。推荐使用 Docker Compose。

### 方式一：Docker Compose (推荐)

1.  创建一个目录（例如 `cfst-web`），并在其中创建 `docker-compose.yml` 文件：

    ```yaml
    version: '3'
    services:
      cfst-web:
        # 如果你自己构建了镜像，请使用构建好的镜像名
        # image: your-image-name:latest
        build: .  # 如果你有源码，直接构建；如果没有，请使用预编译镜像
        container_name: cfst-web
        restart: unless-stopped
        ports:
          - "8080:8080"
        volumes:
          - ./data:/app/data
        environment:
          - TZ=Asia/Shanghai
    ```

2.  在同级目录下启动容器：

    ```bash
    docker-compose up -d
    ```

### 方式二：Docker CLI

如果你不想使用 Compose，可以直接通过命令运行：

```bash
# 1. 构建镜像 (假设你在源码目录)
docker build -t cfst-web .

# 2. 运行容器
docker run -d \
    --name cfst-web \
    --restart unless-stopped \
    -p 8080:8080 \
    -v $(pwd)/data:/app/data \
    -e TZ=Asia/Shanghai \
    cfst-web

#⚙️ 初始化与使用指南
容器启动后，请访问 http://你的服务器IP:8080 进入管理后台。

第一步：上传核心文件 (首次运行必须)
由于版权和架构兼容性原因，镜像内不包含 cfst 测速程序，你需要手动上传：

前往 CloudflareSpeedTest Releases 下载对应你 CPU 架构的压缩包（Linux amd64 或 arm64）。

解压文件。

在 Web 界面 "1. 核心文件管理" 卡片中：

点击 "执行文件 (cfst)" 后的上传按钮，上传解压得到的 CloudflareST 文件 (程序会自动重命名并赋予权限)。

点击 "IPv4" 上传 ip.txt。

点击 "IPv6" 上传 ipv6.txt (如果需要)。

点击右上角的 "刷新状态"，确保状态变为绿色的 "√"。

第二步：配置参数
在 "2. 参数配置" 卡片中填写信息：

1. Cloudflare API 设置
Email: Cloudflare 账号邮箱。

Global API Key: 在 CF 后台 -> My Profile -> API Tokens -> Global API Key 查看。

Zone ID: 域名的区域 ID (在域名概述页右下角)。

主域名 (Main Domain): (必填) 填写该 Zone 的根域名（例如 abc.com）。

重要作用：程序会用它来剔除子域名后缀，防止解析变成 yx.abc.com.abc.com。

2. 域名解析模式
优选域名: 填写你想解析的完整子域名。

单域名模式: 填入 speed.abc.com。程序会将最快的 N 个 IP 全部解析到这一个域名（负载均衡）。

多域名模式: 填入 Line1.abc.com,Line2.abc.com (逗号分隔)。程序会将第 1 快的 IP 给 Line1，第 2 快的给 Line2... 实现线路分发。

3. 测速参数
自定义测速地址: 可填入 Cloudflare CDN 的大文件下载地址，留空则使用默认。

延迟/速度限制: 根据需求设置 -tl (上限), -tll (下限), -sl (速度下限)。

测速端口: 默认为 443，可改为 80, 2053 等 CF 支持的端口。

第三步：保存并运行
点击底部的 "💾 保存配置"。

点击右侧日志栏顶部的 "⚡ 立即测速"。

观察右侧 "实时动态日志"，查看测速进度和 DNS 更新结果。

如果日志太长，可以点击 "🗑️ 清除日志" 按钮清空显示。
