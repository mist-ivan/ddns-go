package config

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeessy2/ddns-go/v6/util"
)

// Email 邮件通知配置。随 Config 一起序列化到 ddns-go 的 yaml 配置文件。
type Email struct {
	// Enable 是否启用邮件通知
	Enable bool
	// SMTPHost SMTP 服务器地址，如 smtp.qq.com
	SMTPHost string
	// SMTPPort SMTP 端口，465/587/25
	SMTPPort int
	// Username SMTP 登录账号，留空表示匿名发送
	Username string
	// Password SMTP 密码或授权码
	Password string
	// From 发件人地址，留空时使用 Username
	From string
	// To 收件人，多个用逗号或分号分隔
	To string
	// Encrypt 加密方式：ssl(隐式 TLS) / starttls / none
	Encrypt string
	// OnStart 首次运行（没有历史 IP 记录）时也发送一封基线邮件
	OnStart bool
	// AlertOnFetchFailure 连续多轮取不到公网 IP 时发送告警邮件
	AlertOnFetchFailure bool
}

// FailedSource 本轮取不到地址的配置源，用于告警邮件定位。
type FailedSource struct {
	Key      string
	Label    string
	Domains  []string
	Families []string
}

// FetchStatus 本轮公网 IP 的期望与获取结果。
// 分开记录 IPv4/IPv6，避免“只启用/只配置了一族”时误报整个公网 IP 获取失败。
type FetchStatus struct {
	IPv4Expected bool
	IPv4OK       bool
	IPv6Expected bool
	IPv6OK       bool
	// FailedSources 是可选的明细，供告警正文说明具体哪个配置源失败。
	FailedSources []FailedSource
	// ConfiguredSources 是本轮仍然有效的配置源 Key 集合（用于兼容旧状态和旧测试）。
	ConfiguredSources []string

	// 下面是 0.17 起的精确状态：键形如 "sourceKey|IPv4" / "sourceKey|IPv6"。
	// 只做“有没有失败”的聚合判断，无法知道是哪个源/哪个族的新故障，恢复时
	// 也无法确认原失败项是否真的仍启用并已恢复。旧调用方不填这些字段时，
	// 下面的方法会回退到 IPv4Expected/IPv4OK 和 FailedSources 的族级信息。
	FailedFamilyKeys     []string
	ConfiguredFamilyKeys []string
	OKFamilyKeys         []string
}

// Failed 是否至少有一个已配置的地址族在本轮获取失败
func (s FetchStatus) Failed() bool {
	return len(s.failureKeys()) > 0
}

// legacyFailedFamilies 兼容只填 IPv4Expected/IPv4OK 的旧调用方。
func (s FetchStatus) legacyFailedFamilies() []string {
	var out []string
	if s.IPv4Expected && !s.IPv4OK {
		out = append(out, "IPv4")
	}
	if s.IPv6Expected && !s.IPv6OK {
		out = append(out, "IPv6")
	}
	return out
}

func (s FetchStatus) failedFamilies() []string {
	return uniqueFamiliesFromKeys(s.failureKeys())
}

func (s FetchStatus) expectedFamilies() []string {
	var out []string
	if s.IPv4Expected {
		out = append(out, "IPv4")
	}
	if s.IPv6Expected {
		out = append(out, "IPv6")
	}
	return out
}

func canonicalFamily(family string) string {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case "ipv4":
		return "IPv4"
	case "ipv6":
		return "IPv6"
	default:
		return ""
	}
}

// failureKey 生成状态机内部使用的 "sourceKey|IPv4" 键。sourceKey 为空时
// 退化为族级键，兼容只有聚合状态的旧调用方。
func failureKey(source, family string) string {
	source = strings.TrimSpace(source)
	family = canonicalFamily(family)
	if family == "" {
		return ""
	}
	if source == "" {
		return family
	}
	return source + "|" + family
}

func splitFailureKey(key string) (source, family string) {
	key = strings.TrimSpace(key)
	if i := strings.LastIndex(key, "|"); i >= 0 {
		return key[:i], canonicalFamily(key[i+1:])
	}
	return "", canonicalFamily(key)
}

func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

func mergeFailureKeys(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return sortedUnique(out)
}

// failureKeys 返回本轮精确的失败项。优先使用 FailedFamilyKeys；否则从
// FailedSources 构造；再否则回退到族级聚合状态。
func (s FetchStatus) failureKeys() []string {
	if len(s.FailedFamilyKeys) > 0 {
		return sortedUnique(s.FailedFamilyKeys)
	}
	var out []string
	for _, src := range s.FailedSources {
		for _, fam := range src.Families {
			if key := failureKey(src.Key, fam); key != "" {
				out = append(out, key)
			}
		}
	}
	if len(out) > 0 {
		return sortedUnique(out)
	}
	return s.legacyFailedFamilies()
}

// configuredFamilyKeys 返回本轮仍启用/仍有效的 source|family 集合。
func (s FetchStatus) configuredFamilyKeys() []string {
	if len(s.ConfiguredFamilyKeys) > 0 {
		return sortedUnique(s.ConfiguredFamilyKeys)
	}
	var out []string
	if len(s.ConfiguredSources) > 0 {
		for _, src := range s.ConfiguredSources {
			if s.IPv4Expected {
				if key := failureKey(src, "IPv4"); key != "" {
					out = append(out, key)
				}
			}
			if s.IPv6Expected {
				if key := failureKey(src, "IPv6"); key != "" {
					out = append(out, key)
				}
			}
		}
	} else {
		out = s.expectedFamilies()
	}
	return sortedUnique(out)
}

// okFamilyKeys 返回本轮成功获取地址的 source|family 集合。
func (s FetchStatus) okFamilyKeys() []string {
	if len(s.OKFamilyKeys) > 0 {
		return sortedUnique(s.OKFamilyKeys)
	}
	var out []string
	if len(s.ConfiguredSources) > 0 {
		for _, src := range s.ConfiguredSources {
			if s.IPv4Expected && s.IPv4OK {
				if key := failureKey(src, "IPv4"); key != "" {
					out = append(out, key)
				}
			}
			if s.IPv6Expected && s.IPv6OK {
				if key := failureKey(src, "IPv6"); key != "" {
					out = append(out, key)
				}
			}
		}
	} else {
		if s.IPv4Expected && s.IPv4OK {
			out = append(out, "IPv4")
		}
		if s.IPv6Expected && s.IPv6OK {
			out = append(out, "IPv6")
		}
	}
	return sortedUnique(out)
}

func familyExpected(status FetchStatus, family string) bool {
	switch canonicalFamily(family) {
	case "IPv4":
		return status.IPv4Expected
	case "IPv6":
		return status.IPv6Expected
	default:
		return false
	}
}

func familyOK(status FetchStatus, family string) bool {
	switch canonicalFamily(family) {
	case "IPv4":
		return status.IPv4Expected && status.IPv4OK
	case "IPv6":
		return status.IPv6Expected && status.IPv6OK
	default:
		return false
	}
}

// uniqueFamiliesFromKeys 从 source|family 键中提取唯一的地址族，并按 IPv4、
// IPv6 的固定顺序输出，避免邮件/状态里的顺序随 map 遍历变化。
func uniqueFamiliesFromKeys(keys []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, key := range keys {
		_, family := splitFailureKey(key)
		if family == "" || seen[family] {
			continue
		}
		seen[family] = true
		out = append(out, family)
	}
	sort.Slice(out, func(i, j int) bool {
		rank := func(f string) int {
			if f == "IPv4" {
				return 0
			}
			return 1
		}
		return rank(out[i]) < rank(out[j])
	})
	return out
}

func sourcesFromFailureKeys(keys []string) []string {
	var out []string
	for _, key := range keys {
		source, _ := splitFailureKey(key)
		if source != "" {
			out = append(out, source)
		}
	}
	return sortedUnique(out)
}

// failureKeysFromLegacy 把旧状态里的“族列表 + 源列表”转换成 0.17 的精确键。
// 旧状态没有保存源和族的对应关系，只能做笛卡尔组合；这是迁移兼容路径，
// 新告警不会再写入这种模糊状态。
func failureKeysFromLegacy(families, sources []string) []string {
	var fams []string
	for _, family := range families {
		if family = canonicalFamily(family); family != "" {
			fams = append(fams, family)
		}
	}
	fams = sortedUnique(fams)
	if len(fams) == 0 {
		return nil
	}
	srcs := sortedUnique(sources)
	if len(srcs) == 0 {
		return fams
	}
	out := make([]string, 0, len(fams)*len(srcs))
	for _, src := range srcs {
		for _, family := range fams {
			if key := failureKey(src, family); key != "" {
				out = append(out, key)
			}
		}
	}
	return sortedUnique(out)
}

// failureKeyCovered 判断当前失败项是否已被已告警集合覆盖。旧状态里的族级键
// 可以覆盖同族的 source|family 键，用于升级迁移时避免重复告警。
func failureKeyCovered(previous []string, current string) bool {
	for _, old := range previous {
		if old == current {
			return true
		}
		oldSource, oldFamily := splitFailureKey(old)
		_, currentFamily := splitFailureKey(current)
		if oldSource == "" && oldFamily != "" && oldFamily == currentFamily {
			return true
		}
	}
	return false
}

func hasNewFailure(previous, current []string) bool {
	for _, key := range current {
		if !failureKeyCovered(previous, key) {
			return true
		}
	}
	return false
}

// recoverableFailureKeys 只返回“原来失败、现在仍启用、现在确实成功”的项。
// 原失败源删掉/改名、或只关闭了出问题的那一族时，对应键不会出现在这里。
func recoverableFailureKeys(previous []string, status FetchStatus) []string {
	configured := make(map[string]bool)
	for _, key := range status.configuredFamilyKeys() {
		configured[key] = true
	}
	ok := make(map[string]bool)
	for _, key := range status.okFamilyKeys() {
		ok[key] = true
	}

	var out []string
	for _, key := range previous {
		source, family := splitFailureKey(key)
		if family == "" {
			continue
		}
		if source == "" {
			// 旧状态只有族级信息：至少要求该族仍启用且成功。
			if familyExpected(status, family) && familyOK(status, family) {
				out = append(out, family)
			}
			continue
		}
		normalized := failureKey(source, family)
		if configured[normalized] && ok[normalized] {
			out = append(out, normalized)
		}
	}
	return sortedUnique(out)
}

