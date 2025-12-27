package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Config 存储用户配置
type Config struct {
	CronSpec      string  `json:"cron_spec"`      // Cron 表达式
	ZoneID        string  `json:"zone_id"`        // Cloudflare Zone ID
	APIKey        string  `json:"api_key"`        // Global API Key
	Email         string  `json:"email"`          // Cloudflare 邮箱
	Domains       string  `json:"domains"`        // 域名列表 (逗号分隔)
	
	// 测速参数
	DownloadURL   string  `json:"download_url"`   // 测速地址
	TestCount     int     `json:"test_count"`     // -dn 测速数量
	MaxResult     int     `json:"max_result"`     // 单域名解析IP数量(默认10)
	MinSpeed      float64 `json:"min_speed"`      // -sl 速度下限
	MaxDelay      int     `json:"max_delay"`      // -tl 延迟上限
	IPType        string  `json:"ip_type"`        // "v4", "v6", "both"
	Colo          string  `json:"colo"`           // 地区码
	EnableHTTPing bool    `json:"enable_httping"` // HTTPing
}

var (
	dataDir    = "/app/data"
	configFile = filepath.Join(dataDir, "config.json")
	logFile    = filepath.Join(dataDir, "app.log")
	cfstFile   = filepath.Join(dataDir, "cfst")
	ip4File    = filepath.Join(dataDir, "ip.txt")
	ip6File    = filepath.Join(dataDir, "ipv6.txt")
	resultFile = filepath.Join(dataDir, "result.csv")
	
	config     Config
	mutex      sync.Mutex // 配置锁
	runMutex   sync.Mutex // 运行锁
	cronRunner *cron.Cron
)

