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
	MainDomain    string  `json:"main_domain"`    // 主域名 (Zone Name)
	Domains       string  `json:"domains"`        // 优选域名列表
	
	// 测速参数
	DownloadURL   string  `json:"download_url"`
	TestCount     int     `json:"test_count"`
	MaxResult     int     `json:"max_result"`
	MinSpeed      float64 `json:"min_speed"`
	MaxDelay      int     `json:"max_delay"`
	MinDelay      int     `json:"min_delay"`
	TestPort      int     `json:"test_port"`
	IPType        string  `json:"ip_type"`
	Colo          string  `json:"colo"`
	EnableHTTPing bool    `json:"enable_httping"`
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
	mutex      sync.Mutex
	runMutex   sync.Mutex
	cronRunner *cron.Cron
)

func main() {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("无法创建数据目录: %v", err)
	}
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		os.WriteFile(logFile, []byte("服务初始化成功...\n"), 0644)
	}

	loadConfig()

	cronRunner = cron.New()
	updateCron()
	cronRunner.Start()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/save", handleSave)
	http.HandleFunc("/api/upload", handleUpload)
	http.HandleFunc("/api/run", handleRunNow)
	http.HandleFunc("/api/logs", handleLogs)
	http.HandleFunc("/api/status", handleStatus)

	writeLog(fmt.Sprintf("Web server running on :8080 (Version: %s)", "1.5.0"))
	log.Println("Web server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// === 核心业务逻辑 ===

func runSpeedTestAndUpdateDNS() {
	if !runMutex.TryLock() {
		writeLog("⚠️ 任务正在运行中，跳过本次请求")
		return
	}
	defer runMutex.Unlock()

	writeLog("=== 开始执行测速任务 ===")

	// 1. 检查文件
	if _, err := os.Stat(cfstFile); os.IsNotExist(err) {
		writeLog("❌ 错误: 找不到 cfst 可执行文件")
		return
	}
	os.Chmod(cfstFile, 0755)

	targetIPFile := ip4File
	if config.IPType == "v6" {
		targetIPFile = ip6File
	} else if config.IPType == "both" {
		targetIPFile = filepath.Join(dataDir, "ip_combined.txt")
		if err := combineFiles(targetIPFile, ip4File, ip6File); err != nil {
			writeLog(fmt.Sprintf("❌ IP合并失败: %v", err))
			return
		}
	}

	if _, err := os.Stat(targetIPFile); os.IsNotExist(err) {
		writeLog("❌ 错误: 找不到 IP 库文件")
		return
	}

	// 2. 准备域名和Zone信息
	domainList := parseDomains(config.Domains)
	if len(domainList) == 0 {
		writeLog("❌ 错误: 未配置优选域名")
		return
	}

	zoneName := config.MainDomain
	if zoneName == "" && config.ZoneID != "" {
		fetched, err := fetchZoneName()
		if err == nil {
			zoneName = fetched
			writeLog(fmt.Sprintf("✅ 自动识别主域名: %s", zoneName))
		} else {
			writeLog(fmt.Sprintf("⚠️ 自动识别主域名失败: %v", err))
		}
	} else {
		writeLog(fmt.Sprintf("✅ 使用配置的主域名: %s", zoneName))
	}

	// 3. 测速参数
	requiredCount := config.MaxResult
	if requiredCount <= 0 { requiredCount = 10 }
	if len(domainList) > 1 && len(domainList) > requiredCount {
		requiredCount = len(domainList)
	}

	testCount := config.TestCount
	if testCount < requiredCount {
		testCount = requiredCount
		writeLog(fmt.Sprintf("ℹ️ 测速数量自动调整为 %d", testCount))
	}

	port := config.TestPort
	if port == 0 { port = 443 }

	args := []string{
		"-o", resultFile,
		"-dn", fmt.Sprintf("%d", testCount),
		"-sl", fmt.Sprintf("%.2f", config.MinSpeed),
		"-tl", fmt.Sprintf("%d", config.MaxDelay),
		"-tll", fmt.Sprintf("%d", config.MinDelay),
		"-tp", fmt.Sprintf("%d", port),
		"-f", targetIPFile,
	}

	if config.DownloadURL != "" { args = append(args, "-url", config.DownloadURL) }
	if config.Colo != "" {
		args = append(args, "-cfcolo", config.Colo)
		if !config.EnableHTTPing { args = append(args, "-httping") }
	}
	if config.EnableHTTPing && !sliceContains(args, "-httping") { args = append(args, "-httping") }

	writeLog(fmt.Sprintf("🚀 执行命令: cfst %v", strings.Join(args, " ")))

	// 4. 运行
	cmd := exec.Command(cfstFile, args...)
	cmd.Dir = dataDir
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		writeLog(fmt.Sprintf("❌ 启动失败: %v", err))
		return
	}
	go io.Copy(getLogWriter(), stdoutPipe)
	go io.Copy(getLogWriter(), stderrPipe)
	cmd.Wait()

	// 5. 结果处理
	ips := parseResultCSV(resultFile, requiredCount)
	if len(ips) == 0 {
		writeLog("❌ 失败: 未获取到有效 IP")
		return
	}
	writeLog(fmt.Sprintf("✅ 获取到 %d 个优选 IP", len(ips)))

	// 6. 更新 DNS
	updateDNSStrategy(domainList, ips, zoneName)
	writeLog("=== 任务完成 ===")
}