// smtpSessionTimeout 单次 SMTP 会话的总超时（从连接建立起算到会话结束）
var smtpSessionTimeout = 45 * time.Second

// emailState 记录上次成功通知过的 IP，用于判断"IP 是否真的变了"
type emailState struct {
	// Schema 状态文件结构版本。0.14 起写入 2；0.17 起写入 3。
	// 旧文件没有该字段，用于一次性迁移 ChangeNotified 的含义。
	Schema int `json:"schema,omitempty"`

	LastIPv4 string `json:"lastIPv4"`
	LastIPv6 string `json:"lastIPv6"`
	LastSent int64  `json:"lastSent"`
	// ChangeNotified 表示是否真的发出过“基线/变更”邮件。
	// 不能复用 LastSent：告警/恢复也会更新 LastSent，但并没有通知过 IP。
	ChangeNotified bool `json:"changeNotified,omitempty"`
	// Alerted 表示当前这轮故障已经发过告警。
	// 必须落盘：只在内存里的话，容器重启后恢复通知就再也不会发（告警邮件里承诺过会发）。
	Alerted bool `json:"alerted,omitempty"`
	// AlertedFamilies 记录告警时失败的地址族（IPv4/IPv6）。
	// 恢复时必须只针对“仍然启用且确实恢复”的原失败族，不能仅凭当前没有失败
	// 就发恢复邮件：用户可能把失败的地址族/全部任务删掉了。
	AlertedFamilies []string `json:"alertedFamilies,omitempty"`
	// AlertedSources 记录告警时失败的配置源 Key；用于识别“原失败源被删除、
	// 同地址族的另一个源仍然健康”这种不应发送恢复通知的情况。
	AlertedSources []string `json:"alertedSources,omitempty"`
	// AlertedFailureKeys 是 0.17 起的精确告警集合，元素形如
	// "sourceKey|IPv4"；恢复时逐项确认该源/该族仍启用且已成功。
	// 旧状态没有这个字段时，由 AlertedFamilies/AlertedSources 迁移生成。
	AlertedFailureKeys []string `json:"alertedFailureKeys,omitempty"`

	// Sources 按"配置源"记录上次见过的地址（键形如 "家里群晖|v4"）。
	// 单独记每个源，某个源本轮取不到地址时不动它的基线，避免"某个源抖动"被当成 IP 变更。
	Sources map[string]string `json:"sources,omitempty"`

	// FailCount / NextRetryAt 用于发信失败的分级退避：
	// 失败 1-2 次退避 10 分钟，3 次及以后退避 60 分钟；成功后清零。
	// SMTP 配错时不再每轮都重试（每次重试最多阻塞约 90 秒）。
	FailCount   int   `json:"failCount,omitempty"`
	NextRetryAt int64 `json:"nextRetryAt,omitempty"`
}

const emailStateSchema = 3

// loginAuth 实现 SMTP AUTH LOGIN，部分邮件服务商只支持该方式
type loginAuth struct {
	username string
	password string
}

func (a *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	challenge := strings.ToLower(strings.TrimSpace(string(fromServer)))
	switch {
	case strings.HasPrefix(challenge, "username"):
		return []byte(a.username), nil
	case strings.HasPrefix(challenge, "password"):
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected SMTP auth challenge: %s", fromServer)
	}
}

// smtpAuthError 标记“服务端不支持当前 AUTH 机制”的错误，只有这类错误才值得换一种方式重试。
// 凭据被拒（535）不算——反复重试只会累加失败次数，可能把邮箱账号锁掉。
type smtpAuthError struct {
	err error
}

func (e *smtpAuthError) Error() string { return e.err.Error() }
func (e *smtpAuthError) Unwrap() error { return e.err }

func isAuthError(err error) bool {
	var authErr *smtpAuthError
	return errors.As(err, &authErr)
}

// isAuthMechanismUnsupported 判断服务端是否只是不认识/不接受当前的 AUTH 机制。
// 只有这种情况才值得换一种机制重试；凭据错误（535）或账号状态问题应当直接失败。
func isAuthMechanismUnsupported(err error) bool {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		switch tpErr.Code {
		case 500, 501, 504, 534: // 语法错误 / 不支持的认证机制
			return true
		case 530, 535, 538: // 需要认证 / 凭据错误 / 加密不足
			return false
		}
	}

	msg := strings.ToLower(err.Error())
	for _, keyword := range []string{"unrecognized", "not supported", "unsupported", "authentication mechanism"} {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}

// normalizeSMTPHost 去掉 IPv6 字面量外层的方括号，便于 JoinHostPort/证书校验。
func normalizeSMTPHost(raw string) string {
	host := strings.TrimSpace(raw)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return host
}

// normalizeEncrypt 校验/推断加密方式。留空时按端口推断。
func normalizeEncrypt(raw string, port int) (string, error) {
	encrypt := strings.ToLower(strings.TrimSpace(raw))
	if encrypt == "" {
		switch port {
		case 465:
			encrypt = "ssl"
		case 587:
			encrypt = "starttls"
		default:
			encrypt = "none"
		}
	}
	switch encrypt {
	case "ssl", "tls", "starttls", "none":
		return encrypt, nil
	default:
		return "", fmt.Errorf("%s", util.LogStr("不支持的加密方式: %s", raw))
	}
}

// isLocalhost 判断 SMTP 主机是否为本机；只有本机明文连接才允许携带凭据（便于本地测试）。
func isLocalhost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parseSender 解析发件人，返回 SMTP 信封地址（裸地址）和邮件头显示值。
func parseSender(raw string) (envelope, header string, err error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("%s", util.LogStr("发件人地址不合法: %s", err.Error()))
	}
	return addr.Address, addr.String(), nil
}

// parseRecipients 解析收件人，兼容逗号、分号、换行分隔以及带显示名的地址。
func parseRecipients(raw string) ([]*mail.Address, error) {
	normalized := strings.NewReplacer("\r\n", ",", "\r", ",", "\n", ",", ";", ",").Replace(raw)
	normalized = strings.TrimSpace(normalized)
	for strings.Contains(normalized, ",,") {
		normalized = strings.ReplaceAll(normalized, ",,", ",")
	}
	normalized = strings.Trim(normalized, " \t,")
	if normalized == "" {
		return nil, errors.New(util.LogStr("收件人不能为空"))
	}
	list, err := mail.ParseAddressList(normalized)
	if err != nil {
		return nil, fmt.Errorf("%s", util.LogStr("收件人地址不合法: %s", err.Error()))
	}
	if len(list) == 0 {
		return nil, errors.New("收件人不能为空")
	}
	return list, nil
}

// CredentialScopeEqual 判断两份 SMTP 配置是否仍属于同一个“凭据作用域”。
// 密码/授权码只能自动复用于相同的主机、端口、账号和有效加密方式；改变其中
// 任何一项都必须重新输入密码，避免把旧凭据发给新的 SMTP 服务。
func (e Email) CredentialScopeEqual(other Email) bool {
	normalizePort := func(port int) int {
		if port <= 0 {
			return 465
		}
		return port
	}
	ePort, otherPort := normalizePort(e.SMTPPort), normalizePort(other.SMTPPort)
	eEncrypt, eErr := normalizeEncrypt(e.Encrypt, ePort)
	otherEncrypt, otherErr := normalizeEncrypt(other.Encrypt, otherPort)
	if eErr != nil || otherErr != nil {
		return false
	}
	return strings.EqualFold(normalizeSMTPHost(e.SMTPHost), normalizeSMTPHost(other.SMTPHost)) &&
		ePort == otherPort &&
		strings.TrimSpace(e.Username) == strings.TrimSpace(other.Username) &&
		eEncrypt == otherEncrypt
}

// Validate 检查配置是否足以发信
func (e *Email) Validate() error {
	host := normalizeSMTPHost(e.SMTPHost)
	if host == "" {
		return errors.New(util.LogStr("SMTP 服务器不能为空"))
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return errors.New(util.LogStr("SMTP 服务器不能包含空白字符"))
	}
	// 常见误填：把 host:port 整体填进“服务器”输入框
	if strings.Count(host, ":") == 1 {
		if _, _, err := net.SplitHostPort(host); err == nil {
			return errors.New(util.LogStr("SMTP 服务器只填主机名，端口请填在端口输入框"))
		}
	}
	if e.SMTPPort < 0 || e.SMTPPort > 65535 {
		return fmt.Errorf("%s", util.LogStr("SMTP 端口不合法: %d", e.SMTPPort))
	}
	port := e.SMTPPort
	if port == 0 {
		port = 465
	}
	encrypt, err := normalizeEncrypt(e.Encrypt, port)
	if err != nil {
		return err
	}
	if e.Username != "" && encrypt == "none" && !isLocalhost(host) {
		return errors.New(util.LogStr("拒绝在未加密的 SMTP 连接上发送账号密码，请改用 SSL 或 STARTTLS"))
	}

	from := strings.TrimSpace(e.From)
	if from == "" {
		from = strings.TrimSpace(e.Username)
	}
	if from == "" {
		return errors.New(util.LogStr("发件人不能为空"))
	}
	if _, _, err := parseSender(from); err != nil {
		return err
	}
	if _, err := parseRecipients(e.To); err != nil {
		return err
	}
	return nil
}

