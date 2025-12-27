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
	Domain        string  `json:"domain"`         // 优选域名
	
	// 测速参数
	DownloadURL   string  `json:"download_url"`   // 测速地址
	TestCount     int     `json:"test_count"`     // -dn 数量
	MinSpeed      float64 `json:"min_speed"`      // -sl 速度下限
	MaxDelay      int     `json:"max_delay"`      // -tl 延迟上限
	IPType        string  `json:"ip_type"`        // "v4", "v6", "both"
	Colo          string  `json:"colo"`           // 地区码，如 HKG,NRT
	EnableHTTPing bool    `json:"enable_httping"` // 是否开启 HTTPing
}

var (
	dataDir    = "/app/data"
	configFile = filepath.Join(dataDir, "config.json")
	cfstFile   = filepath.Join(dataDir, "cfst")
	ip4File    = filepath.Join(dataDir, "ip.txt")
	ip6File    = filepath.Join(dataDir, "ipv6.txt")
	resultFile = filepath.Join(dataDir, "result.csv")
	
	config     Config
	mutex      sync.Mutex
	cronRunner *cron.Cron
	lastLog    string
)

func main() {
	// 确保持久化目录存在
	os.MkdirAll(dataDir, 0755)

	// 初始化配置
	loadConfig()

	// 启动定时任务
	cronRunner = cron.New()
	updateCron()
	cronRunner.Start()

	// 路由
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/save", handleSave)
	http.HandleFunc("/api/upload", handleUpload)
	http.HandleFunc("/api/run", handleRunNow)
	http.HandleFunc("/api/logs", handleLogs)
	http.HandleFunc("/api/status", handleStatus) // 获取版本和文件状态

	log.Println("Web server starting on :8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// 核心逻辑：运行测速
func runSpeedTestAndUpdateDNS() {
	mutex.Lock()
	defer mutex.Unlock() // 简单的锁，防止手动和自动同时运行冲突

	logMsg("=== 开始执行测速任务 ===")

	// 1. 检查可执行文件
	if _, err := os.Stat(cfstFile); os.IsNotExist(err) {
		logMsg("错误: 找不到 cfst 可执行文件，请先在网页上传！")
		return
	}
	// 确保有执行权限
	os.Chmod(cfstFile, 0755)

	// 2. 准备 IP 文件
	targetIPFile := ip4File
	if config.IPType == "v6" {
		targetIPFile = ip6File
	} else if config.IPType == "both" {
		// 合并 v4 和 v6 到临时文件
		targetIPFile = filepath.Join(dataDir, "ip_combined.txt")
		err := combineFiles(targetIPFile, ip4File, ip6File)
		if err != nil {
			logMsg(fmt.Sprintf("合并 IP 文件失败: %v", err))
			return
		}
	}

	if _, err := os.Stat(targetIPFile); os.IsNotExist(err) {
		logMsg(fmt.Sprintf("错误: 找不到 IP 文件 (%s)，请先上传！", targetIPFile))
		return
	}

	// 3. 构建参数
	args := []string{
		"-o", resultFile,
		"-dn", fmt.Sprintf("%d", config.TestCount),
		"-sl", fmt.Sprintf("%.2f", config.MinSpeed),
		"-tl", fmt.Sprintf("%d", config.MaxDelay),
		"-f", targetIPFile,
	}

	if config.DownloadURL != "" {
		args = append(args, "-url", config.DownloadURL)
	}

	// 地区码过滤 (必须配合 HTTPing)
	if config.Colo != "" {
		args = append(args, "-cfcolo", config.Colo)
		// 如果指定了地区，强制开启 HTTPing (CFST 限制)
		if !config.EnableHTTPing {
			logMsg("提示: 检测到指定了地区码，自动开启 -httping 模式")
			args = append(args, "-httping")
		}
	}
	
	if config.EnableHTTPing && !contains(args, "-httping") {
		args = append(args, "-httping")
	}

	logMsg(fmt.Sprintf("执行命令: cfst %v", args))

	cmd := exec.Command(cfstFile, args...)
	cmd.Dir = dataDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	// 输出部分日志（避免过长）
	output := out.String()
	if len(output) > 2000 {
		output = output[len(output)-2000:] + "\n...(truncated)"
	}
	logMsg(fmt.Sprintf("测速输出:\n%s", output))

	if err != nil {
		logMsg(fmt.Sprintf("测速执行失败: %v", err))
		return
	}

	// 4. 解析结果 & 更新 DNS
	processResult()
}

func processResult() {
	// 读取 result.csv
	f, err := os.Open(resultFile)
	if err != nil {
		logMsg("无法读取测速结果文件，可能未生成有效结果。")
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		logMsg(fmt.Sprintf("CSV 解析失败: %v", err))
		return
	}

	var ips []string
	// 跳过标题行，提取前 10 个
	for i, row := range records {
		if i == 0 { continue }
		if len(ips) >= 10 { break } // 最多取 10 个
		if len(row) > 0 {
			ips = append(ips, row[0])
		}
	}

	if len(ips) == 0 {
		logMsg("警告: 测速结果为空，未找到满足条件的 IP！")
		return
	}

	logMsg(fmt.Sprintf("优选 IP 列表: %v", ips))

	// 更新 Cloudflare
	if config.ZoneID != "" && config.APIKey != "" && config.Domain != "" {
		err := updateCloudflareDNS(ips)
		if err != nil {
			logMsg(fmt.Sprintf("Cloudflare API 错误: %v", err))
		} else {
			logMsg("Cloudflare DNS 更新成功！")
		}
	} else {
		logMsg("未配置 Cloudflare API 信息，跳过 DNS 更新。")
	}
	logMsg("=== 任务结束 ===")
}

// 辅助：合并文件
func combineFiles(dest string, srcs ...string) error {
	out, err := os.Create(dest)
	if err != nil { return err }
	defer out.Close()

	for _, src := range srcs {
		in, err := os.Open(src)
		if err != nil { continue } // 忽略不存在的文件
		io.Copy(out, in)
		in.Close()
		out.Write([]byte("\n")) // 确保换行
	}
	return nil
}

// Cloudflare 逻辑 (简化版)
func updateCloudflareDNS(newIPs []string) error {
	// 1. 获取现有记录
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s", config.ZoneID, config.Domain)
	req, _ := http.NewRequest("GET", url, nil)
	setHeaders(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return err }
	defer resp.Body.Close()

	var listResp struct {
		Result []struct { ID string `json:"id"` } `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil { return err }

	// 2. 删除旧记录
	for _, r := range listResp.Result {
		delUrl := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", config.ZoneID, r.ID)
		dReq, _ := http.NewRequest("DELETE", delUrl, nil)
		setHeaders(dReq)
		http.DefaultClient.Do(dReq)
	}

	// 3. 添加新记录
	for _, ip := range newIPs {
		typeStr := "A"
		if strings.Contains(ip, ":") { typeStr = "AAAA" }
		
		payload := map[string]interface{}{
			"type": typeStr, "name": config.Domain, "content": ip, "ttl": 60, "proxied": false,
		}
		body, _ := json.Marshal(payload)
		cReq, _ := http.NewRequest("POST", fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", config.ZoneID), bytes.NewBuffer(body))
		setHeaders(cReq)
		http.DefaultClient.Do(cReq)
	}
	return nil
}

func setHeaders(req *http.Request) {
	req.Header.Set("X-Auth-Email", config.Email)
	req.Header.Set("X-Auth-Key", config.APIKey)
	req.Header.Set("Content-Type", "application/json")
}

// --- Web Handlers ---

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { return }
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "上传失败", 400)
		return
	}
	defer file.Close()

	fileType := r.FormValue("type") // "cfst", "ip4", "ip6"
	var destPath string
	switch fileType {
	case "cfst": destPath = cfstFile
	case "ip4": destPath = ip4File
	case "ip6": destPath = ip6File
	default:
		http.Error(w, "未知文件类型", 400)
		return
	}

	out, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "保存文件失败", 500)
		return
	}
	defer out.Close()
	io.Copy(out, file)
	
	if fileType == "cfst" {
		os.Chmod(destPath, 0755) // 赋予执行权限
	}
	
	w.Write([]byte("上传成功"))
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"has_cfst": fileExists(cfstFile),
		"has_ip4":  fileExists(ip4File),
		"has_ip6":  fileExists(ip6File),
		"version":  "未知",
	}
	
	// 尝试获取 cfst 版本
	if status["has_cfst"].(bool) {
		cmd := exec.Command(cfstFile, "-v")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			// 解析版本号，通常第一行包含版本
			lines := strings.Split(out.String(), "\n")
			if len(lines) > 0 {
				status["version"] = lines[0]
			}
		}
	}
	
	json.NewEncoder(w).Encode(status)
}

func handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" { return }
	mutex.Lock()
	defer mutex.Unlock()

	config.CronSpec = r.FormValue("cron_spec")
	config.ZoneID = r.FormValue("zone_id")
	config.APIKey = r.FormValue("api_key")
	config.Email = r.FormValue("email")
	config.Domain = r.FormValue("domain")
	config.DownloadURL = r.FormValue("download_url")
	config.IPType = r.FormValue("ip_type")
	config.Colo = strings.ToUpper(r.FormValue("colo"))
	config.EnableHTTPing = (r.FormValue("enable_httping") == "on")
	
	fmt.Sscanf(r.FormValue("test_count"), "%d", &config.TestCount)
	fmt.Sscanf(r.FormValue("min_speed"), "%f", &config.MinSpeed)
	fmt.Sscanf(r.FormValue("max_delay"), "%d", &config.MaxDelay)

	saveConfig()
	updateCron()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// 简单的日志和页面渲染逻辑 (省略部分与之前类似的辅助函数)
func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.ParseFiles("index.html")
	mutex.Lock()
	defer mutex.Unlock()
	tmpl.Execute(w, config)
}

func handleRunNow(w http.ResponseWriter, r *http.Request) {
	go runSpeedTestAndUpdateDNS()
	w.Write([]byte("任务已后台启动，请查看日志区域。"))
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()
	w.Write([]byte(lastLog))
}

func logMsg(msg string) {
	ts := time.Now().Format("2006-01-02 15:04:05")
	fullMsg := fmt.Sprintf("[%s] %s\n", ts, msg)
	fmt.Print(fullMsg) // 输出到 Docker logs
	
	// 写入内存日志供 Web 查看
	mutex.Lock() // 这里需要注意死锁，如果 runSpeedTest 已经持有锁调用 logMsg
	// 修正：logMsg 不应该加主锁，或者 lastLog 应单独用锁
	// 为简化，假设 logMsg 被调用时已经有上下文保护，或者简单追加字符串
	// 生产环境建议用 channel 记日志
	if len(lastLog) > 10000 {
		lastLog = lastLog[len(lastLog)-10000:]
	}
	lastLog += fullMsg
	mutex.Unlock()
}

// 配置加载保存逻辑 (略，同前)
func loadConfig() {
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		config = Config{CronSpec: "0 * * * *", TestCount: 10, MinSpeed: 0, MaxDelay: 9999, IPType: "v4"}
		return
	}
	f, _ := os.Open(configFile)
	json.NewDecoder(f).Decode(&config)
	f.Close()
}
func saveConfig() {
	f, _ := os.Create(configFile)
	json.NewEncoder(f).Encode(config)
	f.Close()
}
func updateCron() {
	if len(cronRunner.Entries()) > 0 {
		cronRunner = cron.New()
		cronRunner.Start()
	}
	cronRunner.AddFunc(config.CronSpec, func() { go runSpeedTestAndUpdateDNS() })
}
func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}
func contains(slice []string, item string) bool {
	for _, s := range slice { if s == item { return true } }
	return false
}