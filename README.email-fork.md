# ddns-go 邮件通知分支（IP 变更提醒）

基于 [jeessy2/ddns-go](https://github.com/jeessy2/ddns-go) **v6.17.7**（commit `7aad574`）的定制分支，
新增「公网 IP 变更时发送邮件提醒」功能。其余行为与原版一致。

- 源码：`/home/mist/work/ddns-go`（分支 `main`，以仓库当前 HEAD 为准）
- 镜像：`ddns-go-email:6.17.7-email-0.13`（约 40 MB，多阶段构建）
- 测试：`/home/mist/work/test`（假 SMTP + 假回调，可随时重跑）

> **不要使用 `-u` 参数。** 它会从上游 GitHub releases 下载官方二进制覆盖自身，
> 邮件通知功能会随之丢失，且不可逆（需要重新拉镜像）。同理不要用 `-s install` 在本镜像内装服务。

## 与原版的差异

| 文件 | 改动 |
|---|---|
| `config/email.go` | 新增：`Email` 配置结构、SMTP 投递、邮件模板、IP 变更状态持久化 |
| `config/config.go` | `Config` 增加 `Email Email` 字段；新增 `ResetConfigCache()`（测试用） |
| `dns/index.go` | 每轮收集检测到的 IP，循环结束后调用 `NotifyIPChangeEmail` |
| `web/save.go` | 解析并保存邮件配置；SMTP 密码留空表示保持原值 |
| `web/writing.go` + `web/writing.html` | 新增「邮件通知」配置区与「发送测试邮件」按钮 |
| `web/emailTest.go` | 新增 `/emailTest` 接口（限频 5 秒，且仅在目标与已保存配置一致时复用已存密码） |
| `web/emailStatus.go` | 新增 `/emailStatus`（状态面板数据）、`/emailHistoryClear`（清空历史，仅 POST） |
| `config/email_history.go` | 通知历史（JSONL，最多 200 条） |
| `main.go` | 注册 `/emailTest` / `/emailStatus` / `/emailHistoryClear` 路由；`-f` 检测间隔默认值 300 → **600 秒** |
| `static/i18n.js` / `util/messages.go` | 中英文文案 |
| `Dockerfile.email` | 多阶段构建的镜像定义，CMD 默认 `-f 600` |
| `config/email_test.go` / `web/email_test.go` | 单元测试 |

## 行为语义

1. 每轮检测后，把本轮 IPv4/IPv6 与「上次成功通知的 IP」比较。多个配置时只用**主地址**（按配置顺序第一个取到的值）参与比较，避免某个源抖动导致拼接串变化而误报。
2. **只有真正变化时才发信**；IP 未变的轮次完全静默。
3. 首次运行（无历史记录）：
   - `onstart: false`（默认）→ 只记录基线，不发信；
   - `onstart: true` → 发一封基线邮件。
4. 发送失败时**不更新状态**，下一轮自动重试，不会丢事件；
   但只有认证机制不被接受时才回退 `AUTH LOGIN`，且 `DATA` 已被接收后 `QUIT` 失败不会重发。
5. 某一轮取不到地址（瞬时失败）时，该地址不参与比对，也不会覆盖历史值，避免"掉线"被误报成"IP 变更"；但**任一**启用了地址族的配置持续取不到地址，会计入告警（多配置里坏一个也会报）。
6. 临时/已弃用的 IPv6（SLAAC 隐私地址）会被忽略，不会因为地址按小时轮换而反复发信。
7. 状态文件与配置文件同目录：`<配置文件路径>.email-state.json`，随挂载卷持久化；
   写入采用「临时文件 + rename」原子替换，写盘失败时自动降级到内存状态，**不会重复轰炸**。
8. 单次 SMTP 会话有 45 秒硬超时（连接阶段另有 15 秒超时），服务器半死时不会无限卡住。发信是**同步**的，因此这一轮 DNS 检测会被阻塞：
   单次 `Send` 最坏要建立两个会话（PLAIN 被拒后回退 LOGIN）= 约 90 秒，一轮里若同时要发告警和变更邮件则约 180 秒。这是已知上限，不是无限等待。
9. 开启「获取失败时告警」后，连续 3 轮取不到 IP 发一封告警，恢复后再发一封恢复通知；
   同一次故障只告警一次，恢复通知失败会保留待发送状态并在下一轮重试。
10. 每次成功通知都会追加一条历史记录：`<配置文件路径>.email-history.jsonl`（最多保留 200 条），
    网页「历史记录」区块展示上次通知时间、当前保留条数和最近 10 条。
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
  ddns-go-email:6.17.7-email-0.13 -l :9876 -f 120
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

`encrypt` 留空时按端口自动推断（465 → ssl，587 → starttls，其它端口 → none；页面下拉的默认选项与此一致）；网页上切换端口会自动带出常用值。
认证先尝试 `AUTH PLAIN`，**只有认证阶段失败**才回退 `AUTH LOGIN`。
`none` + 非 localhost + 配置了 SMTP 账号时，程序会直接拒绝发送，避免密码明文上网；如确需内网明文认证，请自行改代码或使用 localhost 转发。

## 构建

**源码分发**：本 fork 没有公开托管的仓库，源码以 `git bundle` 形式提供（包含完整提交历史）：

```bash
git clone ddns-go-email-YYYYMMDD.bundle ddns-go
cd ddns-go && git log --oneline -1        # 应看到对应的版本提交
```


```bash
cd /home/mist/work/ddns-go
sudo docker build -f Dockerfile.email -t ddns-go-email:6.17.7-email-0.13 .
```

## 运行

```bash
sudo docker run -d --name ddns-go-email --restart unless-stopped \
  -p 9876:9876 \
  -v /opt/ddns-go:/root \
  ddns-go-email:6.17.7-email-0.13
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

### 6.17.7-email-0.13

按**功能级端到端复核**（真镜像 + 假 IP 接口 + 假 SMTP，覆盖基线/去重/变更/重启/告警/恢复/双源/退避）修的两处静默失效，外加一轮可用性改进。

| 级别 | 问题 | 修复 |
|---|---|---|
| P2 | **多配置源同一轮变更只通知第一个源**：邮件取 `changes[0]`，但循环里对**每个源**都推进了基线。实测两源同时变更只收到 1 封，而状态文件里两个源的基线都已更新——第二个源的变更**永久丢失**，正文也不说明是哪个源变的 | 把本轮所有变更汇总成一封，正文按「配置: <源名>」逐条列出；历史记录也按源逐条写入。仍保证「每轮最多一封」 |
| P2 | **退避期没有任何解除路径**：SMTP 配错一次后，改好配置、点测试发信成功、甚至重启容器，真实通知仍被静默压制 10~60 分钟，且无日志、无界面提示 | ① 保存配置时若 SMTP 字段有变化，立即 `ResetEmailBackoff()`；②「发送测试邮件」成功后同样解除；③ 退避首次生效时打一条日志；④ 状态面板显示退避倒计时 |
| P3 | 恢复通知与变更通知会**连发两封**：故障期间 IP 变了，恢复轮发「已恢复」，下一轮又发「IP 变更: 旧 → 新」 | 恢复邮件合并「故障期间检测到的变更」，发信成功后推进基线，不再补发 |
| P3 | 邮件缺 `Message-ID` 头，是常见的垃圾邮件评分项（QQ/163/Outlook 更容易判垃圾） | 补 `Message-ID: <纳秒.进程.序号@主机名>`，主机名做字符白名单清洗 |
| P3 | `RunOnceAsync` 在已有任务排队时**直接丢弃**新触发，保存配置后可能要等下一个定时周期（默认 600 秒）才生效 | 改为排队计数（1 个在跑 + 1 个在等），再多才合并 |
| P3 | 历史记录不记来源，多配置时分不清是哪个源变的 | `EmailHistoryEntry` 增加 `source` 字段（旧文件缺该字段可正常解析），面板历史列表显示 |

**文档补充**：必须挂载 `/root` **目录**而不是单个配置文件。只读挂载单文件时状态写不进去，`OnStart` 开启时每次重启都会重发一封基线邮件。

新增测试：`TestComposeIPChangeMailMultiSource`、`TestComposeIPChangeMailMultiSourceEN`、`TestBuildMessageIDIsValidAndUnique`、`TestResetEmailBackoffClearsState`、`TestGetEmailBackoffUntil`、`TestComposeRecoveredMailMentionsChanges`。

已知未处理：配置源改名后旧状态键不清理（0.10 起既有问题）；SMTP 密码仍以明文存于配置文件（0600，协议所限）。

### 6.17.7-email-0.12

针对"长期运行"做的边界核验（soak、资源泄漏、状态文件健壮性、并发、输入边界）后修复的两处隐患。

| 级别 | 问题 | 修复 |
|---|---|---|
| P1 | `web.MemoryLogs` 对裸切片做 append / 重新切片，**完全没有锁**。生产环境里后台 `RunOnce` 与 Web 请求处理器都会经 `log.Println` 并发写入，`go test -race` 直接报 DATA RACE；裁剪用的 `Logs[len-MaxNum:]` 在一次并发 append 之后还可能越界 panic，而 panic 若发生在 RunOnce 的 goroutine 里会终止整个进程 | 加 `sync.Mutex`，新增 `Snapshot()` / `Clear()` 方法；裁剪改为复制新切片，避免底层数组被无限向前推进 |
| P2 | `sendMail` 无条件信任状态文件里的 `nextRetryAt`：一旦它落在很远的未来（系统时钟被大幅回拨、状态文件被改坏或异常写入），此后**每一次发信都静默跳过且没有任何日志**，邮件通知等于永久失效，但进程看起来一切正常 | 超过退避上限（60 分钟）两倍即判定异常，重置 `nextRetryAt` / `failCount` 并打日志自愈；`failCount` 为负数或离谱大值时同样重置 |

核验后**未发现**问题的部分（可作为长期运行的依据）：

- 400 轮连续发信 soak：堆增长约 0，文件句柄 8→8，goroutine 3→3，无泄漏
- SMTP 持续失败 200 轮：句柄不增长
- 历史文件追加 1200 条后稳定在 200 行 / 约 10.8 KB
- 20 000 个源键的状态文件（738 KB）可正常加载
- 500 个收件人、200 个域名：邮件 5.3 KB，无超长行（SMTP 行上限 1000）
- 200 个源同轮全部变更：只发 1 封，符合"每轮最多一封"
- 状态文件被截断、或并发写入 180 次后，仍是合法 JSON 且不 panic
- 端口/加密方式等数值边界：`70000`、`-5`、`bogus` 均被正确拒绝

新增测试：`TestMemoryLogsConcurrentSafety`（需 `-race`）、`TestEmailStateFarFutureRetrySelfHeals`。

已知未处理：配置源改名后旧键不清理（每次改名净增 1 个键、约 30 字节，长期极缓慢累积）；SMTP 密码以明文存于配置文件（0600，协议所限）。

### 6.17.7-email-0.11

独立复检（加对照组 + 源码插桩，在一次性容器里跑）后修复的三类缺陷。**0.10 的镜像保留**，可随时回退对比。

| 级别 | 问题 | 修复 |
|---|---|---|
| P2 | 变更快照**按地址族各追加一次**：IPv4 那一条的 IPv6 字段还是空的，被 `changes[0]` 取走后，**只要 IPv4 变更，邮件里的 IPv6 恒显示 `-`**；双栈同轮变更时，IPv6 的变更被整条丢弃，而基线已经推进，事件永久丢失 | 每个源只构造**一条** `sourceChange`，等 IPv4/IPv6 都填好后再判断是否变更、只追加一次 |
| P2 | 发信成功后 `storeEmailState(next)` 写回的是**发信前**的旧快照，回滚了本轮更早落盘的 `failCount` / `nextRetryAt` / `alerted` / `lastSent` | 新增 `commitEmailState()`：以**当前状态**为基座，只覆盖调用方拥有的 `sources` / `lastIPv4` / `lastIPv6` / `lastSent` |
| P2 | 同上根因：发信成功后 `failCount` 不清零，累计失败 3 次后**退避等级被永久抬到 60 分钟**，文档承诺的"初始 10 分钟"再也不生效 | 同上 |
| P2 | 同上根因：告警同轮时落盘 `alerted=false`，容器在故障期间重启后会**重复告警**，且**恢复通知永远发不出去** | 同上 |
| P2 | 告警同轮压制变更邮件时**顺手推进了基线**，这次变更之后再也不会被检测到（与"每轮最多一封"的本意不符） | 改为**顺延**：不写状态直接返回，下一轮重新检测到同一变更再补发；仍保证每轮最多一封 |
| P3 | 关闭"获取失败告警"时只清了内存 tracker，未重置 `fetchTrackerLoaded`，关掉再打开会用空 tracker 覆盖磁盘上"已告警"的记录 | 关闭时一并把 `fetchTrackerLoaded` 置 false，下次启用重新从状态文件恢复 |
| P3 | `/emailTest` 的 5 秒限频名额在**参数校验之前**消耗，端口填错也要等 5 秒才能重试 | 名额改到校验通过、真正发信之前才占用 |
| P3 | `/emailTest`、`/emailStatus` 未校验 HTTP 方法 | 分别限定 POST / GET，非匹配方法返回 405 |
| P3 | `readEmailHistoryLines` 里 `var reader io.Reader = f` + `_ = reader` 是重构残留 | 删除死代码（尾部跳读逻辑保持不变） |

行为变化（升级时注意）：告警/恢复同轮发生的 IP 变更，过去是"静默推进基线、事件丢弃"，现在是"顺延到下一轮补发"。

新增测试（`config/email_regression_test.go`）：`TestEmailDualFamilyChangeReportsBoth`、`TestEmailIPv4ChangeKeepsIPv6InMail`、`TestEmailAlertAndChangePersistState`、`TestEmailChangeDeferredNotDropped`、`TestEmailRecoveryNoticeSurvivesRestart`、`TestEmailNoDuplicateAlertAfterRestart`、`TestEmailFailCountResetsAfterSuccess`。

已知未处理：状态文件里已消失配置源的键不会主动清理（改配置名后旧键会残留，重新加回同名配置时可能拿过期基线比对，最多多发一封变更邮件）；SMTP 密码仍以明文存于配置文件（权限 0600，协议所限）。

### 6.17.7-email-0.10

按"0.9 复盘 + 卡死场景排查"的结果修复。**0.8 / 0.9 的 tag 都保留**，可随时回退对比。

| 级别 | 问题 | 修复 |
|---|---|---|
| P1 | 0.9 的"主地址"方案只解决了"第二个源抖动"，**第一个源**抖动时主地址会变成第二个源的 IP，仍然发假变更邮件（实测 1→2→3 封） | 状态文件改为**按源记录基线**（`sources: {"A|v4": "1.1.1.1"}`）。每个源只和自己的上次值比；某源本轮取不到地址时不动它的基线；新出现的源静默记录基线。实测两个方向的抖动都不再发信，只有地址真的变化才发 |
| P2 | SMTP 配错时会每轮重试，每次都阻塞约 90 秒，把 DNS 更新周期从 600 秒拖到约 780 秒 | 发信失败进入**指数退避**：失败 1 次退避 10 分钟，连续 3 次退避 60 分钟；成功后清零。状态记录在 `failCount` / `nextRetryAt` |
| P2 | 一轮里可能同时发"恢复通知"和"变更邮件"（两封） | 每轮**最多一封**：本轮已发告警/恢复时不再追发变更邮件；同时把新的基线静默持久化，避免下一轮补发 |
| P2 | `save.go` 每次保存都 `go dns.RunOnce()`，SMTP 慢时连点保存会堆积 goroutine | 新增 `dns.RunOnceAsync()`，已有排队任务时直接跳过 |
| P2 | 历史摘要每次轮询全量读 `.email-history.jsonl`；`scanner.Err()` 未检查，超长行会静默丢历史 | 文件超过 256 KB 时只读尾部 128 KB；补上错误日志 |

行为变化（升级时注意）：恢复通知若发送失败，不再"下一轮立刻重试"，而是等退避期满（默认 10 分钟）；`alerted` 状态仍会保留，通知不会丢。

新增测试：`TestNotifyPerSourceNoFalseAlarm`（按源不误报）、`TestSendBackoffAfterFailure`（退避生效）。

### 6.17.7-email-0.9

按完整代码审计修复。**0.8 的 tag 保留**（同仓库同时存在 0.8 与 0.9，便于回退对比）。

| 编号 | 问题 | 修复 |
|---|---|---|
| P1 | 多配置时各源 IP 被拼成 `"a,b"` 参与比较，任一源瞬时失败就发假"IP 变更"邮件（实测 16 秒两封） | 邮件比对改用**主地址**（配置顺序第一个非空值），完整列表仅用于页面展示 |
| P1 | `util.ForceCompareGlobal` 是裸 bool，HTTP goroutine 与 RunOnce 并发读写（`-race` 实测报 DATA RACE） | 改为 `atomic.Bool`，RunOnce 入口 `Swap(false)` 取快照，配置读取失败时归还标志 |
| P1 | 「清除已保存的密码」在"先输新密码再点清除"或"保存过之后"会静默失效，且明文密码残留输入框 | 点清除时同步清空输入框与内存值；保存成功后也清空 |
| P1 | 告警/恢复状态只在内存，容器重启后恢复通知永久丢失 | `alerted` 落盘到状态文件，进程启动后恢复 |
| P1 | 多配置里单个配置持续取不到 IP 永远不会告警（`ipv4Got` 是"任一成功"） | 改为按配置粒度统计，任一期望配置失败即计入 |
| P1 | 配置文件用 `os.WriteFile` 直接覆盖，断电/磁盘满会留下半截 YAML（启动即失败） | 改为 临时文件 + `Sync` + `rename` 原子写 |
| P1 | SMTP 服务器未宣告 AUTH 时静默跳过认证，凭据被丢弃且报错误导 | 直接返回明确错误 |
| P2 | `/emailHistoryClear`、`/clearLog` 不校验方法，跨站 GET 可清空历史/日志 | 非 POST 返回 405 |
| P2 | `/emailTest` 会把已保存密码发往请求指定的任意 SMTP 主机，且无频率限制 | 仅当 host+port+账号与已保存配置一致时复用密码；5 秒限频 |
| P2 | 状态请求失败的错误提示 1 秒内被旧缓存覆盖，过期数据当最新 | 记录错误态，错误期间暂停缓存重绘，错误用 `textContent` 渲染 |
| P2 | `lastRunAt` 记的是本轮**开始**时间（`defer` 实参立即求值），"上次检测"偏移 | 改用函数字面量，记录完成时间 |
| P2 | 保存触发的立即执行会让"下次检测"倒计时虚高一个周期 | 由定时器维护 `nextRunAt`，前端优先用它 |
| P2 | 邮件输入框里按回车会触发「模拟测试 Webhook」（`webhookTestBtn` 缺 `type`） | 补 `type="button"` |
| P2 | 邮件关闭时非法 SMTP 端口错误被静默吞掉，界面显示值与实际保存值不一致 | 只要填了端口就校验并报错 |
| P2 | 同一次请求可把密码强度阈值从 30bit 降到 25bit | 用保存前的值判定，取更严格的结果 |
| P2 | 首轮基线 IP 被标成「上次通知的 IP」 | 接口返回 `hasNotified`，未发过信时显示「基线 IP（尚未通知）」 |
| P2 | 历史摘要的 `TimeText/KindText/LastTime/lang` 全是死代码，前端另做一套且文案分叉 | 删除死字段与两个格式化函数，展示统一由前端按当前语言渲染 |
| P2 | 后端校验类错误硬编码中文，英文界面下仍是中文 | 22 条错误文案接入 `util.LogStr` + `messages.go` 英文翻译 |
| P2 | 两条日志翻译因格式串不匹配永不生效（全角逗号、`%v` vs `%s`） | 修正键 |
| P2 | 3 个 i18n 死键；深色模式 `.text-muted`/`.text-warning` 对比度不足（约 1.6:1 / 3:1） | 删除死键；新增深浅两套配色覆盖 |
| P2 | Dockerfile 缺 HEALTHCHECK | 增加 `HEALTHCHECK`（探 `/login`） |

新增测试：`TestPrimaryAddr`、`TestUniqueAddr`、`TestCheckPasswordWithLimit`、`TestAlertedStatePersists`、`TestSaveConfigAtomic`、`TestEmailHistoryClearRejectsGet`、`TestSameSMTPHost`。

验证：`go vet` 干净、`go test ./...` 全绿、`go test -race ./config ./web ./dns` 无竞态报告；多配置误报复现脚本改造后不再出现假变更邮件。

### 6.17.7-email-0.8

纯镜像元数据修正，**代码与运行时行为与 0.7 完全一致**：

| 改动 | 说明 |
|---|---|
| 移除 `org.opencontainers.image.source` | 该字段的约定是"源码仓库地址"，而本 fork 源码没有公开托管。之前指向一个空仓库，属误导 |
| 新增 `org.opencontainers.image.url` | 指向镜像页面 `https://hub.docker.com/r/ivanzzffii/ddns-go-email`，作为项目信息入口 |
| 新增 `org.opencontainers.image.base.name` | 声明基础镜像 `docker.io/library/alpine:3.24`，便于追溯底层系统 |
| 其余标签不变 | `title` / `description` / `version` / `revision` / `licenses` 保持原样 |

源码分发方式见「构建」一节：本仓库不公开托管，以 `git bundle` 离线分发。

### 6.17.7-email-0.7

按外部审查报告修复，均为可验证的具体缺陷：

| 问题 | 修复 |
|---|---|
| 同时勾选「清除已保存密码」又填新密码时，新密码被丢弃 | 改为新密码优先；前端输入新密码即撤销清除标记 |
| 保存配置失败仍触发一轮检测（可能发信、改 DNS） | 保存失败直接返回，不再触发 RunOnce |
| 告警/恢复通知不更新「上次通知」时间 | 发送成功后同步更新时间戳 |
| SMTP AUTH 回退在密码错误时重复认证，可能触发邮箱账号锁定 | 仅在服务端「不支持该认证机制」（500/501/504/534）时回退 LOGIN；535 等凭据错误直接失败 |
| 状态面板把「上次成功通知的 IP」标成「当前 IP」 | 拆成「本轮检测 IP」（`dns.LastDetected`）与「上次通知的 IP」 |
| 保存后状态面板不刷新 | 保存成功后立即刷新一次 |
| 密码框 placeholder 硬编码中文 | 交由前端按当前语言渲染 |
| 文档与实现不一致 | 修正超时描述（45 秒上限但会阻塞本轮）、历史条数（10 条）等 |
| 基础镜像 Alpine 3.21 于 2026-11-01 EOL | 升级到 **Alpine 3.24**（与上游一致） |
| 镜像缺少来源元数据 | 增加 OCI labels（version/revision/source/license），构建时传入提交号 |

新增测试：`TestSaveNewPasswordWinsOverClear`、`TestSaveFailureSkipsRunOnce`、`TestAuthMechanismUnsupported`。

### 6.17.7-email-0.6

修复「配置切换」里删除配置的逻辑问题（纯前端，后端无改动）：

| 问题 | 表现 | 修复 |
|---|---|---|
| 删除第一个配置会误触发"新增" | 删除后只往前找可用配置，删索引 0 时走到 -1，落到 `else` 分支调用 `addBtn.click()`，结果凭空多出一个空白配置、选中项跳到末尾 | 改为**优先选后一个**有效配置，没有再往前找；全部删光时清空数组与下拉，重新给一个空白配置 |
| 没有二次确认 | 点一下立即生效，误点后若顺手保存则配置真的丢失 | 删除前弹确认框，提示"保存后才生效，未保存前刷新页面可撤销" |
| `getConfName()` 空值异常 | 删除后该槽位是 `null`，再调用会读 `.DnsName` 抛 TypeError | 空槽位返回 `"N - 已删除"` 占位 |

验证方式：用 Node 仿真四种删除场景（删第一个/中间/最后/唯一一个），再抓真实页面注入点击脚本、用无头浏览器渲染确认数组、下拉项与选中索引。

### 6.17.7-email-0.5

| 改动 | 说明 |
|---|---|
| **修复日志面板位置** | `common.css` 里的固定定位规则写的是 `.col-md-6.logs-panel`，0.4 把列宽改成 `col-md-10` 后选择器不再匹配，日志面板退回文档流、必须滚到页面最底部才能看到。现在日志面板是**居中的固定浮层**（顶部 8vh、最高 80vh），`#mask` 也改为覆盖整个视口 |
| **默认语言改为中文** | 配置里没写 `lang` 时，`GetConfigCached` 会把语言落为 `zh`——界面文案和运行日志都以中文呈现。（之前空语言会退回英文；注意 `CompatibleConfig` 改的是值拷贝，所以要在缓存实例上设置） |
| **新增「清除已保存的密码」** | SMTP 密码一直只能"留空=不修改"，想删掉只能改 YAML。现在密码已设置时会出现「清除已保存的密码」按钮，保存后生效（`EmailPasswordClear`） |

新增测试：`TestEmptyLangDefaultsToChinese`、`TestSaveCanClearEmailPassword`。

### 6.17.7-email-0.4

以 Web 界面为主的一次优化：

| 改动 | 说明 |
|---|---|
| **新增运行状态面板** | 页面上直接显示当前检测到的 IP、上次检测时间、下次检测倒计时、上次通知时间、历史记录条数；数据由新接口 `/emailStatus` 每 5 秒刷新，倒计时每秒更新 |
| **历史记录可视化** | 变更/基线/失败/恢复四种类型用彩色徽章区分，显示时间与"旧 IP → 新 IP"；新增「刷新」与「清空历史」（`/emailHistoryClear`） |
| **测试邮件结果留痕** | 点完「发送测试邮件」后按钮下方保留"上次测试：成功/失败 · 时间"，不再依赖 3 秒就消失的提示条 |
| **错误提示净化** | 接口异常时不再把整段响应体（可能是 HTML 错误页）塞进提示条：自动提取标题、去标签、截断到 200 字 |
| **布局加宽** | 内容区从 `col-md-6` 改为 `col-md-10`，标签列从 `col-sm-2` 加宽到 `col-sm-3`；宽屏下不再折行，整页高度从 3764px 降到约 3680px 且信息密度更高 |
| 后端 | 新增 `dns.LastRunAt()/SetInterval()` 记录检测节奏；`config.GetEmailStateSnapshot()`、`config.ClearEmailHistory()` |

新增测试：`TestClearEmailHistory`、`TestGetEmailStateSnapshot`、`TestEmailStatusEndpoint`。

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
| **新增 IP 变更历史** | 每次成功通知追加一行到 `<配置>.email-history.jsonl`，最多保留 200 条；网页「历史记录」区块展示上次通知时间、当前保留条数与最近 10 条 |
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