func main() {
	os.MkdirAll(dataDir, 0755)
	
	// 初始化日志文件
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		os.WriteFile(logFile, []byte("服务启动...\n"), 0644)
	}

	loadConfig()

	cronRunner = cron.New()
	updateCron()
	cronRunner.Start()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/save", handleSave)
	http.HandleFunc("/api/upload", handleUpload)
	http.HandleFunc("/api/run", handleRunNow)
	http.HandleFunc("/api/logs", handleLogs) // 增量日志接口
	http.HandleFunc("/api/status", handleStatus)

	writeLog("Web server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// === 核心逻辑 ===

func runSpeedTestAndUpdateDNS() {
	// 防止重入
	if !runMutex.TryLock() {
		writeLog("任务正在运行中，跳过本次请求")
		return
	}
	defer runMutex.Unlock()

	writeLog("=== 开始执行测速任务 ===")

	// 1. 检查文件
	if _, err := os.Stat(cfstFile); os.IsNotExist(err) {
		writeLog("错误: 找不到 cfst 可执行文件")
		return
	}
	os.Chmod(cfstFile, 0755)

	targetIPFile := ip4File
	if config.IPType == "v6" {
		targetIPFile = ip6File
	} else if config.IPType == "both" {
		targetIPFile = filepath.Join(dataDir, "ip_combined.txt")
		combineFiles(targetIPFile, ip4File, ip6File)
	}

	if _, err := os.Stat(targetIPFile); os.IsNotExist(err) {
		writeLog("错误: 找不到 IP 库文件")
		return
	}

	// 2. 准备参数
	// 解析域名列表
	domainList := parseDomains(config.Domains)
	if len(domainList) == 0 {
		writeLog("错误: 未配置域名")
		return
	}

	// 确定需要获取的 IP 数量
	// 逻辑：如果是多域名，需要 len(domains) 个 IP；如果是单域名，需要 config.MaxResult 个 IP
	requiredCount := config.MaxResult
	if requiredCount <= 0 { requiredCount = 10 }
	
	// 如果多域名且数量超过 MaxResult，则以域名数量为准（保证每个域名至少有1个IP）
	if len(domainList) > 1 && len(domainList) > requiredCount {
		requiredCount = len(domainList)
	}

	// -dn 参数至少要比 requiredCount 大一点，或者相等，这里直接用 config.TestCount
	// 如果用户设置的测速数量小于需求，强制调大
	testCount := config.TestCount
	if testCount < requiredCount {
		testCount = requiredCount
		writeLog(fmt.Sprintf("提示: 测速数量(-dn)自动调整为 %d 以满足域名解析需求", testCount))
	}

	args := []string{
		"-o", resultFile,
		"-dn", fmt.Sprintf("%d", testCount),
		"-sl", fmt.Sprintf("%.2f", config.MinSpeed),
		"-tl", fmt.Sprintf("%d", config.MaxDelay),
		"-f", targetIPFile,
	}

	if config.DownloadURL != "" { args = append(args, "-url", config.DownloadURL) }
	if config.Colo != "" {
		args = append(args, "-cfcolo", config.Colo)
		if !config.EnableHTTPing { args = append(args, "-httping") }
	}
	if config.EnableHTTPing && !sliceContains(args, "-httping") { args = append(args, "-httping") }

	// 3. 执行测速
	cmd := exec.Command(cfstFile, args...)
	cmd.Dir = dataDir
	
	// 实时捕获输出写入日志
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()
	
	if err := cmd.Start(); err != nil {
		writeLog(fmt.Sprintf("启动测速失败: %v", err))
		return
	}

	// 异步读取输出流到日志
	go io.Copy(getLogWriter(), stdoutPipe)
	go io.Copy(getLogWriter(), stderrPipe)

	if err := cmd.Wait(); err != nil {
		writeLog(fmt.Sprintf("测速命令执行出错 (通常是没找到满足条件的IP): %v", err))
		// 注意：CFST 如果没找到 IP 有时会返回非 0，继续尝试读取 CSV 看是否有部分结果
	}

	// 4. 解析结果
	ips := parseResultCSV(resultFile, requiredCount)
	if len(ips) == 0 {
		writeLog("失败: 未获取到任何有效 IP")
		return
	}
	writeLog(fmt.Sprintf("获取到 %d 个优选 IP", len(ips)))

	// 5. 更新 DNS (核心逻辑修改)
	updateDNSStrategy(domainList, ips)
	
	writeLog("=== 任务完成 ===")
}

// DNS 更新策略
func updateDNSStrategy(domains []string, ips []string) {
	if config.ZoneID == "" || config.APIKey == "" {
		writeLog("跳过 DNS 更新: API 配置缺失")
		return
	}

	// 场景 A: 只有一个域名 -> 负载均衡模式 (将 Top N IP 全部解析到该域名)
	if len(domains) == 1 {
		domain := domains[0]
		// 截取配置的最大数量
		limit := config.MaxResult
		if limit <= 0 { limit = 10 }
		if len(ips) > limit { ips = ips[:limit] }
		
		writeLog(fmt.Sprintf("正在更新域名 [%s] (负载均衡模式, IP数量: %d)...", domain, len(ips)))
		updateCloudflareDNS(domain, ips)
		return
	}

	// 场景 B: 多个域名 -> 1对1 映射模式
	// 域名1 <- IP1, 域名2 <- IP2 ...
	writeLog(fmt.Sprintf("正在更新 %d 个域名 (1对1 极速映射模式)...", len(domains)))
	for i, domain := range domains {
		if i >= len(ips) {
			writeLog(fmt.Sprintf("警告: IP 数量不足，跳过域名 [%s]", domain))
			break
		}
		selectedIP := []string{ips[i]} // 取对应排名的 IP
		writeLog(fmt.Sprintf(" -> 域名 [%s] 解析到 IP [%s] (排名 #%d)", domain, ips[i], i+1))
		updateCloudflareDNS(domain, selectedIP)
	}
}

// 通用 CF 更新函数 (先删后加)
func updateCloudflareDNS(domain string, newIPs []string) {
	// 1. 获取该域名所有 A/AAAA 记录
	records, err := getDNSRecords(domain)
	if err != nil {
		writeLog(fmt.Sprintf("[%s] 获取记录失败: %v", domain, err))
		return
	}

	// 2. 删除旧记录
	for _, r := range records {
		deleteDNSRecord(r)
	}

	// 3. 添加新记录
	for _, ip := range newIPs {
		createDNSRecord(domain, ip)
	}
}

// --- 辅助函数 ---

func parseDomains(input string) []string {
	parts := strings.Split(input, ",")
	var res []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" { res = append(res, t) }
	}
	return res
}