// Send 发送一封纯文本邮件
func (e *Email) Send(subject, body string) error {
	if err := e.Validate(); err != nil {
		return err
	}

	host := normalizeSMTPHost(e.SMTPHost)
	port := e.SMTPPort
	if port <= 0 {
		port = 465
	}
	encrypt, err := normalizeEncrypt(e.Encrypt, port)
	if err != nil {
		return err
	}

	fromRaw := strings.TrimSpace(e.From)
	if fromRaw == "" {
		fromRaw = strings.TrimSpace(e.Username)
	}
	envelopeFrom, headerFrom, err := parseSender(fromRaw)
	if err != nil {
		return err
	}
	toAddrs, err := parseRecipients(e.To)
	if err != nil {
		return err
	}
	envelopeTo := make([]string, 0, len(toAddrs))
	headerTo := make([]string, 0, len(toAddrs))
	for _, addr := range toAddrs {
		envelopeTo = append(envelopeTo, addr.Address)
		headerTo = append(headerTo, addr.String())
	}

	msg := buildMailMessage(headerFrom, headerTo, subject, body)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var auth smtp.Auth
	if e.Username != "" {
		if encrypt == "none" && !isLocalhost(host) {
			return errors.New("拒绝在未加密的 SMTP 连接上发送账号密码，请改用 SSL 或 STARTTLS")
		}
		auth = smtp.PlainAuth("", e.Username, e.Password, host)
	}

	accepted, err := e.deliver(addr, host, encrypt, auth, envelopeFrom, envelopeTo, msg)
	if err != nil && !accepted && auth != nil && isAuthError(err) {
		// 仅认证机制不被接受时回退 AUTH LOGIN；其他错误不重发，避免重复投递。
		util.Log("SMTP AUTH PLAIN 失败，尝试使用 LOGIN 方式重试: %s", err)
		_, err = e.deliver(addr, host, encrypt, &loginAuth{e.Username, e.Password}, envelopeFrom, envelopeTo, msg)
	}
	return err
}

// GetEmailStateSnapshot 返回当前记录的上次通知状态，供页面状态面板展示
func GetEmailStateSnapshot() (ipv4, ipv6 string, lastSent int64) {
	ipv4, ipv6, lastSent, _ = GetEmailStateSnapshotEx()
	return
}

// GetEmailStateSnapshotEx 额外返回 ChangeNotified：
// 只有真正发过“基线/变更”邮件才为 true；告警/恢复不改变它。
func GetEmailStateSnapshotEx() (ipv4, ipv6 string, lastSent int64, changeNotified bool) {
	state := loadEmailState()
	return state.LastIPv4, state.LastIPv6, state.LastSent, state.ChangeNotified
}

// deliver 建立一次 SMTP 会话并投递。
// accepted 表示服务器已对 DATA 内容返回 250（邮件已接收）；此后即使 QUIT 失败也不应重发。
func (e *Email) deliver(addr, host, encrypt string, auth smtp.Auth, from string, to []string, msg []byte) (accepted bool, err error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if encrypt == "ssl" || encrypt == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return false, fmt.Errorf("%s", util.LogStr("连接 SMTP 服务器失败: %s", err.Error()))
	}
	defer conn.Close()

	// 单次会话硬超时：SMTP 服务器半死时读写不能无限阻塞主循环。
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return false, fmt.Errorf("%s", util.LogStr("初始化 SMTP 会话失败: %s", err.Error()))
	}
	defer client.Close()

	if encrypt == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return false, errors.New(util.LogStr("SMTP 服务器不支持 STARTTLS"))
		}
		if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return false, fmt.Errorf("%s", util.LogStr("STARTTLS 失败: %s", err.Error()))
		}
	}

	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			// 用户配了账号密码，服务器却没宣告 AUTH：不能静默降级成匿名发送，
			// 否则凭据被悄悄丢弃，最终报的是莫名其妙的 "MAIL FROM 被拒绝"。
			return false, errors.New(util.LogStr("SMTP 服务器未宣告支持认证（AUTH），已配置的账号密码无法使用"))
		}
		if err = client.Auth(auth); err != nil {
			if isAuthMechanismUnsupported(err) {
				return false, &smtpAuthError{err: err}
			}
			return false, fmt.Errorf("%s", util.LogStr("SMTP 认证被拒绝: %s", err.Error()))
		}
	}

	if err = client.Mail(from); err != nil {
		return false, fmt.Errorf("%s", util.LogStr("MAIL FROM 被拒绝: %s", err.Error()))
	}
	for _, rcpt := range to {
		if err = client.Rcpt(rcpt); err != nil {
			return false, fmt.Errorf("%s", util.LogStr("RCPT TO(%s) 被拒绝: %s", rcpt, err.Error()))
		}
	}
	w, err := client.Data()
	if err != nil {
		return false, fmt.Errorf("%s", util.LogStr("DATA 被拒绝: %s", err.Error()))
	}
	if _, err = w.Write(msg); err != nil {
		return false, fmt.Errorf("%s", util.LogStr("写入邮件内容失败: %s", err.Error()))
	}
	if err = w.Close(); err != nil {
		return false, fmt.Errorf("%s", util.LogStr("提交邮件内容失败: %s", err.Error()))
	}
	accepted = true

	if err = client.Quit(); err != nil {
		// 邮件已被服务器接收，QUIT 只是关闭会话；不能判为发送失败，否则会重复投递。
		util.Log("邮件已投递，关闭 SMTP 会话时出错（忽略）: %s", err)
	}
	return true, nil
}

// SourceAddr 一个配置源本轮检测到的地址（Key 用于在状态文件里区分来源）
type SourceAddr struct {
	Key string
	// Label 是给用户看的名称（配置名或第一个域名）；为空时回退到 Key。
	Label string
	// Domains 是该配置的全部域名，用于邮件正文只展示本次变更的域名。
	Domains []string
	IPv4    string
	IPv6    string
}

// normalizeIPValue 把逗号分隔的地址规范化为 net.IP.String() 形式，
// 去重并排序。IPv6 的十六进制大小写、压缩形式由此统一，避免同一个
// 地址因为接口返回格式变化而被误判成 IP 变更。
func normalizeIPValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	seen := make(map[string]bool)
	items := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			part = ip.String()
		}
		if seen[part] {
			continue
		}
		seen[part] = true
		items = append(items, part)
	}
	sort.Strings(items)
	return strings.Join(items, ",")
}

// firstAddress 返回所有源里第一个非空的指定地址族。
// 不能直接取 normalized[0] 的一对地址：如果第一个源只有 IPv4、
// 第二个源只有 IPv6，会丢掉 IPv6。
func firstAddress(sources []SourceAddr, family string) string {
	for _, src := range sources {
		if family == "v4" && src.IPv4 != "" {
			return src.IPv4
		}
		if family == "v6" && src.IPv6 != "" {
			return src.IPv6
		}
	}
	return ""
}

// sourceLabel 返回邮件/历史里展示的来源名，兼容旧测试只填 Key 的情况。
func sourceLabel(ch sourceChange) string {
	if strings.TrimSpace(ch.Label) != "" {
		return ch.Label
	}
	return ch.Key
}

// domainsForChanges 汇总本次实际变更源涉及的域名，避免把其它配置的域名
// 也列进变更邮件。
func domainsForChanges(changes []sourceChange) []string {
	seen := make(map[string]bool)
	var domains []string
	for _, ch := range changes {
		for _, d := range ch.Domains {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			domains = append(domains, d)
		}
	}
	return domains
}

// resetFetchHealthState 清除内存 tracker 和落盘的 alerted 状态。
// 关闭告警/邮件功能时必须调用，否则重新开启时可能补发一封旧的“已恢复”。
func resetFetchHealthState() {
	fetchTrackLock.Lock()
	fetchTracker = fetchFailureTracker{}
	fetchTrackerLoaded = false
	fetchTrackLock.Unlock()
	setAlertedState(false)
}

// sourceStateKey 生成状态文件里的键
func sourceStateKey(key, family string) string {
	return key + "|" + family
}

// errSendBackoff 表示当前处于发信退避期，本轮应静默跳过
var errSendBackoff = errors.New("email send is in backoff")

// lastBackoffNotice 上次提示过的退避截止时间，避免每轮都刷同一条日志
var lastBackoffNotice atomic.Int64

// emailBackoffEpoch 每次 ResetEmailBackoff 递增。
// sendMail 在发送前记录 epoch，发送结束后只有 epoch 未变化才允许更新退避字段；
// 否则“旧 SMTP 配置的在途发送”会在新配置已经重置退避后再次把退避设回去。
var emailBackoffEpoch atomic.Uint64

// sendMail 带退避的发信：失败后逐步拉长重试间隔，成功则清零。
// SMTP 配错时每轮都重试会白白占用 90 秒并拖慢 DNS 更新，所以必须退避。
func (conf *Config) sendMail(subject, body string) error {
	attemptEpoch := emailBackoffEpoch.Load()
	state := loadEmailState()
	now := time.Now().Unix()

	// 防御异常状态：NextRetryAt 若落在很远的未来（系统时钟被大幅回拨、状态文件被
	// 手工改坏或异常写入），此后每一次发信都会被静默跳过、且没有任何日志，
	// 长期运行下等于邮件通知永久失效。超过退避上限两倍即判定无效并当场自愈。
	if state.NextRetryAt > 0 && state.NextRetryAt-now > int64((2*sendBackoffMax)/time.Second) {
		util.Log("邮件通知: 状态文件中的 nextRetryAt 异常（%d），已忽略并重置", state.NextRetryAt)
		updateEmailState(func(cur *emailState) {
			cur.NextRetryAt = 0
			cur.FailCount = 0
		})
		state.NextRetryAt = 0
		state.FailCount = 0
	}
	// FailCount 同理：负数或离谱的大值会让退避档位永远停在最高一级
	if state.FailCount < 0 || state.FailCount > 1000 {
		updateEmailState(func(cur *emailState) {
			cur.FailCount = 0
		})
		state.FailCount = 0
	}

	if state.NextRetryAt > 0 && now < state.NextRetryAt {
		// 静默跳过最容易让人以为"通知坏了"，至少把退避截止时间记一次日志。
		if lastBackoffNotice.Swap(state.NextRetryAt) != state.NextRetryAt {
			util.Log("邮件通知: 处于发送退避期, 约 %s 后重试（改好配置或测试发信成功可立即解除）",
				time.Duration(state.NextRetryAt-now)*time.Second)
		}
		return errSendBackoff
	}

	if err := conf.Email.Send(subject, body); err != nil {
		// 发送可能持续 45 秒，期间状态可能被 markNotifiedNow/ResetEmailBackoff
		// 更新。失败计数必须基于“当前状态”原子累加，不能写回发送前的快照。
		if emailBackoffEpoch.Load() != attemptEpoch {
			util.Log("邮件通知: 发送失败发生在退避重置之后（旧配置/测试已解除退避），忽略本次失败")
			return err
		}
		updateEmailState(func(cur *emailState) {
			cur.FailCount++
			backoff := sendBackoffInitial
			if cur.FailCount >= 3 {
				backoff = sendBackoffMax
			}
			cur.NextRetryAt = time.Now().Add(backoff).Unix()
		})
		return err
	}

	// 成功后在锁内清零，避免用发送前的旧快照覆盖并发写入的 LastSent/Alerted。
	// 同样不能清掉重置之后新配置产生的退避。
	if emailBackoffEpoch.Load() == attemptEpoch {
		updateEmailState(func(cur *emailState) {
			cur.FailCount = 0
			cur.NextRetryAt = 0
		})
	}
	return nil
}

