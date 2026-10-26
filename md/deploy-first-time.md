# PlcBridge 首次部署指南

> 适用版本：2026-10-26 之后的版本（强制 API 密钥）。
>
> 前提：**现场尚未部署**。本指南按"全新安装"编写，不涉及数据迁移。

---

## 0. TL;DR

现场部署只需要做三件事：

1. 编译两个可执行文件
2. 确保 `config` 目录存在
3. 跑一条安装命令

**API 密钥不需要人工设置** —— 谁先启动谁自动生成并落盘，两个进程共享同一个文件。

---

## 1. 编译

```powershell
cd F:\Project\PlcBridge

go build -o bin\plcbridge.exe     ./cmd/plcbridge
go build -o bin\plcbridge-web.exe ./cmd/plcbridge-web
```

> 前端资源（`web/dist`）需要单独构建并内嵌进 `plcbridge-web.exe`。
> 若 `internal/interface/web/dist/` 为空，页面能开但没有 UI 资源。

---

## 2. 准备配置目录

```powershell
New-Item -ItemType Directory -Force F:\Project\PlcBridge\config | Out-Null
```

**为什么目录必须先建好**：`-config` / `PLCBRIDGE_CONFIG` 指定的目录不存在时，进程**直接拒启动**（不降级兜底），
这是 2026-10-12 修掉 dual-dir 漂移 bug 后的刻意设计。

---

## 3. 部署

### 3.1 Windows（计划任务 + supervisor 自愈）

```powershell
# 管理员 PowerShell
cd F:\Project\PlcBridge
.\scripts\install-service.ps1 -Install
```

它会：

- 注册计划任务 `PlcBridgeSupervisor`（开机自启、SYSTEM 账号、崩溃 1 分钟后自动重启）
- 立刻启动 supervisor
- supervisor 拉起 `plcbridge-web`（:8080）和 `plcbridge`（:8082）

验证：

```powershell
.\scripts\install-service.ps1 -Status
```

期望输出：

```
[supervisor]  PID xxxxx  running
[state]      status=running  web=xxxx  core=xxxx  msg=ok
[port 8080] plcbridge-web   listening
[port 8082] plcbridge       listening
```

> 端口对照：`8080` = web 控制台 / REST API，`8082` = core 的 API（采集通道、点位、回写），
> `9091` = core 的 Prometheus metrics。

### 3.2 Linux（systemd）

```bash
sudo useradd -r -s /usr/sbin/nologin plcbridge
sudo mkdir -p /opt/plcbridge /etc/plcbridge
sudo chown -R plcbridge:plcbridge /opt/plcbridge /etc/plcbridge

sudo cp bin/plcbridge bin/plcbridge-web /opt/plcbridge/
sudo cp scripts/systemd/*.service /etc/systemd/system/

sudo systemctl daemon-reload
sudo systemctl enable --now plcbridge-web plcbridge
sudo systemctl status plcbridge plcbridge-web
```

> **注意**：unit 里的 `--config /etc/plcbridge` 必须一致。
> 两个 unit 指向同一个 config 目录 → 共享同一份 `api.secret` → 天然用同一个密钥。

---

## 4. API 密钥是怎么来的（为什么不用手工配）

### 优先级

| 优先级 | 来源 | 用途 |
|---|---|---|
| 1 | 环境变量 `PLCBRIDGE_API_SECRET` | 想手动指定时用 |
| 2 | `<configDir>/api.secret` | **默认路径 —— 自动生成，无需人工干预** |

### 自动生成流程

```
进程启动
  ├─ 环境变量有且够强？ ──是──→ 用它
  └─ 否 → <configDir>/api.secret
            ├─ 文件存在？ ──是──→ 读它（等写完再读）
            └─ 否 → 生成 32 字节随机密钥
                   └─ O_EXCL 独占创建落盘（原子）
```

**并发安全**：两个进程同时启动时，`O_CREATE|O_EXCL` 保证只有一个真正写入，
另一个发现文件已存在 → 读赢家写的密钥 → 两者拿到**同一个**密钥。

