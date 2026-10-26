# ddns-go 邮件通知分支（IP 变更提醒）

基于 [jeessy2/ddns-go](https://github.com/jeessy2/ddns-go) **v6.17.7**（commit `7aad574`）的定制分支，
新增「公网 IP 变更时发送邮件提醒」功能。其余行为与原版一致。

- 源码：`/home/mist/work/ddns-go`（分支 `master`，提交 `0ea105f`）
- 镜像：`ddns-go-email:6.17.7-email.3`（约 40 MB，多阶段构建）
- 测试：`/home/mist/work/test`（假 SMTP + 假回调，可随时重跑）

## 与原版的差异

| 文件 | 改动 |
|---|---|
| `config/email.go` | 新增：`Email` 配置结构、SMTP 投递、邮件模板、IP 变更状态持久化 |
| `config/config.go` | `Config` 增加 `Email Email` 字段；新增 `ResetConfigCache()`（测试用） |
| `dns/index.go` | 每轮收集检测到的 IP，循环结束后调用 `NotifyIPChangeEmail` |
| `web/save.go` | 解析并保存邮件配置；SMTP 密码留空表示保持原值 |
| `web/writing.go` + `web/writing.html` | 新增「邮件通知」配置区与「发送测试邮件」按钮 |
| `web/emailTest.go` | 新增 `/emailTest` 接口 |
| `main.go` | 注册 `/emailTest` 路由；`-f` 检测间隔默认值 300 → **600 秒** |
| `static/i18n.js` / `util/messages.go` | 中英文文案 |
| `Dockerfile.email` | 多阶段构建的镜像定义，CMD 默认 `-f 600` |
| `config/email_test.go` / `web/email_test.go` | 单元测试 |

## 行为语义

1. 每轮检测后，把本轮 IPv4/IPv6 与「上次成功通知的 IP」比较。
2. **只有真正变化时才发信**；IP 未变的轮次完全静默。
3. 首次运行（无历史记录）：
   - `onstart: true`（默认）→ 发一封基线邮件；
   - `onstart: false` → 只记录基线，不发信。
4. 发送失败时**不更新状态**，下一轮自动重试，不会丢事件。
5. 某一轮取不到地址（瞬时失败）时，该地址不参与比对，也不会覆盖历史值，避免"掉线"被误报成"IP 变更"。
6. 临时/已弃用的 IPv6（SLAAC 隐私地址）会被忽略，不会因为地址按小时轮换而反复发信。
7. 状态文件与配置文件同目录：`<配置文件路径>.email-state.json`，随挂载卷持久化；
   写入采用「临时文件 + rename」原子替换，写盘失败时自动降级到内存状态，**不会重复轰炸**。
8. SMTP 会话有 45 秒硬超时，服务器半死时不会把 DNS 更新主循环一起拖住。
9. 邮件通知与原有 Webhook 相互独立，可同时开启。

## 检测间隔

fork 版把默认检测间隔从 300 秒改为 **600 秒（10 分钟）**：

- 程序 flag：`-f` 默认 `600`（`main.go`）
- 镜像 CMD：`-l :9876 -f 600`（`Dockerfile.email`）

`RunTimer` 是「执行一轮 → sleep(`-f`)」的循环，所以实际周期 = 600 秒 + 单轮耗时。
每轮都会重新获取公网 IP（无缓存），因此 IP 变化后最迟约 10 分钟发出提醒。

另外 `-cacheTimes`（默认 5）只影响「与 DNS 服务商比对」的频率：IP 未变时跳过 5 轮，
第 6 轮才真正调用服务商 API，用于避免触发限流；它不影响 IP 获取，也不影响邮件判断。

需要临时改快/改慢时，重建容器并显式传参即可覆盖默认值：

```bash
docker run -d --name ddns-go-email --restart unless-stopped \
  -p 9876:9876 -v /opt/ddns-go:/root \
  ddns-go-email:6.17.7-email.3 -l :9876 -f 120
```

## 配置字段

在 ddns-go 网页 `邮件通知` 区域填写，落到配置文件的 `email` 段：

```yaml
email:
    enable: true            # 总开关
    smtphost: smtp.qq.com   # SMTP 服务器
    smtpport: 465           # 465 / 587 / 25
    username: you@qq.com    # SMTP 账号（留空则匿名发送）
    password: 授权码         # 密码或授权码（网页上不回显）
    from: ddns-go <you@qq.com>
    to: a@example.com, b@example.com   # 多个用逗号分隔
    encrypt: ssl            # ssl(465) / starttls(587) / none(25)
    onstart: true           # 首次启动也发基线邮件
```

**加密方式**：

| 端口 | `encrypt` | 说明 |
|---|---|---|
| 465 | `ssl` | 隐式 TLS，连上即加密 |
| 587 | `starttls` | 明文连接后升级 TLS |
| 25 | `none` | 不加密（仅内网/本地测试）|

`encrypt` 留空时按端口自动推断。认证先尝试 `AUTH PLAIN`，失败自动回退 `AUTH LOGIN`（兼容只支持 LOGIN 的服务商）。

## 构建

```bash
cd /home/mist/work/ddns-go
sudo docker build -f Dockerfile.email -t ddns-go-email:6.17.7-email.3 .
```

## 运行

```bash
sudo docker run -d --name ddns-go-email --restart unless-stopped \
  -p 9876:9876 \
  -v /opt/ddns-go:/root \
  ddns-go-email:6.17.7-email.3
```

打开 `http://<主机IP>:9876`，配置 DNS 与邮件通知后保存即可。
配置与邮件状态都保存在挂载的 `/root` 目录内。

## 测试

```bash
# 单元测试
sudo docker run --rm -v $PWD:/src -w /src -v /home/mist/work/gomod:/go/pkg/mod \
  -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 golang:1.25-alpine go test ./...

# 端到端（本地假 SMTP + 假回调，不依赖真实邮箱）
bash /home/mist/work/test/run_e2e.sh
python3 /home/mist/work/test/verify_mail.py
```

已验证的三条主路径：
- 首次启动 → 1 封基线邮件
- IP 变化 → 1 封变更邮件（含「上次 IP」）
- IP 未变 → 不重复发送

## 更新记录

### 6.17.7-email.3

针对稳定性做了三项修复，均有单元测试覆盖：

| 问题 | 修复 |
|---|---|
| 状态文件写不进去时（只读挂载/权限/磁盘满），每轮都判定"IP 变了"，**每 10 分钟重发同一封** | 先更新内存态再落盘；落盘失败只告警一次并改用内存状态，本次运行内不再重复提醒 |
| 状态文件直接覆盖写，进程中途被杀会留下截断的 JSON | 改为「临时文件 + `os.Rename`」原子替换；读到损坏文件时按首次运行处理并记日志 |
| SMTP 连接建立后没有读写超时，服务器半死时 `Send` 无限阻塞，**把 DNS 更新主循环一起卡死** | 连接后设置 45 秒会话级 deadline，整段 SMTP 对话有硬上限 |
| IPv6 隐私扩展地址（SLAAC temporary）按小时轮换，每次轮换都发一封邮件 | 解析 `/proc/net/if_inet6` 的标志位，忽略 temporary(0x01)/deprecated(0x20)/tentative(0x40) 地址 |
| 某轮取不到 IP（瞬时失败）会被当成"变更为空" | 空值不参与比对、不覆盖历史状态 |

新增测试：`TestStoreEmailStateWritesAtomically`、`TestStoreEmailStateFallsBackToMemory`、
`TestLoadEmailStateToleratesCorruptFile`、`TestParseIfInet6`、`TestFilterUnstableIPv6`、
`TestFilterUnstableIPv6WithoutProc`、`TestSendHonorsSessionTimeout`。

### 6.17.7-email.2

- 默认检测间隔 300 → 600 秒（`-f` 默认值与镜像 CMD 同步修改）
- 修正镜像内编译期版本字符串（此前 tag 是 email.2 但 `-v` 输出 email.1）

### 6.17.7-email.1

- 首个版本：IP 变更邮件提醒、网页配置区、发送测试邮件接口

## 已知边界

- 邮件正文为 `text/plain`，未做 HTML 模板。
- 未做发送限流：若公网 IPv4 在短时间内反复横跳，仍会按变化次数各发一封。
- 纯 IPv6 场景下邮件标题会显示 `- -> -`（标题固定用 IPv4 拼接），正文信息完整；待后续修复。
- 状态是全局的（一份配置对应一个 IP 基线）；若配置了多个差异很大的 IP 来源，会合并成一条记录。
- 未接入原版的 Webhook 变量体系（`#{ipv4Addr}` 等），邮件正文格式由本分支固定。
- IPv6 过滤依赖 `/proc/net/if_inet6`；若 IPv6 来自远程 URL（容器内看不到该地址），无法判断临时标志，此时不做过滤。
