# ddns-go 邮件通知分支（IP 变更提醒）

基于 [jeessy2/ddns-go](https://github.com/jeessy2/ddns-go) **v6.17.7**（commit `7aad574`）的定制分支，
新增「公网 IP 变更时发送邮件提醒」功能。其余行为与原版一致。

- 源码：`/home/mist/work/ddns-go`（分支 `main`，以仓库当前 HEAD 为准）
- 镜像：`ddns-go-email:6.17.7-email-0.3`（约 40 MB，多阶段构建）
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
   - `onstart: false`（默认）→ 只记录基线，不发信；
   - `onstart: true` → 发一封基线邮件。
4. 发送失败时**不更新状态**，下一轮自动重试，不会丢事件；
   但只有认证机制不被接受时才回退 `AUTH LOGIN`，且 `DATA` 已被接收后 `QUIT` 失败不会重发。
5. 某一轮取不到地址（瞬时失败）时，该地址不参与比对，也不会覆盖历史值，避免"掉线"被误报成"IP 变更"。
6. 临时/已弃用的 IPv6（SLAAC 隐私地址）会被忽略，不会因为地址按小时轮换而反复发信。
7. 状态文件与配置文件同目录：`<配置文件路径>.email-state.json`，随挂载卷持久化；
   写入采用「临时文件 + rename」原子替换，写盘失败时自动降级到内存状态，**不会重复轰炸**。
8. 单次 SMTP 会话有 45 秒硬超时，服务器半死时不会把 DNS 更新主循环一起拖住；
   认证失败回退 LOGIN 时最多再建立一次会话。
9. 开启「获取失败时告警」后，连续 3 轮取不到 IP 发一封告警，恢复后再发一封恢复通知；
   同一次故障只告警一次，恢复通知失败会保留待发送状态并在下一轮重试。
10. 每次成功通知都会追加一条历史记录：`<配置文件路径>.email-history.jsonl`（最多保留 200 条），
    网页「历史记录」区块展示上次通知时间、当前保留条数和最近 5 条。
11. 邮件通知与原有 Webhook 相互独立，可同时开启。
12. 后台 `RunOnce` 已串行化，保存配置触发的立即执行不会与定时器并发重复发信。
13. `From`/`To` 支持 `Name <addr@example.com>` 显示名；SMTP 信封会自动使用裸地址。

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
  ddns-go-email:6.17.7-email-0.3 -l :9876 -f 120
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
    alertonfetchfailure: false  # 连续 3 轮取不到 IP 时告警
```

**加密方式**：

| 端口 | `encrypt` | 说明 |
|---|---|---|
| 465 | `ssl` | 隐式 TLS，连上即加密 |
| 587 | `starttls` | 明文连接后升级 TLS |
| 25 | `none` | 不加密（仅内网/本地测试）|

`encrypt` 留空时按端口自动推断；网页上切换端口会自动带出常用值。
认证先尝试 `AUTH PLAIN`，**只有认证阶段失败**才回退 `AUTH LOGIN`。
`none` + 非 localhost + 配置了 SMTP 账号时，程序会直接拒绝发送，避免密码明文上网；如确需内网明文认证，请自行改代码或使用 localhost 转发。

## 构建

```bash
cd /home/mist/work/ddns-go
sudo docker build -f Dockerfile.email -t ddns-go-email:6.17.7-email-0.3 .
```

## 运行

```bash
sudo docker run -d --name ddns-go-email --restart unless-stopped \
  -p 9876:9876 \
  -v /opt/ddns-go:/root \
  ddns-go-email:6.17.7-email-0.3
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

### 6.17.7-email-0.3

| 修复 | 说明 |
|---|---|
| **后台登录用户名被覆盖** | 设置页 `Username` 输入框误绑 `{{.Email.Username}}`，保存时会把管理员用户名改成 SMTP 用户名，甚至锁死后台。已改回 `{{.Username}}` 并加回归测试 |
| **`From` 显示名导致 501** | `ddns-go <you@qq.com>` 原样进 `MAIL FROM`，真实 SMTP 返回 501。现在用 `mail.ParseAddress` 拆成“信封裸地址 + 邮件头显示名”，`To` 同时支持显示名、逗号/分号/换行 |
| **QUIT 失败导致重复投递** | `DATA` 返回 250 即视为已投递；`QUIT` 错误只记日志，不再重发整封邮件 |
| **错误重试放大** | 只有认证阶段失败才回退 `AUTH LOGIN`；`MAIL FROM`/`RCPT`/`DATA`/网络错误不再重发 |
| **明文发送 SMTP 凭据** | `encrypt: none` + 非 localhost + 账号时直接拒绝，不再回退 LOGIN 把密码 base64 发出去 |
| **恢复通知丢失** | 恢复邮件发送失败时保留 `alerted` 状态，下一轮健康时继续重试 |
| **并发重复发信** | `dns.RunOnce()` 加串行锁，保存配置触发的立即执行不会与定时器重叠 |
| **IPv6 误告警** | 只有“启用且配置了域名”的地址族才算期望；告警文案区分 IPv4/IPv6 |
| **IPv6 变更标题错误** | 标题按实际变化的地址族生成，不再出现 `- -> -` 或 `1.1.1.1 -> 1.1.1.1` |
| **端口/加密配错** | 端口变化自动带出 465→ssl、587→starttls、25→none；非法端口不再静默改成 465；保存时校验地址、加密方式和 `host:port` 误填 |
| **文档** | 修正 `onstart` 默认值、45 秒超时、累计条数、源码分支等过期描述 |

新增测试：`TestParseRecipients`、`TestParseRecipientsWithDisplayName`、
`TestParseSenderKeepsDisplayNameForHeader`、`TestSendUsesBareEnvelopeAndDoesNotRetryAfterQuit`、
`TestNotifyIPChangeEmailSendsOncePerChange`、`TestHandleFetchHealthRetriesRecovery`、
`TestComposeIPChangeMailIPv6Only`，以及 Web 页面“管理员用户名不被 SMTP 用户名覆盖”的回归断言。

### 6.17.7-email-0.2

| 改动 | 说明 |
|---|---|
| **修复多源 IP 探测的 fallback** | 原版在「接口返回 200 但内容里没有 IP」「读响应体失败」这两条路径上会直接 `return`，导致配置在后面的备用接口永远不会被尝试。现在只有真正解析出地址才算成功，否则继续试下一个接口（IPv4/IPv6 都修了） |
| **新增 IP 变更历史** | 每次成功通知追加一行到 `<配置>.email-history.jsonl`，最多保留 200 条；网页「历史记录」区块展示上次通知时间、当前保留条数与最近 5 条 |
| **新增获取失败告警（可开关）** | 连续 3 轮取不到公网 IP 时发一封告警，恢复后再发一封恢复通知；同一次故障只告警一次，恢复轮不重复发基线邮件 |

配套改进：日志里输出接口响应体时会截断到 120 个字符，避免把整页 HTML 写进日志。

新增测试：`TestGetIpv4AddrFromUrlFallsBack`、`TestGetIpv4AddrFromUrlKeepsTryingAfterHTTPError`、
`TestGetIpv4AddrFromUrlReturnsEmptyWhenAllFail`、`TestEmailHistoryAppendAndSummarize`、
`TestEmailHistoryIsTrimmed`、`TestEmailHistorySummaryOnEmptyFile`、
`TestFetchFailureTrackerAlertsOncePerOutage`、`TestFetchFailureTrackerResetsAfterRecovery`。

### 6.17.7-email-0.1

首个正式版本号。此前临时 tag 为 `6.17.7-email.3`（`sha256:23510baa...`），
本次仅重命名 tag 并把编译期版本号对齐为 `6.17.7-email-0.1`，功能代码无差异。

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
- 恢复轮如果同时发生 IP 变化，仍可能收到“已恢复”和“IP 变更”两封邮件；两封内容都带当前 IP。
- 状态是全局的（一份配置对应一个 IP 基线）；若配置了多个差异很大的 IP 来源，会合并成一条记录。
- SMTP 密码/授权码与 DNS Secret 一样，以明文保存在 0600 的配置文件里；备份/迁移时注意保护。
- 未接入原版的 Webhook 变量体系（`#{ipv4Addr}` 等），邮件正文格式由本分支固定。
- IPv6 过滤依赖 `/proc/net/if_inet6`；若 IPv6 来自远程 URL（容器内看不到该地址），无法判断临时标志，此时不做过滤。
- 「获取失败」计数器只在内存里，容器重启后重新累计；历史文件记录的是**成功发出**的通知，失败的尝试只在日志里。
- 历史文件与状态文件同目录（`/root`），换机器迁移时两个文件要一起带走。
