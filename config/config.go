package config

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/jeessy2/ddns-go/v6/util"
	passwordvalidator "github.com/wagslane/go-password-validator"
	"gopkg.in/yaml.v3"
)

// Ipv4Reg IPv4正则
var Ipv4Reg = regexp.MustCompile(`((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])`)

// Ipv6Reg IPv6正则
// 这里用于从 URL / 命令输出中“抓取”IPv6 字符串，允许包含 IPv4-mapped IPv6，
// 例如 ::ffff:192.168.1.102。最终是否合法由 net.ParseIP 进一步保证会更稳妥，
// 但这里先放宽提取范围，避免因正则过严导致截断或漏匹配。
var Ipv6Reg = regexp.MustCompile(`([0-9A-Fa-f:.]{2,})`)

// DnsConfig 配置
type DnsConfig struct {
	Name string
	Ipv4 struct {
		Enable bool
		// 获取IP类型 url/netInterface
		GetType      string
		URL          string
		NetInterface string
		Cmd          string
		Domains      []string
	}
	Ipv6 struct {
		Enable bool
		// 获取IP类型 url/netInterface
		GetType      string
		URL          string
		NetInterface string
		Cmd          string
		Ipv6Reg      string // ipv6匹配正则表达式
		Domains      []string
	}
	DNS DNS
	TTL string
	// 发送HTTP请求时使用的网卡名称，为空则使用默认网卡
	HttpInterface string
}

// DNS DNS配置
type DNS struct {
	// 名称。如：alidns,webhook
	Name   string
	ID     string
	Secret string
	// ExtParam 扩展参数，用于某些DNS提供商的特殊需求（如Vercel的teamId）
	ExtParam string
}

type Config struct {
	DnsConf []DnsConfig
	User
	Webhook
	// 邮件通知（IP 变更提醒）
	Email Email
	// 禁止公网访问
	NotAllowWanAccess bool
	// 语言
	Lang string
}

// ConfigCache ConfigCache
type cacheType struct {
	ConfigSingle *Config
	Err          error
	Lock         sync.Mutex
}

var cache = &cacheType{}

// GetConfigCached 获得缓存的配置
func GetConfigCached() (conf Config, err error) {
	cache.Lock.Lock()
	defer cache.Lock.Unlock()

	if cache.ConfigSingle != nil {
		return *cache.ConfigSingle, cache.Err
	}

	// init config
	cache.ConfigSingle = &Config{}

	configFilePath := util.GetConfigFilePath()
	_, err = os.Stat(configFilePath)
	if err != nil {
		cache.Err = err
		return *cache.ConfigSingle, err
	}

	byt, err := os.ReadFile(configFilePath)
	if err != nil {
		util.Log("异常信息: %s", err)
		cache.Err = err
		return *cache.ConfigSingle, err
	}

	err = yaml.Unmarshal(byt, cache.ConfigSingle)
	if err != nil {
		util.Log("异常信息: %s", err)
		cache.Err = err
		return *cache.ConfigSingle, err
	}

	// fork 默认语言：配置里没写语言时用中文，界面与日志都以中文呈现
	if strings.TrimSpace(cache.ConfigSingle.Lang) == "" {
		cache.ConfigSingle.Lang = "zh"
	}

	// 未填写登录信息, 确保不能从公网访问
	if cache.ConfigSingle.Username == "" && cache.ConfigSingle.Password == "" {
		cache.ConfigSingle.NotAllowWanAccess = true
	}

	// remove err
	cache.Err = nil
	return *cache.ConfigSingle, err
}