func updateDNSStrategy(domains []string, ips []string, zoneName string) {
	if config.ZoneID == "" || config.APIKey == "" {
		writeLog("⚠️ API 配置缺失，跳过更新")
		return
	}

	if len(domains) == 1 {
		domain := domains[0]
		limit := config.MaxResult
		if limit <= 0 { limit = 10 }
		if len(ips) > limit { ips = ips[:limit] }
		
		writeLog(fmt.Sprintf("📡 更新域名 [%s] (负载均衡模式)...", domain))
		updateCloudflareDNS(domain, ips, zoneName)
		return
	}

	writeLog(fmt.Sprintf("📡 更新 %d 个域名 (1对1 分发模式)...", len(domains)))
	for i, domain := range domains {
		if i >= len(ips) { break }
		writeLog(fmt.Sprintf(" -> [%s] 解析至 [%s]", domain, ips[i]))
		updateCloudflareDNS(domain, []string{ips[i]}, zoneName)
	}
}

func updateCloudflareDNS(domain string, newIPs []string, zoneName string) {
	// 1. 获取现有记录 (关键修改：增加 per_page=100)
	records, err := getDNSRecords(domain)
	if err != nil {
		writeLog(fmt.Sprintf("❌ 获取旧记录失败 [%s]: %v", domain, err))
		return
	}

	if len(records) > 0 {
		writeLog(fmt.Sprintf("🗑️ 发现 %d 条旧记录，正在清理...", len(records)))
	} else {
		writeLog(fmt.Sprintf("ℹ️ 未发现旧记录 [%s]", domain))
	}

	// 2. 删除旧记录
	for _, r := range records {
		if err := deleteDNSRecord(r); err != nil {
			writeLog(fmt.Sprintf("⚠️ 删除记录失败 (ID: %s): %v", r, err))
		}
	}

	// 3. 计算记录名 (处理双重后缀)
	recordName := domain
	if zoneName != "" {
		domainLower := strings.ToLower(domain)
		zoneLower := strings.ToLower(zoneName)
		if domainLower == zoneLower {
			recordName = "@"
		} else if strings.HasSuffix(domainLower, "."+zoneLower) {
			recordName = domain[:len(domain)-len(zoneLower)-1]
		}
	}

	// 4. 创建新记录
	for _, ip := range newIPs {
		if err := createDNSRecord(recordName, ip); err != nil {
			writeLog(fmt.Sprintf("❌ 创建记录失败 [%s -> %s]: %v", recordName, ip, err))
		}
	}
	writeLog(fmt.Sprintf("✅ 已添加 %d 条新记录 [%s]", len(newIPs), domain))
}

// --- Cloudflare API ---