func parseResultCSV(file string, max int) []string {
	f, err := os.Open(file)
	if err != nil { return nil }
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil { return nil }

	var ips []string
	// 跳过标题，取第一列
	for i, row := range records {
		if i == 0 { continue }
		if len(ips) >= max { break }
		if len(row) > 0 { ips = append(ips, row[0]) }
	}
	return ips
}

// CF API Helpers
func getDNSRecords(domain string) ([]string, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s", config.ZoneID, domain)
	req, _ := http.NewRequest("GET", url, nil)
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()

	var res struct {
		Result []struct { ID string `json:"id"` } `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	var ids []string
	for _, r := range res.Result { ids = append(ids, r.ID) }
	return ids, nil
}

func deleteDNSRecord(id string) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", config.ZoneID, id)
	req, _ := http.NewRequest("DELETE", url, nil)
	setHeaders(req)
	http.DefaultClient.Do(req)
}

func createDNSRecord(domain, ip string) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", config.ZoneID)
	typeStr := "A"
	if strings.Contains(ip, ":") { typeStr = "AAAA" }
	payload := map[string]interface{}{
		"type": typeStr, "name": domain, "content": ip, "ttl": 60, "proxied": false,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	setHeaders(req)
	http.DefaultClient.Do(req)
}

func setHeaders(req *http.Request) {
	req.Header.Set("X-Auth-Email", config.Email)
	req.Header.Set("X-Auth-Key", config.APIKey)
	req.Header.Set("Content-Type", "application/json")
}

// --- 日志系统 (文件版) ---

// LogWriter 实现 io.Writer 接口，直接写文件
type LogWriter struct{}
func (l LogWriter) Write(p []byte) (n int, err error) {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil { return 0, err }
	defer f.Close()
	return f.Write(p)
}
func getLogWriter() io.Writer { return LogWriter{} }

func writeLog(msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("[%s] %s\n", ts, msg)
	fmt.Print(line) // 输出到 Docker console
	
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		f.WriteString(line)
		f.Close()
	}
}

// 增量日志 Handler
func handleLogs(w http.ResponseWriter, r *http.Request) {
	offsetStr := r.URL.Query().Get("offset")
	offset, _ := strconv.ParseInt(offsetStr, 10, 64)

	f, err := os.Open(logFile)
	if err != nil { return }
	defer f.Close()

	info, _ := f.Stat()
	fileSize := info.Size()

	// 如果前端 offset 大于文件大小 (文件被重置)，从头读
	if offset > fileSize { offset = 0 }

	f.Seek(offset, 0)
	content, _ := io.ReadAll(f)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"log": string(content),
		"offset": offset + int64(len(content)),
	})
}

// --- 其他 Handler ---
// (省略部分未变动的辅助函数, 完整代码需包含 combineFiles, sliceContains 等)

func handleSave(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()
	config.CronSpec = r.FormValue("cron_spec")
	config.ZoneID = r.FormValue("zone_id")
	config.APIKey = r.FormValue("api_key")
	config.Email = r.FormValue("email")
	config.Domains = r.FormValue("domains") // 变更
	config.DownloadURL = r.FormValue("download_url")
	config.IPType = r.FormValue("ip_type")
	config.Colo = strings.ToUpper(r.FormValue("colo"))
	config.EnableHTTPing = (r.FormValue("enable_httping") == "on")
	
	fmt.Sscanf(r.FormValue("test_count"), "%d", &config.TestCount)
	fmt.Sscanf(r.FormValue("max_result"), "%d", &config.MaxResult)
	fmt.Sscanf(r.FormValue("min_speed"), "%f", &config.MinSpeed)
	fmt.Sscanf(r.FormValue("max_delay"), "%d", &config.MaxDelay)

	saveConfig()
	updateCron()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFiles("index.html")
	mutex.Lock()
	defer mutex.Unlock()
	if config.MaxResult == 0 { config.MaxResult = 10 } // 默认值
	tmpl.Execute(w, config)
}

// 以下函数复用之前的逻辑
func loadConfig() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		config = Config{CronSpec: "0 * * * *", TestCount: 10, MaxResult: 10, IPType: "v4"}
		return
	}
	f, _ := os.Open(configFile)
	json.NewDecoder(f).Decode(&config)