// CompatibleConfig 兼容之前的配置文件
func (conf *Config) CompatibleConfig() {

	// fork 默认语言：配置里没有语言时用中文（否则界面和日志都会退回英文）
	if strings.TrimSpace(conf.Lang) == "" {
		conf.Lang = "zh"
	}

	// 如果之前密码不为空且不是bcrypt加密后的密码, 把密码加密并保存
	if conf.Password != "" && !util.IsHashedPassword(conf.Password) {
		hashedPwd, err := util.HashPassword(conf.Password)
		if err == nil {
			conf.Password = hashedPwd
			conf.SaveConfig()
		}
	}

	// 兼容v5.0.0之前的配置文件
	if len(conf.DnsConf) > 0 {
		return
	}

	configFilePath := util.GetConfigFilePath()
	_, err := os.Stat(configFilePath)
	if err != nil {
		return
	}
	byt, err := os.ReadFile(configFilePath)
	if err != nil {
		return
	}

	dnsConf := &DnsConfig{}
	err = yaml.Unmarshal(byt, dnsConf)
	if err != nil {
		return
	}
	if len(dnsConf.DNS.Name) > 0 {
		cache.Lock.Lock()
		defer cache.Lock.Unlock()
		conf.DnsConf = append(conf.DnsConf, *dnsConf)
		cache.ConfigSingle = conf
	}
}

// ResetConfigCache 清空配置缓存，下次读取时重新加载配置文件（测试用）
func ResetConfigCache() {
	cache.Lock.Lock()
	defer cache.Lock.Unlock()
	cache.ConfigSingle = nil
	cache.Err = nil
}

// SaveConfig 保存配置
func (conf *Config) SaveConfig() (err error) {
	cache.Lock.Lock()
	defer cache.Lock.Unlock()

	byt, err := yaml.Marshal(conf)
	if err != nil {
		log.Println(err)
		return err
	}

	configFilePath := util.GetConfigFilePath()
	// 原子写：先写临时文件并落盘，再 rename 覆盖。
	// 直接 WriteFile 会先截断原文件，写到一半断电/磁盘满就留下半截 YAML，下次启动直接解析失败。
	tmpPath := configFilePath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		log.Println(err)
		return
	}
	if _, err = f.Write(byt); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		log.Println(err)
		return
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		log.Println(err)
		return
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		log.Println(err)
		return
	}
	if err = os.Rename(tmpPath, configFilePath); err != nil {
		_ = os.Remove(tmpPath)
		log.Println(err)
		return
	}

	util.Log("配置文件已保存在: %s", configFilePath)

	// 清空配置缓存
	cache.ConfigSingle = nil

	return
}

// 重置密码
func (conf *Config) ResetPassword(newPassword string) {
	// 初始化语言
	util.InitLogLang(conf.Lang)

	// 先检查密码是否安全
	hashedPwd, err := conf.CheckPassword(newPassword)
	if err != nil {
		util.Log(err.Error())
		return
	}

	// 保存配置
	conf.Password = hashedPwd
	conf.SaveConfig()
	util.Log("用户名 %s 的密码已重置成功! 请重启ddns-go", conf.Username)
}

// CheckPassword 检查密码
func (conf *Config) CheckPassword(newPassword string) (hashedPwd string, err error) {
	return conf.CheckPasswordWithLimit(newPassword, conf.NotAllowWanAccess)
}

// CheckPasswordWithLimit 按调用方给定的“是否仅内网访问”计算强度阈值。
// 保存配置时必须传入保存前的值：同一次请求里把 NotAllowWanAccess 改成 true
// 可以把阈值从 30bit 降到 25bit，之后取消勾选就留下一个弱密码。
func (conf *Config) CheckPasswordWithLimit(newPassword string, lanOnly bool) (hashedPwd string, err error) {
	var minEntropyBits float64 = 30
	if lanOnly {
		minEntropyBits = 25
	}
	err = passwordvalidator.Validate(newPassword, minEntropyBits)
	if err != nil {
		return "", errors.New(util.LogStr("密码不安全！尝试使用更复杂的密码"))
	}

	// 加密密码
	hashedPwd, err = util.HashPassword(newPassword)
	if err != nil {
		return "", errors.New(util.LogStr("异常信息: %s", err.Error()))
	}
	return
}