func fetchZoneName() (string, error) {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s", config.ZoneID)
	req, _ := http.NewRequest("GET", url, nil)
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return "", err }
	defer resp.Body.Close()

	var res struct {
		Success bool `json:"success"`
		Result struct { Name string `json:"name"` } `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil { return "", err }
	if !res.Success { return "", fmt.Errorf("request failed") }
	return res.Result.Name, nil
}

func getDNSRecords(domain string) ([]string, error) {
	// 关键修改：添加 per_page=100，确保能一次性获取并删除所有积压的记录
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&per_page=100", config.ZoneID, domain)
	req, _ := http.NewRequest("GET", url, nil)
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()

	var res struct {
		Success bool `json:"success"`
		Result []struct { ID string `json:"id"` } `json:"result"`
		Errors []interface{} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil { return nil, err }
	if !res.Success { return nil, fmt.Errorf("api error: %v", res.Errors) }
	
	var ids []string
	for _, r := range res.Result { ids = append(ids, r.ID) }
	return ids, nil
}

func deleteDNSRecord(id string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", config.ZoneID, id)
	req, _ := http.NewRequest("DELETE", url, nil)
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close() // 关键修改：及时关闭连接
	
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	return nil
}

func createDNSRecord(name, ip string) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", config.ZoneID)
	typeStr := "A"
	if strings.Contains(ip, ":") { typeStr = "AAAA" }
	
	payload := map[string]interface{}{
		"type": typeStr, "name": name, "content": ip, "ttl": 60, "proxied": false,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close() // 关键修改：及时关闭连接
	
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	return nil
}

func setHeaders(req *http.Request) {
	req.Header.Set("X-Auth-Email", config.Email)
	req.Header.Set("X-Auth-Key", config.APIKey)
	req.Header.Set("Content-Type", "application/json")
}

// --- 辅助与处理 ---

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
	for i, row := range records {
		if i == 0 { continue }
		if len(ips) >= max { break }
		if len(row) > 0 { ips = append(ips, row[0]) }
	}
	return ips
}

func combineFiles(dst string, src ...string) error {
	out, err := os.Create(dst)
	if err != nil { return err }
	defer out.Close()
	for _, s := range src {
		in, err := os.Open(s); if err == nil { io.Copy(out, in); in.Close(); out.Write([]byte("\n")) }
	}
	return nil
}
func sliceContains(s []string, e string) bool { for _, a := range s { if a == e { return true } }; return false }

// --- 日志与Web ---
type LogWriter struct{}
func (l LogWriter) Write(p []byte) (n int, err error) {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil { return 0, err }
	defer f.Close()
	fmt.Print(string(p))
	return f.Write(p)
}
func getLogWriter() io.Writer { return LogWriter{} }
func writeLog(msg string) { getLogWriter().Write([]byte(fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg))) }

func handleLogs(w http.ResponseWriter, r *http.Request) {
	offsetStr := r.URL.Query().Get("offset")
	offset, _ := strconv.ParseInt(offsetStr, 10, 64)
	f, err := os.Open(logFile); if err != nil { return }; defer f.Close()
	info, _ := f.Stat()
	if offset > info.Size() { offset = 0 }
	f.Seek(offset, 0)
	content, _ := io.ReadAll(f)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"log": string(content), "offset": offset + int64(len(content))})
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()
	config.CronSpec = r.FormValue("cron_spec")
	config.ZoneID = r.FormValue("zone_id")
	config.APIKey = r.FormValue("api_key")
	config.Email = r.FormValue("email")
	config.MainDomain = strings.TrimSpace(r.FormValue("main_domain"))
	config.Domains = r.FormValue("domains")
	config.DownloadURL = r.FormValue("download_url")
	config.IPType = r.FormValue("ip_type")
	config.Colo = strings.ToUpper(r.FormValue("colo"))
	config.EnableHTTPing = (r.FormValue("enable_httping") == "on")
	fmt.Sscanf(r.FormValue("test_count"), "%d", &config.TestCount)
	fmt.Sscanf(r.FormValue("max_result"), "%d", &config.MaxResult)
	fmt.Sscanf(r.FormValue("min_speed"), "%f", &config.MinSpeed)
	fmt.Sscanf(r.FormValue("max_delay"), "%d", &config.MaxDelay)
	fmt.Sscanf(r.FormValue("min_delay"), "%d", &config.MinDelay)
	fmt.Sscanf(r.FormValue("test_port"), "%d", &config.TestPort)
	saveConfig()
	updateCron()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { return }
	file, _, err := r.FormFile("file"); if err != nil { return }
	defer file.Close()
	tp := r.FormValue("type")
	dest := ""
	if tp == "cfst" { dest = cfstFile } else if tp == "ip4" { dest = ip4File } else if tp == "ip6" { dest = ip6File } else { return }
	out, err := os.Create(dest); if err != nil { return }
	defer out.Close()
	io.Copy(out, file)
	if tp == "cfst" { os.Chmod(dest, 0755) }
	w.Write([]byte("ok"))
}
func handleStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]bool{"has_cfst": fileExists(cfstFile), "has_ip4": fileExists(ip4File), "has_ip6": fileExists(ip6File)})
}
func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFiles("index.html")
	mutex.Lock(); defer mutex.Unlock()
	if config.MaxResult == 0 { config.MaxResult = 10 }
	if config.TestPort == 0 { config.TestPort = 443 }
	tmpl.Execute(w, config)
}
func handleRunNow(w http.ResponseWriter, r *http.Request) { go runSpeedTestAndUpdateDNS(); w.Write([]byte("ok")) }
func loadConfig() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) { config = Config{CronSpec: "0 * * * *", TestCount: 10, MaxResult: 10, IPType: "v4", TestPort: 443}; return }
	f, _ := os.Open(configFile); json.NewDecoder(f).Decode(&config); f.Close()
}
func saveConfig() { f, _ := os.Create(configFile); json.NewEncoder(f).Encode(config); f.Close() }
func updateCron() { if len(cronRunner.Entries()) > 0 { cronRunner = cron.New(); cronRunner.Start() }; cronRunner.AddFunc(config.CronSpec, func() { go runSpeedTestAndUpdateDNS() }) }
func fileExists(f string) bool { _, e := os.Stat(f); return !os.IsNotExist(e) }