// ResetEmailBackoff 解除发信退避。
//
// SMTP 配置改好、或"发送测试邮件"成功后必须调用：否则退避期（最长 60 分钟）内
// 真实通知会被静默跳过，用户会以为功能坏了——而测试发信却能成功，更容易误判。
func ResetEmailBackoff() {
	// 先递增 epoch：在途的旧发送即使稍后失败/成功，也不能再改退避字段。
	emailBackoffEpoch.Add(1)
	lastBackoffNotice.Store(0)
	changed := false
	updateEmailState(func(state *emailState) {
		if state.FailCount == 0 && state.NextRetryAt == 0 {
			return
		}
		state.FailCount = 0
		state.NextRetryAt = 0
		changed = true
	})
	if changed {
		util.Log("邮件通知: 发送退避已解除, 下一轮将立即尝试发送")
	}
}

// GetEmailBackoffUntil 返回退避截止时间（unix 秒），0 表示当前不在退避期
func GetEmailBackoffUntil() int64 {
	state := loadEmailState()
	now := time.Now().Unix()
	if state.NextRetryAt <= now {
		return 0
	}
	// 与 sendMail 的自愈阈值保持一致：异常远期值不应出现在状态面板上。
	if state.NextRetryAt-now > int64((2*sendBackoffMax)/time.Second) {
		return 0
	}
	return state.NextRetryAt
}

const (
	sendBackoffInitial = 10 * time.Minute
	sendBackoffMax     = 60 * time.Minute
)

// sourceChange 一个源相对上次的地址变化
type sourceChange struct {
	Key   string
	Label string
	// Domains 是该源配置的域名，邮件正文只列本次变更源的域名。
	Domains  []string
	PrevIPv4 string
	IPv4     string
	PrevIPv6 string
	IPv6     string
}

// NotifyIPChangeEmail 按"每个配置源各自的上次值"判断是否变化，变化时发信。
// 某个源本轮没取到地址时不会影响其它源，也不会动它自己的基线，避免误报。
func NotifyIPChangeEmail(conf *Config, sources []SourceAddr, fetchStatus FetchStatus) {
	if conf == nil {
		return
	}
	if !conf.Email.Enable {
		// 关闭邮件时清掉故障告警状态，避免重新开启后补发旧的“已恢复”。
		resetFetchHealthState()
		return
	}
	if strings.TrimSpace(conf.Email.SMTPHost) == "" || strings.TrimSpace(conf.Email.To) == "" {
		return
	}

	// 统一 IP 文本格式（IPv6 大小写/压缩形式/多地址排序），再剔除临时/已弃用的
	// IPv6（SLAAC 隐私地址），它们会按小时级轮换。
	normalized := make([]SourceAddr, 0, len(sources))
	for _, src := range sources {
		src.IPv4 = normalizeIPValue(src.IPv4)
		src.IPv6 = filterUnstableIPv6(src.IPv6)
		if src.IPv4 == "" && src.IPv6 == "" {
			continue
		}
		normalized = append(normalized, src)
	}

	// 按地址族分别取第一个非空值，而不是取第一个源的一对地址。
	primaryIPv4 := firstAddress(normalized, "v4")
	primaryIPv6 := firstAddress(normalized, "v6")

	state := loadEmailState()
	if state.Sources == nil {
		state.Sources = map[string]string{}
	}

	// 逐个源比对；只更新"本轮真的取到地址"的那些键
	next := state
	next.Sources = make(map[string]string, len(state.Sources)+len(normalized)*2)
	for k, v := range state.Sources {
		next.Sources[k] = v
	}

	var changes []sourceChange
	for _, src := range normalized {
		change := sourceChange{
			Key:      src.Key,
			Label:    src.Label,
			Domains:  src.Domains,
			PrevIPv4: normalizeIPValue(state.Sources[sourceStateKey(src.Key, "v4")]),
			PrevIPv6: normalizeIPValue(state.Sources[sourceStateKey(src.Key, "v6")]),
			IPv4:     src.IPv4,
			IPv6:     src.IPv6,
		}
		changed := false
		if src.IPv4 != "" {
			if change.PrevIPv4 != "" && change.PrevIPv4 != src.IPv4 {
				changed = true
			}
			next.Sources[sourceStateKey(src.Key, "v4")] = src.IPv4
		}
		if src.IPv6 != "" {
			if change.PrevIPv6 != "" && change.PrevIPv6 != src.IPv6 {
				changed = true
			}
			next.Sources[sourceStateKey(src.Key, "v6")] = src.IPv6
		}
		// 每个源只产出一条变更记录，且必须等 IPv4/IPv6 都填好之后再追加。
		if changed {
			changes = append(changes, change)
		}
	}

	// 取 IP 失败的告警独立于变更通知：即使本轮一个地址都没拿到，也要能报出去。
	// 把本轮变更一起传进去：恢复邮件顺带说明"故障期间 IP 变了"，之后不必再补发一封。
	alertSent, recoveredSent := handleFetchHealth(conf, fetchStatus, primaryIPv4, primaryIPv6, changes)

	if len(normalized) == 0 {
		return
	}

	firstRun := len(state.Sources) == 0 && state.LastIPv4 == "" && state.LastIPv6 == ""
	if firstRun {
		next.LastIPv4, next.LastIPv6 = primaryIPv4, primaryIPv6
		if !conf.Email.OnStart {
			commitEmailState(next)
			util.Log("邮件通知: 已记录初始 IP 基线 (%s / %s)，本次不发送", displayOrDash(primaryIPv4), displayOrDash(primaryIPv6))
			return
		}
		if recoveredSent {
			// 恢复通知里已经带了当前 IP，不再重复发基线邮件
			next.LastSent = time.Now().Unix()
			commitEmailState(next)
			return
		}
		if alertSent {
			// 告警已经发出，本轮不能再发基线邮件。
			// 告警正文已经带上本轮 IP；这里静默提交基线，保证下一轮不重复。
			next.LastSent = time.Now().Unix()
			commitEmailState(next)
			util.Log("邮件通知: 首轮已发送告警, 基线已静默记录")
			return
		}
	} else {
		if len(changes) == 0 {
			if recoveredSent {
				// 恢复通知带当前 IP；同步状态面板的“上次通知 IP”。
				next.LastIPv4, next.LastIPv6 = primaryIPv4, primaryIPv6
				next.LastSent = time.Now().Unix()
				commitEmailState(next)
				return
			}
			// 新出现的源静默记录基线；已有源没变化则什么都不做
			if len(next.Sources) != len(state.Sources) {
				commitEmailState(next)
			}
			return
		}
		// 恢复邮件里已经带上当前 IP 与故障期间的变更，直接推进基线：
		// 否则下一轮还会把同一批变更再发一遍，用户会收到两封内容重叠的邮件。
		if recoveredSent {
			next.LastIPv4, next.LastIPv6 = primaryIPv4, primaryIPv6
			next.LastSent = time.Now().Unix()
			// 恢复邮件正文带了故障期间的变更，也算一次“变更通知”。
			next.ChangeNotified = true
			commitEmailState(next)
			util.Log("邮件通知: 恢复通知已包含本次变更, 不再单独发送变更邮件")
			return
		}
		// 告警轮绝不能推进基线：那样这次变更会被永久丢弃，之后再也检测不到。
		// 直接返回、不写状态，下一轮会重新检测到同一个变更并补发。
		if alertSent {
			util.Log("邮件通知: 本轮已发送告警邮件, IP 变更通知顺延到下一轮")
			return
		}
	}

	// 本轮要报告的变更。首轮基线要列出全部源；其余轮次列出全部变更源。
	var report []sourceChange
	if firstRun {
		report = make([]sourceChange, 0, len(normalized))
		for _, src := range normalized {
			report = append(report, sourceChange{
				Key:     src.Key,
				Label:   src.Label,
				Domains: src.Domains,
				IPv4:    src.IPv4,
				IPv6:    src.IPv6,
			})
		}
	} else {
		report = changes
	}
	if len(report) == 0 {
		return
	}

	subject, body := composeIPChangeMail(conf, report, firstRun)
	if err := conf.sendMail(subject, body); err != nil {
		if errors.Is(err, errSendBackoff) {
			return
		}
		util.Log("邮件通知发送失败（已进入退避）: %s", err)
		// 不更新状态：下一轮（退避期满后）会重新检测到这个变化
		return
	}

	next.LastIPv4, next.LastIPv6 = primaryIPv4, primaryIPv6
	next.LastSent = time.Now().Unix()
	next.ChangeNotified = true
	commitEmailState(next)

	kind := historyKindChange
	if firstRun {
		kind = historyKindBaseline
	}
	// 每条变更单独记一行，面板里能看出是哪个配置源变的
	for _, ch := range report {
		AppendEmailHistory(EmailHistoryEntry{
			Kind:     kind,
			Source:   sourceLabel(ch),
			IPv4:     ch.IPv4,
			PrevIPv4: ch.PrevIPv4,
			IPv6:     ch.IPv6,
			PrevIPv6: ch.PrevIPv6,
		})
	}
	if firstRun {
		util.Log("邮件通知: 已发送初始 IP 基线邮件 (%s / %s)", displayOrDash(primaryIPv4), displayOrDash(primaryIPv6))
	} else if len(report) > 1 {
		util.Log("邮件通知: %d 个配置源 IP 已变更，合并通知已发送", len(report))
	} else {
		util.Log("邮件通知: IP 已变更，通知邮件发送成功 (%s / %s)", displayOrDash(primaryIPv4), displayOrDash(primaryIPv6))
	}
}