func (conf *DnsConfig) getIpv4AddrFromInterface() string {
	ipv4, _, err := GetNetInterface()
	if err != nil {
		util.Log("从网卡获得IPv4失败")
		return ""
	}

	for _, netInterface := range ipv4 {
		if netInterface.Name == conf.Ipv4.NetInterface && len(netInterface.Address) > 0 {
			return netInterface.Address[0]
		}
	}

	util.Log("从网卡中获得IPv4失败! 网卡名: %s", conf.Ipv4.NetInterface)
	return ""
}

// getIpv4AddrFromUrl 依次尝试多个接口地址。
// 只有真正解析出地址才算成功：连接失败、读体失败、返回内容里没有 IP，
// 都会继续尝试下一个接口（原版这几处会直接返回空，导致备用源形同虚设）。
func (conf *DnsConfig) getIpv4AddrFromUrl() string {
	client := util.CreateBoundNoProxyHTTPClient("tcp4", conf.HttpInterface)
	for _, url := range strings.Split(conf.Ipv4.URL, ",") {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		if addr := fetchIPv4FromURL(client, url); addr != "" {
			return addr
		}
	}
	return ""
}

// fetchIPv4FromURL 请求单个接口并解析出 IPv4，任何一步失败都返回空字符串
func fetchIPv4FromURL(client *http.Client, url string) string {
	resp, err := client.Get(url)
	if err != nil {
		util.Log("通过接口获取IPv4失败! 接口地址: %s", url)
		util.Log("异常信息: %s", err)
		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024000))
	if err != nil {
		util.Log("异常信息: %s", err)
		return ""
	}

	result := Ipv4Reg.FindString(string(body))
	if result == "" {
		util.Log("获取IPv4结果失败! 接口: %s ,返回值: %s", url, truncateForLog(string(body), 120))
		return ""
	}
	return result
}

func findIPv6InText(text string) string {
	for _, candidate := range Ipv6Reg.FindAllString(text, -1) {
		if ip := net.ParseIP(candidate); ip != nil && ip.To4() == nil {
			return candidate
		}
	}
	return ""
}

func (conf *DnsConfig) getAddrFromCmd(addrType string) string {
	var cmd string
	var comp *regexp.Regexp
	if addrType == "IPv4" {
		cmd = conf.Ipv4.Cmd
		comp = Ipv4Reg
	} else {
		cmd = conf.Ipv6.Cmd
		comp = Ipv6Reg
	}
	// cmd is empty
	if cmd == "" {
		return ""
	}
	// run cmd with proper shell
	var execCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		execCmd = exec.Command("powershell", "-Command", cmd)
	} else {
		// If Bash does not exist, use sh
		_, err := exec.LookPath("bash")
		if err != nil {
			execCmd = exec.Command("sh", "-c", cmd)
		} else {
			execCmd = exec.Command("bash", "-c", cmd)
		}
	}
	// run cmd
	out, err := execCmd.CombinedOutput()
	if err != nil {
		util.Log("获取%s结果失败! 未能成功执行命令：%s, 错误：%q, 退出状态码：%s", addrType, execCmd.String(), out, err)
		return ""
	}
	str := string(out)
	// get result
	result := ""
	if addrType == "IPv4" {
		result = comp.FindString(str)
	} else {
		result = findIPv6InText(str)
	}
	if result == "" {
		util.Log("获取%s结果失败! 命令: %s, 标准输出: %q", addrType, execCmd.String(), str)
	}
	return result
}

// GetIpv4Addr 获得IPv4地址
func (conf *DnsConfig) GetIpv4Addr() string {
	// 判断从哪里获取IP
	switch conf.Ipv4.GetType {
	case "netInterface":
		// 从网卡获取 IP
		return conf.getIpv4AddrFromInterface()
	case "url":
		// 从 URL 获取 IP
		return conf.getIpv4AddrFromUrl()
	case "cmd":
		// 从命令行获取 IP
		return conf.getAddrFromCmd("IPv4")
	default:
		log.Println("IPv4's get IP method is unknown")
		return "" // unknown type
	}
}