> 绝不会出现"web 用密钥 A、core 用密钥 B"导致内部调用全 401 的情况。

### 手动指定（可选）

```powershell
$env:PLCBRIDGE_API_SECRET = "<至少 32 字节的随机串>"
```

Linux 用 `EnvironmentFile`（unit 文件全局可读，别把密钥写进 unit）：

```bash
echo "PLCBRIDGE_API_SECRET=$(openssl rand -base64 32)" > /etc/plcbridge/api.env
chmod 600 /etc/plcbridge/api.env
# unit 里已有 EnvironmentFile=-/etc/plcbridge/api.env
```

---

## 5. 首次登录

web 首次启动会生成 admin 账号，初始明文密码写在：

```
<configDir>\web.bootstrap        (Windows)
<configDir>/web.bootstrap        (Linux)
```

```powershell
Get-Content F:\Project\PlcBridge\config\web.bootstrap
```

**登录后立刻改密码，然后删掉这个文件。**

---

## 6. 验证清单

```powershell
# 1. 两个进程都活着
Get-Process plcbridge, plcbridge-web

# 2. 密钥文件已生成，且不是弱值
$secret = (Get-Content F:\Project\PlcBridge\config\api.secret -Raw).Trim()
"len=$($secret.Length)"        # 期望 44

# 3. 用密钥签一个 token，验证 core 接受
$env:PLCBRIDGE_API_SECRET = $secret
$tok = go run ./cmd/gen_token
Invoke-WebRequest -Uri "http://127.0.0.1:8082/api/v1/channels/runtime" `
  -Headers @{ Authorization = "Bearer $tok" }
# 期望 200

# 4. 无 token 应被拒
Invoke-WebRequest -Uri "http://127.0.0.1:8082/api/v1/channels/runtime"
# 期望 401
```

---

## 7. 常见坑

### 7.1 `-Status` 显示 `state.json missing`

**已修复（2026-10-26）**。原因是 `install-service.ps1` 读 `$Root\runtime\`，
而 supervisor 和 web 写/读 `$ConfigDir\runtime\` —— 三者不一致。
现在三者统一到 `<configDir>/runtime/`。

若仍出现，确认 `PLCBRIDGE_CONFIG_DIR` 或 `-ConfigDir` 三处一致。

### 7.2 core 起来但 web 调它全 401

说明两个进程用了**不同的密钥**。检查：

```powershell
# 两个进程的 configDir 是否一致
Get-CimInstance Win32_Process -Filter "Name='plcbridge.exe' or Name='plcbridge-web.exe'" |
  Select-Object Name, CommandLine
```

不一致 → 统一 `-config` 指向同一目录。

### 7.3 `config` 目录在别的盘 / 别的路径

```powershell
.\scripts\install-service.ps1 -Install -ConfigDir "D:\plcdata"
```

必须与 supervisor 和 web 的 configDir 一致。

### 7.4 弱密钥被拒

```
启动被拒绝: API 密钥强度不足: 长度 8 < 32 字节
```

生产环境**不要**用 `PLCBRIDGE_ALLOW_INSECURE_SECRET=1` —— 那是本地开发专用逃生舱，
用了等于全网可伪造 Bearer Token。

### 7.5 systemd 起来后 web 日志报 "supervisor script not found"

Linux 下不跑 `supervisor.ps1`（那是 Windows 脚本），supervisor 由 systemd 接管。
这条警告无害，可忽略。

---

## 8. 相关文件

| 文件 | 作用 | 能不能删 |
|---|---|---|
| `config\config.json` | 运行时配置（通道 / 设备 / 点位） | 否 |
| `config\api.secret` | **API 共享密钥**（web ↔ core） | **否**（删了会重新生成 → 内部调用全 401） |
| `config\web.hmac` | session 签名密钥 | 否（删了旧 cookie 失效） |
| `config\web.bootstrap` | 初始明文密码（一次性） | 登录改密后删 |
| `config\data\plcbridge.db` | SQLite | 否 |
| `config\runtime\` | supervisor 状态 / 控制文件 | 否（会自动重建） |