// fetchAlertThreshold 连续多少轮取不到 IP 才告警，避免网络抖动造成误报
const fetchAlertThreshold = 3

// fetchFailureTracker 记录连续失败次数和已告警的精确失败项。
// 0.17 起不再只记一个 alerted 布尔值：当 IPv4 已经告警、IPv6 又失败时，
// 新出现的失败项必须能触发新的告警，而不是被旧的 alerted 永久压住。
type fetchFailureTracker struct {
	consecutive int
	alerted     bool
	// failureKeys 是已经告警覆盖的失败项，键形如 "sourceKey|IPv4"。
	failureKeys []string
	// pendingKeys/pendingCount 用于在旧故障仍在时，为新出现的失败项单独累计阈值。
	pendingKeys  []string
	pendingCount int
	// families/sources 是为状态兼容保留的派生视图。
	families []string
	sources  []string
}

// observe 保留给旧调用方/旧测试：只处理布尔故障状态。
func (t *fetchFailureTracker) observe(failed bool) (alert bool, recovered bool) {
	if !failed {
		// 注意：这里不立即清空 families。调用方需要用它判断
		// 原先失败的地址族是否仍然存在，再决定是否发恢复邮件。
		recovered = t.alerted
		t.consecutive = 0
		t.alerted = false
		return false, recovered
	}

	t.consecutive++
	if t.consecutive >= fetchAlertThreshold && !t.alerted {
		t.alerted = true
		return true, false
	}
	return false, false
}

func (t *fetchFailureTracker) syncDerived() {
	t.families = uniqueFamiliesFromKeys(t.failureKeys)
	t.sources = sourcesFromFailureKeys(t.failureKeys)
}

// observeStatus 用精确的 source|family 失败集合驱动告警状态。
// - 旧故障期间出现新失败项：先按阈值单独累计，达到阈值后发新告警；
// - 旧故障仍在时不重置旧告警，避免丢失最终恢复通知；
// - 全部健康时返回 recovered，调用方按 previousKeys 逐项判断是否真的恢复。
func (t *fetchFailureTracker) observeStatus(status FetchStatus) (alert bool, recovered bool) {
	keys := status.failureKeys()
	if len(keys) == 0 {
		recovered = t.alerted
		t.consecutive = 0
		t.alerted = false
		t.failureKeys = nil
		t.pendingKeys = nil
		t.pendingCount = 0
		t.families = nil
		t.sources = nil
		return false, recovered
	}

	if !t.alerted {
		t.consecutive++
		if t.consecutive >= fetchAlertThreshold {
			t.alerted = true
			t.failureKeys = append([]string(nil), keys...)
			t.pendingKeys = nil
			t.pendingCount = 0
			t.syncDerived()
			return true, false
		}
		return false, false
	}

	// 兼容没有任何失败项明细的旧状态：先采用当前集合，不在升级首轮重复告警。
	if len(t.failureKeys) == 0 {
		t.failureKeys = append([]string(nil), keys...)
		t.syncDerived()
		return false, false
	}

	newKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if !failureKeyCovered(t.failureKeys, key) {
			newKeys = append(newKeys, key)
		}
	}
	if len(newKeys) > 0 {
		t.pendingKeys = mergeFailureKeys(t.pendingKeys, newKeys)
		t.pendingCount++
		if t.pendingCount >= fetchAlertThreshold {
			t.failureKeys = mergeFailureKeys(t.failureKeys, keys)
			t.consecutive = t.pendingCount
			t.pendingKeys = nil
			t.pendingCount = 0
			t.syncDerived()
			return true, false
		}
		// 新失败项在达到阈值前不能并入 failureKeys，否则下一轮就会被
		// failureKeyCovered 当成“已告警”，永远不会达到阈值。
		t.syncDerived()
		return false, false
	}

	// 没有新失败项时，把当前仍失败的项并入已告警集合；已经恢复的旧项
	// 会保留到恢复通知成功，或被“原任务已删除/禁用”逻辑清理。
	t.pendingKeys = nil
	t.pendingCount = 0
	t.failureKeys = mergeFailureKeys(t.failureKeys, keys)
	t.syncDerived()
	return false, false
}

var (
	fetchTrackLock sync.Mutex
	fetchTracker   fetchFailureTracker
	// fetchTrackerLoaded 保证第一次使用时从状态文件恢复 alerted
	fetchTrackerLoaded bool
)

// setAlertedStateWithFailures 把“当前故障是否已告警”和精确失败项写入状态文件。
func setAlertedStateWithFailures(alerted bool, keys []string) {
	copiedKeys := append([]string(nil), keys...)
	families := uniqueFamiliesFromKeys(copiedKeys)
	sources := sourcesFromFailureKeys(copiedKeys)
	updateEmailState(func(state *emailState) {
		state.Alerted = alerted
		if alerted {
			state.AlertedFailureKeys = copiedKeys
			state.AlertedFamilies = families
			state.AlertedSources = sources
		} else {
			state.AlertedFailureKeys = nil
			state.AlertedFamilies = nil
			state.AlertedSources = nil
		}
	})
}

// setAlertedStateWithAlert 兼容旧调用方：把族/源转换为精确失败项后写入。
func setAlertedStateWithAlert(alerted bool, families, sources []string) {
	setAlertedStateWithFailures(alerted, failureKeysFromLegacy(families, sources))
}

// setAlertedStateWithFamilies 兼容只关心地址族的调用方。
func setAlertedStateWithFamilies(alerted bool, families []string) {
	setAlertedStateWithAlert(alerted, families, nil)
}

// setAlertedState 兼容只关心布尔值的调用方；传 false 会一并清空地址族。
func setAlertedState(alerted bool) {
	setAlertedStateWithFamilies(alerted, nil)
}

// familiesToRecover 根据“告警时的失败族”和本轮状态，决定恢复通知应提哪些族。
// previous 为空表示旧状态文件没有记录族，退回当前期望的族（兼容迁移）。
func familiesToRecover(status FetchStatus, previous []string) []string {
	if len(previous) == 0 {
		return status.expectedFamilies()
	}
	seen := make(map[string]bool)
	var out []string
	for _, family := range previous {
		switch {
		case strings.EqualFold(family, "IPv4") && status.IPv4Expected && !seen["IPv4"]:
			seen["IPv4"] = true
			out = append(out, "IPv4")
		case strings.EqualFold(family, "IPv6") && status.IPv6Expected && !seen["IPv6"]:
			seen["IPv6"] = true
			out = append(out, "IPv6")
		}
	}
	return out
}