func (conf *DnsConfig) getIpv6AddrFromInterface() string {
	_, ipv6, err := GetNetInterface()
	if err != nil {
		util.Log("从网卡获得IPv6失败")
		return ""
	}

	for _, netInterface := range ipv6 {
		if netInterface.Name == conf.Ipv6.NetInterface && len(netInterface.Address) > 0 {
			if conf.Ipv6.Ipv6Reg != "" {
				// 匹配第几个IPv6
				if match, err := regexp.MatchString("@\\d", conf.Ipv6.Ipv6Reg); err == nil && match {
					num, err := strconv.Atoi(conf.Ipv6.Ipv6Reg[1:])
					if err == nil {
						if num > 0 {
							if num <= len(netInterface.Address) {
								return netInterface.Address[num-1]
							}
							util.Log("未找到第 %d 个IPv6地址! 将使用第一个IPv6地址", num)
							return netInterface.Address[0]
						}
						util.Log("IPv6匹配表达式 %s 不正确! 最小从1开始", conf.Ipv6.Ipv6Reg)
						return ""
					}
				}
				// 正则表达式匹配
				util.Log("IPv6将使用正则表达式 %s 进行匹配", conf.Ipv6.Ipv6Reg)
				for i := 0; i < len(netInterface.Address); i++ {
					matched, err := regexp.MatchString(conf.Ipv6.Ipv6Reg, netInterface.Address[i])
					if matched && err == nil {
						util.Log("匹配成功! 匹配到地址: %s", netInterface.Address[i])
						return netInterface.Address[i]
					}
				}
				util.Log("没有匹配到任何一个IPv6地址, 将使用第一个地址")
			}
			return netInterface.Address[0]
		}
	}

	util.Log("从网卡中获得IPv6失败! 网卡名: %s", conf.Ipv6.NetInterface)
	return ""
}

// getIpv6AddrFromUrl 同 getIpv4AddrFromUrl，取不到地址时继续尝试下一个接口
func (conf *DnsConfig) getIpv6AddrFromUrl() string {
	client := util.CreateBoundNoProxyHTTPClient("tcp6", conf.HttpInterface)
	for _, url := range strings.Split(conf.Ipv6.URL, ",") {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		if addr := fetchIPv6FromURL(client, url); addr != "" {
			return addr
		}
	}
	return ""
}

// fetchIPv6FromURL 请求单个接口并解析出 IPv6，任何一步失败都返回空字符串
func fetchIPv6FromURL(client *http.Client, url string) string {
	resp, err := client.Get(url)
	if err != nil {
		util.Log("通过接口获取IPv6失败! 接口地址: %s", url)
		util.Log("异常信息: %s", err)
		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024000))
	if err != nil {
		util.Log("异常信息: %s", err)
		return ""
	}

	result := findIPv6InText(string(body))
	if result == "" {
		util.Log("获取IPv6结果失败! 接口: %s ,返回值: %s", url, truncateForLog(string(body), 120))
		return ""
	}
	return result
}

// truncateForLog 截断日志里的响应体，避免把整页 HTML 写进日志
func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "...(truncated)"
}

// GetIpv6Addr 获得IPv6地址
func (conf *DnsConfig) GetIpv6Addr() (result string) {
	// 判断从哪里获取IP
	switch conf.Ipv6.GetType {
	case "netInterface":
		// 从网卡获取 IP
		return conf.getIpv6AddrFromInterface()
	case "url":
		// 从 URL 获取 IP
		return conf.getIpv6AddrFromUrl()
	case "cmd":
		// 从命令行获取 IP
		return conf.getAddrFromCmd("IPv6")
	default:
		log.Println("IPv6's get IP method is unknown")
		return "" // unknown type
	}
}

// GetHTTPClient 获得HTTP客户端，如果配置了HttpInterface则绑定到指定网卡
func (conf *DnsConfig) GetHTTPClient() *http.Client {
	return util.CreateHTTPClientWithInterface(conf.HttpInterface)
}