// failedSourceKeys 取告警时失败源的 Key 列表。
func failedSourceKeys(sources []FailedSource) []string {
	seen := make(map[string]bool)
	var out []string
	for _, src := range sources {
		key := strings.TrimSpace(src.Key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// anySourceStillConfigured 判断告警时的失败源是否还有至少一个在本轮有效。
func anySourceStillConfigured(previous, configured []string) bool {
	if len(previous) == 0 {
		return true
	}
	current := make(map[string]bool, len(configured))
	for _, key := range configured {
		current[key] = true
	}
	for _, key := range previous {
		if current[key] {
			return true
		}
	}
	return false
}

// markNotifiedNow 记录"刚刚发过通知"的时间戳，但不改动 IP 基线。
// 告警/恢复这类通知也要反映到状态面板的"上次通知"上。
func markNotifiedNow() {
	now := time.Now().Unix()
	updateEmailState(func(state *emailState) {
		state.LastSent = now
	})
}

// handleFetchHealth 处理"取不到公网 IP"的告警与恢复通知。
// 返回本轮是否真的发出了告警 / 恢复邮件（调用方据此保证"每轮最多一封"）。
func handleFetchHealth(conf *Config, status FetchStatus, ipv4, ipv6 string, changes []sourceChange) (alertSent, recoveredSent bool) {
	if !conf.Email.AlertOnFetchFailure {
		// 关闭告警时同时清掉落盘的 alerted：
		// 否则重新开启后，若网络已恢复，会用旧的 alerted=true 补发一封假的恢复通知。
		resetFetchHealthState()
		return false, false
	}

	fetchTrackLock.Lock()
	if !fetchTrackerLoaded {
		// 进程重启后恢复“是否已告警”和已告警的精确失败项，否则：
		// 故障期间重启 → 恢复时不会发恢复通知；已告警的故障会再告警一次。
		state := loadEmailState()
		fetchTracker.alerted = state.Alerted
		keys := append([]string(nil), state.AlertedFailureKeys...)
		if len(keys) == 0 && state.Alerted {
			keys = failureKeysFromLegacy(state.AlertedFamilies, state.AlertedSources)
		}
		fetchTracker.failureKeys = keys
		fetchTracker.families = uniqueFamiliesFromKeys(keys)
		fetchTracker.sources = sourcesFromFailureKeys(keys)
		fetchTrackerLoaded = true
	}

	// 必须在 observeStatus 之前留快照：健康轮次会清空 tracker，但恢复判断
	// 仍需要知道“告警时到底坏了哪些 source|family”。
	previousKeys := append([]string(nil), fetchTracker.failureKeys...)
	previousFamilies := append([]string(nil), fetchTracker.families...)
	previousSources := append([]string(nil), fetchTracker.sources...)
	alert, recovered := fetchTracker.observeStatus(status)
	consecutive := fetchTracker.consecutive
	alertKeys := append([]string(nil), fetchTracker.failureKeys...)
	fetchTrackLock.Unlock()

	if alert {
		subject, body := composeFetchAlertMail(conf, consecutive, status, ipv4, ipv6)
		if err := conf.sendMail(subject, body); err != nil {
			util.Log("邮件通知: IP 获取失败告警发送失败: %s", err)
			// 允许下一轮重试，避免一次发送失败就把告警吞掉。
			// 同时清掉旧告警：新告警完整覆盖了当前失败项，旧状态不能再压住它。
			fetchTrackLock.Lock()
			fetchTracker = fetchFailureTracker{}
			fetchTrackerLoaded = true
			fetchTrackLock.Unlock()
			setAlertedStateWithFailures(false, nil)
			return false, false
		}
		alertSent = true
		if len(alertKeys) == 0 {
			alertKeys = status.failureKeys()
		}
		AppendEmailHistory(EmailHistoryEntry{Kind: historyKindFetchFailed, IPv4: ipv4, IPv6: ipv6})
		markNotifiedNow()
		setAlertedStateWithFailures(true, alertKeys)
		util.Log("邮件通知: 连续 %d 轮未能获取 IP, 已发送告警邮件", consecutive)
	}

	if recovered {
		var recoveryFamilies []string

		if len(previousKeys) > 0 {
			// 0.17 精确路径：逐项确认原失败源/族仍然启用且已经成功。
			recoveredKeys := recoverableFailureKeys(previousKeys, status)
			recoveryFamilies = uniqueFamiliesFromKeys(recoveredKeys)
			if len(recoveryFamilies) == 0 {
				fetchTrackLock.Lock()
				fetchTracker = fetchFailureTracker{}
				fetchTrackerLoaded = true
				fetchTrackLock.Unlock()
				setAlertedStateWithFailures(false, nil)
				util.Log("邮件通知: 原失败项已移除、禁用或未恢复, 清除告警状态, 不发送恢复通知")
				return alertSent, false
			}
		} else {
			// 兼容 0.17 之前没有失败项明细的状态：沿用旧的族级判断。
			if !anySourceStillConfigured(previousSources, status.ConfiguredSources) {
				fetchTrackLock.Lock()
				fetchTracker = fetchFailureTracker{}
				fetchTrackerLoaded = true
				fetchTrackLock.Unlock()
				setAlertedStateWithFailures(false, nil)
				util.Log("邮件通知: 原失败配置源已移除, 清除告警状态, 不发送恢复通知")
				return alertSent, false
			}
			recoveryFamilies = familiesToRecover(status, previousFamilies)
			if len(recoveryFamilies) == 0 {
				fetchTrackLock.Lock()
				fetchTracker = fetchFailureTracker{}
				fetchTrackerLoaded = true
				fetchTrackLock.Unlock()
				setAlertedStateWithFailures(false, nil)
				util.Log("邮件通知: 原失败检测任务已移除或禁用, 清除告警状态, 不发送恢复通知")
				return alertSent, false
			}
		}

		subject, body := composeRecoveredMailForFamilies(conf, recoveryFamilies, ipv4, ipv6, changes)
		if err := conf.sendMail(subject, body); err != nil {
			util.Log("邮件通知: IP 获取恢复通知发送失败: %s", err)
			// observeStatus 已经清掉 alerted；这里必须还原，否则下一轮健康时
			// 不会再触发恢复通知，通知就永久丢失了。
			fetchTrackLock.Lock()
			fetchTracker.alerted = true
			fetchTracker.consecutive = 0
			fetchTracker.failureKeys = append([]string(nil), previousKeys...)
			fetchTracker.families = append([]string(nil), previousFamilies...)
			fetchTracker.sources = append([]string(nil), previousSources...)
			fetchTrackerLoaded = true
			fetchTrackLock.Unlock()
			return false, false
		}
		recoveredSent = true
		AppendEmailHistory(EmailHistoryEntry{Kind: historyKindRecovered, IPv4: ipv4, IPv6: ipv6})
		markNotifiedNow()
		setAlertedStateWithFailures(false, nil)
		fetchTrackLock.Lock()
		fetchTracker = fetchFailureTracker{}
		fetchTrackerLoaded = true
		fetchTrackLock.Unlock()
		util.Log("邮件通知: IP 获取已恢复, 已发送恢复通知")
		return alertSent, true
	}

	return alertSent, recoveredSent
}

// composeFetchAlertMail 生成"取不到 IP"的告警邮件。
// ipv4/ipv6 是本轮实际取到的地址；某个地址族失败时另一个族可能仍然成功，
// 这时应展示本轮值，而不是状态文件里“上次通知”的旧值。
func composeFetchAlertMail(conf *Config, consecutive int, status FetchStatus, ipv4, ipv6 string) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()
	state := loadEmailState()
	if strings.TrimSpace(ipv4) == "" {
		ipv4 = state.LastIPv4
	}
	if strings.TrimSpace(ipv6) == "" {
		ipv6 = state.LastIPv6
	}
	families := status.failedFamilies()
	familyText := strings.Join(families, " / ")
	if familyText == "" {
		familyText = "公网 IP"
	}

	var b strings.Builder
	if zh {
		subject = fmt.Sprintf("[ddns-go] 连续 %d 轮未能获取 %s 地址", consecutive, familyText)
		fmt.Fprintf(&b, "ddns-go 已连续 %d 轮未能检测到 %s 地址，相关域名解析可能已经停止更新。\n\n", consecutive, familyText)
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
		fmt.Fprintf(&b, "最后已知 IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "最后已知 IPv6: %s\n", displayOrDash(ipv6))
		writeFailedSources(&b, status.FailedSources, zh)
		fmt.Fprintf(&b, "\n请检查容器网络、%s 的 IP 查询接口是否可访问。恢复之后会再发一封通知。\n", familyText)
	} else {
		subject = fmt.Sprintf("[ddns-go] Failed to detect %s address for %d rounds", familyText, consecutive)
		fmt.Fprintf(&b, "ddns-go has failed to detect the %s address for %d consecutive rounds; related DNS records may be stale.\n\n", familyText, consecutive)
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
		fmt.Fprintf(&b, "Last known IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "Last known IPv6: %s\n", displayOrDash(ipv6))
		writeFailedSources(&b, status.FailedSources, zh)
		fmt.Fprintf(&b, "\nCheck the container network and whether the %s IP lookup endpoints are reachable. A recovery notice will follow.\n", familyText)
	}
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// writeFailedSources 在告警正文里列出具体失败的配置源/地址族/域名。
func writeFailedSources(b *strings.Builder, sources []FailedSource, zh bool) {
	if len(sources) == 0 {
		return
	}
	if zh {
		b.WriteString("\n失败来源:\n")
	} else {
		b.WriteString("\nFailed sources:\n")
	}
	for _, src := range sources {
		label := strings.TrimSpace(src.Label)
		if label == "" {
			label = src.Key
		}
		if label == "" {
			label = "-"
		}
		families := strings.Join(src.Families, "/")
		if families == "" {
			families = "-"
		}
		domains := strings.Join(src.Domains, ", ")
		if domains == "" {
			domains = "-"
		}
		if zh {
			fmt.Fprintf(b, "  配置: %s | 地址族: %s | 域名: %s\n", label, families, domains)
		} else {
			fmt.Fprintf(b, "  Config: %s | Family: %s | Domains: %s\n", label, families, domains)
		}
	}
}

// composeRecoveredMail 保留原签名，供已有测试/调用方使用。
func composeRecoveredMail(conf *Config, status FetchStatus, ipv4, ipv6 string, changes []sourceChange) (subject, body string) {
	return composeRecoveredMailForFamilies(conf, status.expectedFamilies(), ipv4, ipv6, changes)
}

// composeRecoveredMailForFamilies 生成"已恢复"的邮件。
// families 只包含“原先失败、现在仍启用且已恢复”的地址族；
// 如果原失败任务被删除/禁用，调用方不会走到这里。
//
// changes 是故障期间检测到的 IP 变更，一并列出：否则恢复后下一轮还会把同一批
// 变更再发一封，用户会收到两封内容重叠的邮件。
func composeRecoveredMailForFamilies(conf *Config, families []string, ipv4, ipv6 string, changes []sourceChange) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()
	familyText := strings.Join(families, " / ")
	if familyText == "" {
		familyText = "公网 IP"
	}

	var b strings.Builder
	if zh {
		subject = fmt.Sprintf("[ddns-go] %s 检测已恢复", familyText)
		fmt.Fprintf(&b, "ddns-go 已重新拿到 %s 地址，检测恢复正常。\n\n", familyText)
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
		fmt.Fprintf(&b, "当前 IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "当前 IPv6: %s\n", displayOrDash(ipv6))
	} else {
		subject = fmt.Sprintf("[ddns-go] %s detection recovered", familyText)
		fmt.Fprintf(&b, "ddns-go can detect the %s address again.\n\n", familyText)
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
		fmt.Fprintf(&b, "IPv4: %s\n", displayOrDash(ipv4))
		fmt.Fprintf(&b, "IPv6: %s\n", displayOrDash(ipv6))
	}
	writeChangeSummary(&b, changes, zh)
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// writeChangeSummary 把"故障期间发生的 IP 变更"逐条写进正文
func writeChangeSummary(b *strings.Builder, changes []sourceChange, zh bool) {
	if len(changes) == 0 {
		return
	}
	if zh {
		b.WriteString("\n故障期间检测到 IP 变更:\n")
	} else {
		b.WriteString("\nIP changes detected while detection was failing:\n")
	}
	for _, ch := range changes {
		label := sourceLabel(ch)
		if label == "" {
			label = "-"
		}
		if ch.IPv4 != "" && ch.IPv4 != ch.PrevIPv4 {
			fmt.Fprintf(b, "  %s: IPv4 %s -> %s\n", label, displayOrDash(ch.PrevIPv4), ch.IPv4)
		}
		if ch.IPv6 != "" && ch.IPv6 != ch.PrevIPv6 {
			fmt.Fprintf(b, "  %s: IPv6 %s -> %s\n", label, displayOrDash(ch.PrevIPv6), ch.IPv6)
		}
	}
}

// filterUnstableIPv6 从逗号分隔的地址串里剔除本机标记为临时/已弃用/待定的 IPv6。
// 无论是否解析到 /proc/net/if_inet6，返回值都会先做规范化（net.IP.String()）、
// 去重和排序：IPv6 的大小写/压缩形式不应被当成地址变化。
func filterUnstableIPv6(raw string) string {
	normalized := normalizeIPValue(raw)
	if normalized == "" {
		return ""
	}
	unstable := unstableIPv6Addrs()
	if len(unstable) == 0 {
		return normalized
	}

	kept := make([]string, 0, 4)
	var dropped []string
	for _, item := range strings.Split(normalized, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if ip := net.ParseIP(item); ip != nil && unstable[ip.String()] {
			dropped = append(dropped, item)
			continue
		}
		kept = append(kept, item)
	}

	if len(dropped) > 0 {
		logIgnoredIPv6(dropped)
	}
	sort.Strings(kept)
	return strings.Join(kept, ",")
}

// logIgnoredIPv6 同一组被忽略的地址只记一次日志，避免每轮刷屏
func logIgnoredIPv6(dropped []string) {
	sort.Strings(dropped)
	key := strings.Join(dropped, ",")

	emailStateLock.Lock()
	repeated := key == lastIgnoredIPv6
	lastIgnoredIPv6 = key
	emailStateLock.Unlock()

	if !repeated {
		util.Log("邮件通知: 忽略临时/已弃用的 IPv6 地址: %s", key)
	}
}

// composeIPChangeMail 生成通知邮件的标题和正文。
//
// changes 可能包含多个配置源在同一轮的变更，必须全部列出：
// 只报第一条却把每个源的基线都推进，会让其余变更永久丢失。
func composeIPChangeMail(conf *Config, changes []sourceChange, firstRun bool) (subject, body string) {
	zh := strings.HasPrefix(strings.ToLower(conf.Lang), "zh")
	now := time.Now().Format("2006-01-02 15:04:05 -07:00")
	hostname, _ := os.Hostname()

	first := sourceChange{}
	if len(changes) > 0 {
		first = changes[0]
	}
	firstIP := first.IPv4
	if firstIP == "" {
		firstIP = first.IPv6
	}

	var b strings.Builder
	switch {
	case firstRun:
		if zh {
			subject = fmt.Sprintf("[ddns-go] 启动基线: 当前 IP %s", displayOrDash(firstIP))
			b.WriteString("ddns-go 已启动，这是首次记录的 IP 基线。\n\n")
		} else {
			subject = fmt.Sprintf("[ddns-go] Baseline IP: %s", displayOrDash(firstIP))
			b.WriteString("ddns-go has started. This is the first recorded IP baseline.\n\n")
		}
	case len(changes) > 1:
		subject = composeMultiChangeSubject(zh, changes)
		if zh {
			fmt.Fprintf(&b, "ddns-go 检测到 %d 个配置的公网 IP 发生变化。\n\n", len(changes))
		} else {
			fmt.Fprintf(&b, "ddns-go detected IP changes for %d configurations.\n\n", len(changes))
		}
	default:
		subject = composeChangeSubject(zh, first)
		if zh {
			b.WriteString("ddns-go 检测到公网 IP 地址发生变化。\n\n")
		} else {
			b.WriteString("ddns-go detected that the public IP address has changed.\n\n")
		}
	}

	if zh {
		fmt.Fprintf(&b, "时间: %s\n", now)
		fmt.Fprintf(&b, "主机: %s\n", hostname)
	} else {
		fmt.Fprintf(&b, "Time: %s\n", now)
		fmt.Fprintf(&b, "Host: %s\n", hostname)
	}

	for _, ch := range changes {
		if len(changes) > 1 {
			b.WriteString("\n")
		}
		if label := sourceLabel(ch); label != "" {
			if zh {
				fmt.Fprintf(&b, "配置: %s\n", label)
			} else {
				fmt.Fprintf(&b, "Config: %s\n", label)
			}
		}
		writeAddrLines(&b, ch, zh, !firstRun)
	}

	domains := domainsForChanges(changes)
	if len(domains) > 0 {
		if zh {
			fmt.Fprintf(&b, "域名: %s\n", strings.Join(domains, ", "))
		} else {
			fmt.Fprintf(&b, "Domains: %s\n", strings.Join(domains, ", "))
		}
	}
	b.WriteString("\n-- \nddns-go email notification\n")
	return subject, b.String()
}

// composeChangeSubject 单条变更的主题
func composeChangeSubject(zh bool, ch sourceChange) string {
	v4Changed := ch.IPv4 != "" && ch.PrevIPv4 != ch.IPv4
	v6Changed := ch.IPv6 != "" && ch.PrevIPv6 != ch.IPv6
	if zh {
		switch {
		case v4Changed && v6Changed:
			return fmt.Sprintf("[ddns-go] IP 地址变更: %s -> %s / IPv6 %s -> %s",
				displayOrDash(ch.PrevIPv4), displayOrDash(ch.IPv4),
				displayOrDash(ch.PrevIPv6), displayOrDash(ch.IPv6))
		case v6Changed:
			return fmt.Sprintf("[ddns-go] IPv6 地址变更: %s -> %s",
				displayOrDash(ch.PrevIPv6), displayOrDash(ch.IPv6))
		default:
			return fmt.Sprintf("[ddns-go] IP 地址变更: %s -> %s",
				displayOrDash(ch.PrevIPv4), displayOrDash(ch.IPv4))
		}
	}
	switch {
	case v4Changed && v6Changed:
		return fmt.Sprintf("[ddns-go] IP changed: %s -> %s / IPv6 %s -> %s",
			displayOrDash(ch.PrevIPv4), displayOrDash(ch.IPv4),
			displayOrDash(ch.PrevIPv6), displayOrDash(ch.IPv6))
	case v6Changed:
		return fmt.Sprintf("[ddns-go] IPv6 changed: %s -> %s",
			displayOrDash(ch.PrevIPv6), displayOrDash(ch.IPv6))
	default:
		return fmt.Sprintf("[ddns-go] IP changed: %s -> %s",
			displayOrDash(ch.PrevIPv4), displayOrDash(ch.IPv4))
	}
}

// composeMultiChangeSubject 多源同轮变更的主题：给条数与结果 IP，避免主题过长
func composeMultiChangeSubject(zh bool, changes []sourceChange) string {
	ips := make([]string, 0, len(changes))
	for _, ch := range changes {
		// 优先展示真正变化的地址族；否则 IPv6-only 源会展示未变的 IPv4。
		var ip string
		if ch.IPv4 != "" && ch.IPv4 != ch.PrevIPv4 {
			ip = ch.IPv4
		} else if ch.IPv6 != "" {
			ip = ch.IPv6
		} else {
			ip = ch.IPv4
		}
		if ip != "" {
			ips = append(ips, ip)
		}
	}
	list := strings.Join(ips, ", ")
	if zh {
		return fmt.Sprintf("[ddns-go] %d 个配置的 IP 地址变更: %s", len(changes), list)
	}
	return fmt.Sprintf("[ddns-go] IP changed for %d configs: %s", len(changes), list)
}

// writeAddrLines 写一个源的新旧地址
func writeAddrLines(b *strings.Builder, ch sourceChange, zh, showPrev bool) {
	if zh {
		fmt.Fprintf(b, "IPv4: %s\n", displayOrDash(ch.IPv4))
		if showPrev {
			fmt.Fprintf(b, "  上次: %s\n", displayOrDash(ch.PrevIPv4))
		}
		fmt.Fprintf(b, "IPv6: %s\n", displayOrDash(ch.IPv6))
		if showPrev {
			fmt.Fprintf(b, "  上次: %s\n", displayOrDash(ch.PrevIPv6))
		}
		return
	}
	fmt.Fprintf(b, "IPv4: %s\n", displayOrDash(ch.IPv4))
	if showPrev {
		fmt.Fprintf(b, "  previous: %s\n", displayOrDash(ch.PrevIPv4))
	}
	fmt.Fprintf(b, "IPv6: %s\n", displayOrDash(ch.IPv6))
	if showPrev {
		fmt.Fprintf(b, "  previous: %s\n", displayOrDash(ch.PrevIPv6))
	}
}

func displayOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// buildMailMessage 构造 MIME 报文（正文 base64，避免中文与换行问题）
func buildMailMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	writeFoldedHeaderLine(&b, "From", from)
	writeFoldedHeaderLine(&b, "To", strings.Join(to, ", "))
	writeFoldedHeaderLine(&b, "Subject", mime.QEncoding.Encode("utf-8", sanitizeHeaderValue(subject)))
	writeHeaderLine(&b, "Date", time.Now().Format(time.RFC1123Z))
	writeHeaderLine(&b, "Message-ID", buildMessageID())
	writeHeaderLine(&b, "MIME-Version", "1.0")
	writeHeaderLine(&b, "Content-Type", "text/plain; charset=UTF-8")
	writeHeaderLine(&b, "Content-Transfer-Encoding", "base64")
	b.WriteString("\r\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// writeHeaderLine 写入一行头，禁止头注入
func writeHeaderLine(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(sanitizeHeaderValue(value))
	b.WriteString("\r\n")
}

const maxHeaderLineLength = 76

// writeFoldedHeaderLine 写入可折叠的头。To/Subject 在多收件人或多源时
// 可能非常长，RFC 5321 限制单行 1000 字符（含 CRLF），这里在空格处折到 76。
func writeFoldedHeaderLine(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	prefixLen := len(key) + 2
	lineLen := prefixLen
	words := strings.Fields(sanitizeHeaderValue(value))
	if len(words) == 0 {
		b.WriteString("\r\n")
		return
	}
	for i, word := range words {
		if i > 0 {
			if lineLen+1+len(word) > maxHeaderLineLength && lineLen > prefixLen {
				b.WriteString("\r\n ")
				lineLen = 1
			} else {
				b.WriteString(" ")
				lineLen++
			}
		}
		b.WriteString(word)
		lineLen += len(word)
	}
	b.WriteString("\r\n")
}

// sanitizeHeaderValue 去掉换行，避免在头部注入额外的头
func sanitizeHeaderValue(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

// msgIDSeq 保证同一进程内多封邮件的 Message-ID 不重复
var msgIDSeq atomic.Uint64

// msgIDDomain 取主机名作为 Message-ID 的域名部分，只保留合法字符
func msgIDDomain() string {
	host, err := os.Hostname()
	if err != nil {
		return "ddns-go.local"
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "ddns-go.local"
	}
	return out
}

// buildMessageID 生成 RFC 5322 的 Message-ID。
//
// 缺这个头是常见的垃圾邮件评分项（QQ/163/Outlook 尤为明显），
// 一个只用来发告警的镜像不应该在这上面丢分。
func buildMessageID() string {
	return fmt.Sprintf("<%d.%d.%d@%s>",
		time.Now().UnixNano(), os.Getpid(), msgIDSeq.Add(1), msgIDDomain())
}

// emailStatePath 状态文件与配置文件放在同一目录，随容器挂载卷一起持久化
func emailStatePath() string {
	return util.GetConfigFilePath() + ".email-state.json"
}

var (
	// emailStateLock 保护内存态、状态文件写入和日志去重标记。
	// “读状态→发信→写状态”这一整段仍由 dns.RunOnce 的串行锁保证不并发；
	// 但保存配置/测试邮件触发的 ResetEmailBackoff 可能与后台发信并发，
	// 所以文件写入也必须在同一把锁内完成，避免两个 goroutine 抢同一个 .tmp。
	emailStateLock sync.Mutex
	// emailStateMem 是本次运行内的权威状态：即便落盘失败也不会重复提醒
	emailStateMem  *emailState
	emailStateWarn bool
	// emailStateParseWarn 保证损坏的状态文件只刷一次日志，而不是每次状态轮询都刷。
	emailStateParseWarn bool
	lastIgnoredIPv6     string
)

// normalizeEmailState 统一状态文件里的 IP 文本形式。
// 0.13/0.14 的状态文件可能保存了非规范的 IPv6（大写、未压缩），
// 比较前必须把基线和当前值都规范化，否则升级后的首轮会误发变更邮件。
func normalizeEmailState(state *emailState) {
	state.LastIPv4 = normalizeIPValue(state.LastIPv4)
	state.LastIPv6 = normalizeIPValue(state.LastIPv6)
	if state.Alerted && len(state.AlertedFailureKeys) == 0 {
		state.AlertedFailureKeys = failureKeysFromLegacy(state.AlertedFamilies, state.AlertedSources)
	}
	if !state.Alerted {
		state.AlertedFailureKeys = nil
	}
	state.AlertedFailureKeys = sortedUnique(state.AlertedFailureKeys)
	state.AlertedFamilies = uniqueFamiliesFromKeys(state.AlertedFailureKeys)
	state.AlertedSources = sourcesFromFailureKeys(state.AlertedFailureKeys)
	if state.Sources == nil {
		return
	}
	sources := make(map[string]string, len(state.Sources))
	for k, v := range state.Sources {
		sources[k] = normalizeIPValue(v)
	}
	state.Sources = sources
}

// loadEmailStateLocked 调用方必须持有 emailStateLock。
func loadEmailStateLocked() emailState {
	if emailStateMem != nil {
		state := *emailStateMem
		normalizeEmailState(&state)
		return state
	}

	byt, err := os.ReadFile(emailStatePath())
	if err != nil {
		return emailState{}
	}
	var state emailState
	if err := json.Unmarshal(byt, &state); err != nil {
		if !emailStateParseWarn {
			emailStateParseWarn = true
			util.Log("邮件通知: 状态文件无法解析, 按首次运行处理: %s", err)
		}
		return emailState{}
	}
	emailStateParseWarn = false
	// 旧状态文件没有 schema 字段，也没有 ChangeNotified：
	// 若曾经有过成功通知，迁移为“已通知过 IP”，避免升级后 UI 退回“基线未通知”。
	if state.Schema < emailStateSchema {
		if state.LastSent > 0 && (state.LastIPv4 != "" || state.LastIPv6 != "") {
			state.ChangeNotified = true
		}
		state.Schema = emailStateSchema
	}
	// 0.16 及更早的状态只有“族列表 + 源列表”，没有 source|family 对应关系。
	// 这里做一次兼容迁移；新告警会写入精确的 AlertedFailureKeys。
	if state.Alerted && len(state.AlertedFailureKeys) == 0 {
		state.AlertedFailureKeys = failureKeysFromLegacy(state.AlertedFamilies, state.AlertedSources)
	}
	normalizeEmailState(&state)
	return state
}

// loadEmailState 优先返回内存态，其次读文件
func loadEmailState() emailState {
	emailStateLock.Lock()
	defer emailStateLock.Unlock()
	return loadEmailStateLocked()
}

// persistEmailStateLocked 更新内存态并落盘；调用方必须持有 emailStateLock。
func persistEmailStateLocked(state emailState) (shouldWarn bool, err error) {
	emailStateMem = &state
	err = writeEmailStateFile(state)
	if err != nil {
		shouldWarn = !emailStateWarn
		emailStateWarn = true
		return
	}
	emailStateWarn = false
	emailStateParseWarn = false
	return
}

// storeEmailState 用给定状态整体替换内存态与文件。
// 落盘失败只告警一次：内存态已经记住了本次的 IP，下一轮不会重复发同一封邮件。
func storeEmailState(state emailState) {
	normalizeEmailState(&state)
	state.Schema = emailStateSchema

	emailStateLock.Lock()
	shouldWarn, err := persistEmailStateLocked(state)
	emailStateLock.Unlock()

	if shouldWarn {
		util.Log("邮件通知: 状态写入失败 (%s), 已改用内存状态, 本次运行内不会重复提醒", err)
	}
}

// updateEmailState 在锁内完成“读当前状态 → 修改 → 写回”，
// 避免 ResetEmailBackoff / markNotifiedNow / setAlertedState 等函数
// 各自 load→store，互相用旧快照覆盖对方的字段。
func updateEmailState(fn func(*emailState)) {
	emailStateLock.Lock()
	state := loadEmailStateLocked()
	fn(&state)
	normalizeEmailState(&state)
	state.Schema = emailStateSchema
	shouldWarn, err := persistEmailStateLocked(state)
	emailStateLock.Unlock()

	if shouldWarn {
		util.Log("邮件通知: 状态写入失败 (%s), 已改用内存状态, 本次运行内不会重复提醒", err)
	}
}

// commitEmailState 以"当前状态"为基座合并写入，而不是用发信前的快照整体覆盖。
//
// 本轮更早的时候已经落过盘的内容不在 next 里：sendMail 成功时会清零
// FailCount/NextRetryAt，markNotifiedNow/setAlertedState 会写入 LastSent/Alerted。
// 旧实现直接 storeEmailState(next)，等于把这些刚写入的值回滚成发信前的旧值，
// 症状是：发信成功后 failCount 不清零（退避等级被永久抬高）、告警后 alerted
// 被写回 false（重启后同一故障重复告警，且恢复通知发不出去）。
func commitEmailState(next emailState) {
	updateEmailState(func(cur *emailState) {
		cur.Sources = next.Sources
		cur.LastIPv4 = next.LastIPv4
		cur.LastIPv6 = next.LastIPv6
		if next.LastSent != 0 {
			cur.LastSent = next.LastSent
		}
		if next.ChangeNotified {
			cur.ChangeNotified = true
		}
	})
}

// writeEmailStateFile 先写临时文件、fsync、再 rename，避免进程中途被杀或
// 掉电留下半个/空 JSON。调用方必须持有 emailStateLock，保证并发写串行化。
func writeEmailStateFile(state emailState) error {
	byt, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	path := emailStatePath()
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(byt); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ifInet6Path 内核暴露的 IPv6 地址表，测试可覆盖
var ifInet6Path = "/proc/net/if_inet6"

// Linux 地址标志位，见 include/uapi/linux/if_addr.h
const (
	ipv6FlagTemporary  = 0x01 // IFA_F_SECONDARY，IPv6 下即 IFA_F_TEMPORARY
	ipv6FlagDeprecated = 0x20
	ipv6FlagTentative  = 0x40
)

// parseIfInet6 解析 /proc/net/if_inet6 内容，返回 规范化地址 -> 标志位
func parseIfInet6(r io.Reader) map[string]uint32 {
	out := make(map[string]uint32)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || len(fields[0]) != net.IPv6len*2 {
			continue
		}

		buf := make([]byte, net.IPv6len)
		valid := true
		for i := 0; i < net.IPv6len; i++ {
			v, err := strconv.ParseUint(fields[0][i*2:i*2+2], 16, 8)
			if err != nil {
				valid = false
				break
			}
			buf[i] = byte(v)
		}
		if !valid {
			continue
		}

		flags, err := strconv.ParseUint(fields[4], 16, 32)
		if err != nil {
			continue
		}
		out[net.IP(buf).String()] = uint32(flags)
	}
	return out
}

// unstableIPv6Addrs 返回本机上临时/已弃用/待定的 IPv6 地址集合
func unstableIPv6Addrs() map[string]bool {
	f, err := os.Open(ifInet6Path)
	if err != nil {
		return nil
	}
	defer f.Close()

	out := make(map[string]bool)
	for addr, flags := range parseIfInet6(f) {
		if flags&(ipv6FlagTemporary|ipv6FlagDeprecated|ipv6FlagTentative) != 0 {
			out[addr] = true
		}
	}
	return out
}
