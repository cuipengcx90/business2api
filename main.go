package main

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
)

// ==================== 配置结构 ====================

type PoolConfig struct {
	TargetCount          int    `json:"target_count"`           // 目标账号数量
	MinCount             int    `json:"min_count"`              // 最小账号数，低于此值触发注册
	CheckIntervalMinutes int    `json:"check_interval_minutes"` // 检查间隔(分钟)
	RegisterThreads      int    `json:"register_threads"`       // 注册线程数
	RegisterHeadless     bool   `json:"register_headless"`      // 无头模式
	RegisterScript       string `json:"register_script"`        // 注册脚本路径
	RefreshOnStartup     bool   `json:"refresh_on_startup"`     // 启动时刷新账号
}

type AppConfig struct {
	APIKeys       []string   `json:"api_keys"`       // API 密钥列表
	ListenAddr    string     `json:"listen_addr"`    // 监听地址
	DataDir       string     `json:"data_dir"`       // 数据目录
	Pool          PoolConfig `json:"pool"`           // 号池配置
	Proxy         string     `json:"proxy"`          // 代理
	DefaultConfig string     `json:"default_config"` // 默认 configId
}

var appConfig = AppConfig{
	ListenAddr: ":8000",
	DataDir:    "./data",
	Pool: PoolConfig{
		TargetCount:          50,
		MinCount:             10,
		CheckIntervalMinutes: 30,
		RegisterThreads:      1,
		RegisterHeadless:     true,
		RegisterScript:       "./main.js",
		RefreshOnStartup:     true,
	},
}

// 兼容旧的环境变量
var (
	DataDir       string
	Proxy         string
	ListenAddr    string
	DefaultConfig string
	JwtTTL        = 270 * time.Second
)

// 保存默认配置到文件
func saveDefaultConfig(configPath string) error {
	data, err := json.MarshalIndent(appConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func loadAppConfig() {
	// 尝试加载配置文件
	configPath := "config.json"
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &appConfig); err != nil {
			log.Printf("⚠️ 解析配置文件失败: %v，使用默认配置", err)
		} else {
			log.Printf("✅ 加载配置文件: %s", configPath)
		}
	} else if os.IsNotExist(err) {
		// 配置文件不存在，创建默认配置
		log.Printf("⚠️ 配置文件不存在，创建默认配置: %s", configPath)
		if err := saveDefaultConfig(configPath); err != nil {
			log.Printf("❌ 创建默认配置失败: %v", err)
		}
	}

	// 环境变量覆盖配置文件
	if v := os.Getenv("DATA_DIR"); v != "" {
		appConfig.DataDir = v
	}
	if v := os.Getenv("PROXY"); v != "" {
		appConfig.Proxy = v
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		appConfig.ListenAddr = v
	}
	if v := os.Getenv("CONFIG_ID"); v != "" {
		appConfig.DefaultConfig = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		appConfig.APIKeys = append(appConfig.APIKeys, v)
	}

	// 设置全局变量
	DataDir = appConfig.DataDir
	Proxy = appConfig.Proxy
	ListenAddr = appConfig.ListenAddr
	DefaultConfig = appConfig.DefaultConfig
}

var FixedModels = []string{
	"gemini-2.5-flash",
	"gemini-2.5-pro",
	"gemini-3-pro-preview",
	"gemini-3-pro",
	"gemini-2.5-flash-image",
	"gemini-2.5-pro-image",
	"gemini-3-pro-preview-image",
	"gemini-3-pro-image",
	"gemini-2.5-flash-video",
	"gemini-2.5-pro-video",
	"gemini-3-pro-preview-video",
	"gemini-3-pro-video",
}

// 模型名称映射到 Google API 的 modelId
var modelMapping = map[string]string{
	"gemini-2.5-flash":     "gemini-2.5-flash",
	"gemini-2.5-pro":       "gemini-2.5-pro",
	"gemini-3-pro-preview": "gemini-3-pro-preview",
	"gemini-3-pro":         "gemini-3-pro",
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ==================== 数据结构 ====================

type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

type AccountData struct {
	Email         string   `json:"email"`
	FullName      string   `json:"fullName"`
	Authorization string   `json:"authorization"`
	Cookies       []Cookie `json:"cookies"`
	Timestamp     string   `json:"timestamp"`
	ConfigID      string   `json:"configId,omitempty"` // 从 URL /cid/xxx 提取
	CSESIDX       string   `json:"csesidx,omitempty"`  // 从 URL ?csesidx=xxx 提取
}

type Account struct {
	Data        AccountData
	FilePath    string
	JWT         string
	JWTExpires  time.Time
	ConfigID    string
	CSESIDX     string
	LastRefresh time.Time // 上次刷新时间，用于冷却
	Refreshed   bool      // 是否已刷新成功

	mu sync.Mutex
}

const refreshCooldown = 5 * time.Minute // 刷新冷却时间

// ==================== 号池管理 ====================

type AccountPool struct {
	readyAccounts   []*Account // 已刷新可用的账号
	pendingAccounts []*Account // 待刷新的账号
	index           uint64
	mu              sync.RWMutex
	refreshInterval time.Duration // 刷新间隔
	refreshWorkers  int           // 刷新并发数
	stopChan        chan struct{}
}

var pool = &AccountPool{
	refreshInterval: 5 * time.Minute, // 5分钟刷新一次全部账号
	refreshWorkers:  5,               // 提高并发数
	stopChan:        make(chan struct{}),
}

func (p *AccountPool) Load(dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	existingAccounts := make(map[string]*Account)
	for _, acc := range p.readyAccounts {
		existingAccounts[acc.FilePath] = acc
	}
	for _, acc := range p.pendingAccounts {
		existingAccounts[acc.FilePath] = acc
	}
	var newReadyAccounts []*Account
	var newPendingAccounts []*Account

	for _, f := range files {
		// 如果账号已存在，保留在原来的池中
		if acc, ok := existingAccounts[f]; ok {
			if acc.Refreshed {
				newReadyAccounts = append(newReadyAccounts, acc)
			} else {
				newPendingAccounts = append(newPendingAccounts, acc)
			}
			delete(existingAccounts, f)
			continue
		}

		// 新账号，加入 pending 池
		data, err := os.ReadFile(f)
		if err != nil {
			log.Printf("⚠️ 读取 %s 失败: %v", f, err)
			continue
		}

		var acc AccountData
		if err := json.Unmarshal(data, &acc); err != nil {
			log.Printf("⚠️ 解析 %s 失败: %v", f, err)
			continue
		}

		csesidx := acc.CSESIDX
		if csesidx == "" {
			csesidx = extractCSESIDX(acc.Authorization)
		}
		if csesidx == "" {
			log.Printf("⚠️ %s 无法获取 csesidx", f)
			continue
		}

		configID := acc.ConfigID
		if configID == "" && DefaultConfig != "" {
			configID = DefaultConfig
		}

		newPendingAccounts = append(newPendingAccounts, &Account{
			Data:      acc,
			FilePath:  f,
			CSESIDX:   csesidx,
			ConfigID:  configID,
			Refreshed: false,
		})
	}

	p.readyAccounts = newReadyAccounts
	p.pendingAccounts = newPendingAccounts
	return nil
}

// GetPendingAccount 获取一个待刷新的账号
func (p *AccountPool) GetPendingAccount() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.pendingAccounts) == 0 {
		return nil
	}

	acc := p.pendingAccounts[0]
	p.pendingAccounts = p.pendingAccounts[1:]
	return acc
}
func (p *AccountPool) MarkReady(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	acc.Refreshed = true
	p.readyAccounts = append(p.readyAccounts, acc)
}
func (p *AccountPool) RemoveAccount(acc *Account) {
	if err := os.Remove(acc.FilePath); err != nil {
		log.Printf("⚠️ 删除文件失败 %s: %v", acc.FilePath, err)
	} else {
		log.Printf("🗑️ 已删除失效账号: %s", filepath.Base(acc.FilePath))
	}
}

func (acc *Account) SaveToFile() error {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	// 更新时间戳
	acc.Data.Timestamp = time.Now().Format(time.RFC3339)

	data, err := json.MarshalIndent(acc.Data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号数据失败: %w", err)
	}

	if err := os.WriteFile(acc.FilePath, data, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	return nil
}
func (p *AccountPool) StartPoolManager() {
	// 启动多个刷新 worker
	for i := 0; i < p.refreshWorkers; i++ {
		go p.refreshWorker(i)
	}

	// 周期性重新扫描文件
	go p.scanWorker()
}

// refreshWorker 刷新工作协程
func (p *AccountPool) refreshWorker(id int) {
	for {
		select {
		case <-p.stopChan:
			return
		default:
		}

		acc := p.GetPendingAccount()
		if acc == nil {
			// 没有待刷新账号，等待一段时间
			time.Sleep(time.Second)
			continue
		}

		// 检查冷却，避免频繁刷新
		if time.Since(acc.LastRefresh) < refreshCooldown {
			// 冷却中，直接放回 ready 队列
			acc.Refreshed = true
			p.MarkReady(acc)
			continue
		}

		acc.JWTExpires = time.Time{}
		if err := acc.RefreshJWT(); err != nil {
			// 只有账号失效（401/403）才删除，其他错误放回队列重试
			if strings.Contains(err.Error(), "账号失效") {
				log.Printf("❌ [worker-%d] [%s] %v", id, acc.Data.Email, err)
				p.RemoveAccount(acc)
			} else if strings.Contains(err.Error(), "刷新冷却中") {
				// 冷却中，直接放回 ready 队列
				acc.Refreshed = true
				p.MarkReady(acc)
			} else {
				log.Printf("⚠️ [worker-%d] [%s] 刷新失败: %v，稍后重试", id, acc.Data.Email, err)
				p.MarkPending(acc)
			}
		} else {
			// 写回文件
			if err := acc.SaveToFile(); err != nil {
				log.Printf("⚠️ [%s] 写回文件失败: %v", acc.Data.Email, err)
			}
			p.MarkReady(acc)
		}
	}
}

// scanWorker 周期性扫描新账号文件并刷新所有账号
func (p *AccountPool) scanWorker() {
	ticker := time.NewTicker(p.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			// 扫描新账号文件
			p.Load(DataDir)
			// 将所有 ready 账号移回 pending 重新刷新
			p.RefreshAllAccounts()

		}
	}
}
func (p *AccountPool) RefreshAllAccounts() {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stillReady []*Account
	refreshed := 0
	skipped := 0

	for _, acc := range p.readyAccounts {
		// 检查是否在冷却中
		if time.Since(acc.LastRefresh) < refreshCooldown {
			// 冷却中，保留在 ready 队列
			stillReady = append(stillReady, acc)
			skipped++
			continue
		}

		// 未冷却，移入 pending 队列刷新
		acc.Refreshed = false
		acc.JWTExpires = time.Time{}
		p.pendingAccounts = append(p.pendingAccounts, acc)
		refreshed++
	}

	p.readyAccounts = stillReady

	if refreshed > 0 || skipped > 0 {
		log.Printf("🔄 周期刷新: %d 个账号加入刷新队列, %d 个账号冷却中跳过", refreshed, skipped)
	}
}
func (p *AccountPool) PendingCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pendingAccounts)
}

// ReadyCount 返回可用账号数
func (p *AccountPool) ReadyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.readyAccounts)
}

func (p *AccountPool) MarkPending(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 从 ready 池移除
	for i, a := range p.readyAccounts {
		if a == acc {
			p.readyAccounts = append(p.readyAccounts[:i], p.readyAccounts[i+1:]...)
			break
		}
	}

	acc.Refreshed = false
	p.pendingAccounts = append(p.pendingAccounts, acc)
}
func (acc *Account) InvalidateJWT() {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.JWT = ""
	acc.JWTExpires = time.Time{}
	acc.LastRefresh = time.Time{} // 清除冷却时间，允许立即刷新
}

func extractCSESIDX(auth string) string {
	// Bearer eyJ...
	parts := strings.Split(auth, " ")
	if len(parts) != 2 {
		return ""
	}
	token := parts[1]
	jwtParts := strings.Split(token, ".")
	if len(jwtParts) != 3 {
		return ""
	}

	payload, err := base64.RawURLEncoding.DecodeString(jwtParts[1])
	if err != nil {
		return ""
	}

	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}

	// sub: "csesidx/394868671"
	if strings.HasPrefix(claims.Sub, "csesidx/") {
		return strings.TrimPrefix(claims.Sub, "csesidx/")
	}
	return ""
}

func (p *AccountPool) Next() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.readyAccounts) == 0 {
		return nil
	}

	// 尝试找一个不在冷却中的账号
	n := len(p.readyAccounts)
	startIdx := atomic.AddUint64(&p.index, 1) - 1
	for i := 0; i < n; i++ {
		acc := p.readyAccounts[(startIdx+uint64(i))%uint64(n)]
		acc.mu.Lock()
		inCooldown := time.Since(acc.LastRefresh) < refreshCooldown
		acc.mu.Unlock()
		if !inCooldown {
			return acc
		}
	}
	// 所有账号都在冷却中，返回第一个（等待冷却结束）
	return p.readyAccounts[startIdx%uint64(n)]
}

func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.readyAccounts)
}

func (p *AccountPool) TotalCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.readyAccounts) + len(p.pendingAccounts)
}

// ==================== JWT 生成 ====================

func urlsafeB64Encode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func kqEncode(s string) string {
	var b []byte
	for _, ch := range s {
		v := int(ch)
		if v > 255 {
			b = append(b, byte(v&255), byte(v>>8))
		} else {
			b = append(b, byte(v))
		}
	}
	return urlsafeB64Encode(b)
}

func createJWT(keyBytes []byte, keyID, csesidx string) string {
	now := time.Now().Unix()
	header := map[string]interface{}{
		"alg": "HS256",
		"typ": "JWT",
		"kid": keyID,
	}
	payload := map[string]interface{}{
		"iss": "https://business.gemini.google",
		"aud": "https://biz-discoveryengine.googleapis.com",
		"sub": fmt.Sprintf("csesidx/%s", csesidx),
		"iat": now,
		"exp": now + 300,
		"nbf": now,
	}

	headerJSON, _ := json.Marshal(header)
	payloadJSON, _ := json.Marshal(payload)

	headerB64 := kqEncode(string(headerJSON))
	payloadB64 := kqEncode(string(payloadJSON))
	message := headerB64 + "." + payloadB64

	h := hmac.New(sha256.New, keyBytes)
	h.Write([]byte(message))
	sig := h.Sum(nil)

	return message + "." + urlsafeB64Encode(sig)
}
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if Proxy != "" {
		proxyURL, err := url.Parse(Proxy)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   1800 * time.Second,
	}
}

var httpClient *http.Client

func initHTTPClient() {
	httpClient = newHTTPClient()
	if Proxy != "" {
		log.Printf("✅ 使用代理: %s", Proxy)
	}
}

// 读取响应体，自动处理 gzip
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzReader.Close()
		reader = gzReader
	}
	return io.ReadAll(reader)
}
func parseNDJSON(data []byte) []map[string]interface{} {
	var result []map[string]interface{}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(line, &obj); err == nil {
			result = append(result, obj)
		}
	}
	return result
}
func parseIncompleteJSONArray(data []byte) []map[string]interface{} {
	var result []map[string]interface{}
	if err := json.Unmarshal(data, &result); err == nil {
		return result
	}

	// 检查是否以 [ 开头但没有正确闭合
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// 尝试添加 ] 闭合
		if trimmed[len(trimmed)-1] != ']' {
			// 找到最后一个完整的 } 并在其后添加 ]
			lastBrace := bytes.LastIndex(trimmed, []byte("}"))
			if lastBrace > 0 {
				fixed := append(trimmed[:lastBrace+1], ']')
				if err := json.Unmarshal(fixed, &result); err == nil {
					log.Printf("⚠️ JSON 数组不完整，已修复并解析成功")
					return result
				}
			}
		}
	}

	return nil
}

// ==================== 账号操作 ====================

func (acc *Account) getCookie(name string) string {
	for _, c := range acc.Data.Cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (acc *Account) RefreshJWT() error {
	acc.mu.Lock()
	defer acc.mu.Unlock()

	// JWT 未过期，直接返回
	if time.Now().Before(acc.JWTExpires) {
		return nil
	}

	// 冷却期内，跳过刷新
	if time.Since(acc.LastRefresh) < refreshCooldown {
		return fmt.Errorf("刷新冷却中，剩余 %.0f 秒", (refreshCooldown - time.Since(acc.LastRefresh)).Seconds())
	}

	secureSES := acc.getCookie("__Secure-C_SES")
	hostOSES := acc.getCookie("__Host-C_OSES")

	cookie := fmt.Sprintf("__Secure-C_SES=%s", secureSES)
	if hostOSES != "" {
		cookie += fmt.Sprintf("; __Host-C_OSES=%s", hostOSES)
	}

	req, _ := http.NewRequest("GET", "https://business.gemini.google/auth/getoxsrf", nil)
	q := req.URL.Query()
	q.Add("csesidx", acc.CSESIDX)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://business.gemini.google/")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("getoxsrf 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := readResponseBody(resp)
		// 401/403 表示账号失效，需要删除
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("账号失效: %d %s", resp.StatusCode, string(body))
		}
		// 其他状态码可能是临时问题
		return fmt.Errorf("getoxsrf 失败: %d %s", resp.StatusCode, string(body))
	}

	body, _ := readResponseBody(resp)
	txt := strings.TrimPrefix(string(body), ")]}'")
	txt = strings.TrimSpace(txt)

	var data struct {
		XsrfToken string `json:"xsrfToken"`
		KeyID     string `json:"keyId"`
	}
	if err := json.Unmarshal([]byte(txt), &data); err != nil {
		return fmt.Errorf("解析 xsrf 响应失败: %w", err)
	}

	// 使用 RawURLEncoding 并补齐 padding
	token := data.XsrfToken
	switch len(token) % 4 {
	case 2:
		token += "=="
	case 3:
		token += "="
	}
	keyBytes, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("解码 xsrfToken 失败: %w", err)
	}

	acc.JWT = createJWT(keyBytes, data.KeyID, acc.CSESIDX)
	acc.JWTExpires = time.Now().Add(JwtTTL)
	acc.LastRefresh = time.Now() // 更新刷新时间

	// 获取 configId
	if acc.ConfigID == "" {
		configID, err := acc.fetchConfigID()
		if err != nil {
			return fmt.Errorf("获取 configId 失败: %w", err)
		}
		acc.ConfigID = configID
	}
	return nil
}

func (acc *Account) GetJWT() (string, string, error) {
	if err := acc.RefreshJWT(); err != nil {
		return "", "", err
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.JWT, acc.ConfigID, nil
}

// 获取 configId - 优先从账号文件，其次从环境变量
func (acc *Account) fetchConfigID() (string, error) {
	// 1. 优先使用账号文件中的 configId
	if acc.Data.ConfigID != "" {
		return acc.Data.ConfigID, nil
	}

	// 2. 使用环境变量中的默认 configId
	if DefaultConfig != "" {
		return DefaultConfig, nil
	}

	return "", fmt.Errorf("未配置 configId，请设置 CONFIG_ID 环境变量或在账号文件中添加 configId 字段")
}

// ==================== Session 管理 ====================

func getCommonHeaders(jwt, origAuth string) map[string]string {
	headers := map[string]string{
		"accept":             "*/*",
		"accept-encoding":    "gzip, deflate, br, zstd",
		"accept-language":    "zh-CN,zh;q=0.9,en;q=0.8",
		"authorization":      "Bearer " + jwt,
		"content-type":       "application/json",
		"origin":             "https://business.gemini.google",
		"referer":            "https://business.gemini.google/",
		"user-agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		"x-server-timeout":   "1800",
		"sec-ch-ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "cross-site",
	}
	// 同时携带原始 authorization
	if origAuth != "" {
		headers["x-original-authorization"] = origAuth
	}
	return headers
}

func createSession(jwt, configID, origAuth string) (string, error) {
	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"createSessionRequest": map[string]interface{}{
			"session": map[string]string{"name": "", "displayName": ""},
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetCreateSession", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("createSession 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("createSession 失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Session struct {
			Name string `json:"name"`
		} `json:"session"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析 session 响应失败: %w", err)
	}

	return result.Session.Name, nil
}

// 上传图片到 Session，返回 fileId（支持 base64 或 URL）
func uploadContextFile(jwt, configID, sessionName, mimeType, base64Content, origAuth string) (string, error) {
	ext := "jpg"
	if parts := strings.Split(mimeType, "/"); len(parts) == 2 {
		ext = parts[1]
	}
	fileName := fmt.Sprintf("upload_%d_%s.%s", time.Now().Unix(), uuid.New().String()[:6], ext)

	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"addContextFileRequest": map[string]interface{}{
			"name":         sessionName,
			"fileName":     fileName,
			"mimeType":     mimeType,
			"fileContents": base64Content,
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetAddContextFile", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传文件请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("上传文件失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AddContextFileResponse struct {
			FileID string `json:"fileId"`
		} `json:"addContextFileResponse"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析上传响应失败: %w", err)
	}

	if result.AddContextFileResponse.FileID == "" {
		return "", fmt.Errorf("上传成功但 fileId 为空，响应: %s", string(respBody))
	}

	return result.AddContextFileResponse.FileID, nil
}

// 通过 URL 上传图片到 Session，返回 fileId
func uploadContextFileByURL(jwt, configID, sessionName, imageURL, origAuth string) (string, error) {
	body := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"addContextFileRequest": map[string]interface{}{
			"name":    sessionName,
			"fileUri": imageURL,
		},
	}

	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetAddContextFile", bytes.NewReader(bodyBytes))

	for k, v := range getCommonHeaders(jwt, origAuth) {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传文件请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := readResponseBody(resp)
	if err != nil {
		return "", fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("URL上传文件失败: %d %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AddContextFileResponse struct {
			FileID string `json:"fileId"`
		} `json:"addContextFileResponse"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("解析上传响应失败: %w", err)
	}

	if result.AddContextFileResponse.FileID == "" {
		return "", fmt.Errorf("URL上传成功但 fileId 为空，响应: %s", string(respBody))
	}

	return result.AddContextFileResponse.FileID, nil
}

// ==================== OpenAI 兼容接口 ====================

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string 或 []ContentPart
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float64   `json:"temperature"`
	TopP        float64   `json:"top_p"`
}

type ChatChoice struct {
	Index        int                    `json:"index"`
	Delta        map[string]interface{} `json:"delta,omitempty"`
	Message      map[string]interface{} `json:"message,omitempty"`
	FinishReason *string                `json:"finish_reason"`
}

type ChatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
}

func createChunk(id string, created int64, model string, delta map[string]interface{}, finishReason *string) string {
	chunk := ChatChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
	data, _ := json.Marshal(chunk)
	return string(data)
}

// 从响应中提取内容（文本、图片或思考）
func extractContentFromReply(replyMap map[string]interface{}, jwt, session, configID, origAuth string) (text string, imageData string, imageMime string, reasoning string) {
	groundedContent, ok := replyMap["groundedContent"].(map[string]interface{})
	if !ok {
		return
	}
	content, ok := groundedContent["content"].(map[string]interface{})
	if !ok {
		return
	}

	// 检查是否是思考内容
	if thought, ok := content["thought"].(bool); ok && thought {
		if t, ok := content["text"].(string); ok && t != "" {
			reasoning = t
		}
		return
	}

	// 提取文本
	if t, ok := content["text"].(string); ok && t != "" {
		text = t
	}

	// 提取图片 (inlineData - 直接返回 base64)
	if inlineData, ok := content["inlineData"].(map[string]interface{}); ok {
		if mime, ok := inlineData["mimeType"].(string); ok {
			imageMime = mime
		}
		if data, ok := inlineData["data"].(string); ok {
			imageData = data
		}
	}

	// 提取文件 (file - 需要下载，可能是图片或视频)
	if file, ok := content["file"].(map[string]interface{}); ok {
		fileId, _ := file["fileId"].(string)
		mimeType, _ := file["mimeType"].(string)
		if fileId != "" {
			// 根据 mimeType 判断类型
			fileType := "文件"
			if strings.HasPrefix(mimeType, "image/") {
				fileType = "图片"
			} else if strings.HasPrefix(mimeType, "video/") {
				fileType = "视频"
			}
			log.Printf("📥 发现%s: fileId=%s, mimeType=%s", fileType, fileId, mimeType)
			data, err := downloadGeneratedFile(jwt, fileId, session, configID, origAuth)
			if err != nil {
				log.Printf("❌ 下载%s失败: %v", fileType, err)
			} else {
				imageData = data
				imageMime = mimeType
				log.Printf("✅ %s下载成功, 大小: %d bytes", fileType, len(data))
			}
		}
	}

	return
}

// 下载生成的文件（图片或视频）
func downloadGeneratedFile(jwt, fileId, session, configID, origAuth string) (string, error) {
	// 参数验证
	if jwt == "" {
		return "", fmt.Errorf("JWT 为空，无法下载文件")
	}
	if session == "" {
		return "", fmt.Errorf("session 为空，无法下载文件")
	}
	if configID == "" {
		return "", fmt.Errorf("configID 为空，无法下载文件")
	}

	log.Printf("📥 开始下载文件: fileId=%s, session=%s", fileId, session)

	// 步骤1: 使用 widgetListSessionFileMetadata 获取文件下载 URL
	listBody := map[string]interface{}{
		"configId":         configID,
		"additionalParams": map[string]string{"token": "-"},
		"listSessionFileMetadataRequest": map[string]interface{}{
			"name":   session,
			"filter": "file_origin_type = AI_GENERATED",
		},
	}
	listBodyBytes, _ := json.Marshal(listBody)

	listReq, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetListSessionFileMetadata", bytes.NewReader(listBodyBytes))
	for k, v := range getCommonHeaders(jwt, origAuth) {
		listReq.Header.Set(k, v)
	}

	listResp, err := httpClient.Do(listReq)
	if err != nil {
		return "", fmt.Errorf("获取文件元数据失败: %w", err)
	}
	defer listResp.Body.Close()

	listRespBody, _ := readResponseBody(listResp)

	if listResp.StatusCode != 200 {
		return "", fmt.Errorf("获取文件元数据失败: HTTP %d: %s", listResp.StatusCode, string(listRespBody))
	}

	// 解析响应，查找匹配的 fileId
	var listResult struct {
		ListSessionFileMetadataResponse struct {
			FileMetadata []struct {
				FileID      string `json:"fileId"`
				Session     string `json:"session"` // 包含完整的 projects 路径
				DownloadURI string `json:"downloadUri"`
			} `json:"fileMetadata"`
		} `json:"listSessionFileMetadataResponse"`
	}
	if err := json.Unmarshal(listRespBody, &listResult); err != nil {
		return "", fmt.Errorf("解析文件元数据失败: %w", err)
	}

	// 查找匹配的文件，获取完整 session 路径
	var fullSession string
	for _, meta := range listResult.ListSessionFileMetadataResponse.FileMetadata {
		if meta.FileID == fileId {
			fullSession = meta.Session // 如: projects/372889301682/locations/global/collections/...
			break
		}
	}

	if fullSession == "" {
		return "", fmt.Errorf("未找到 fileId=%s 的文件信息", fileId)
	}

	// 构建下载 URL：使用 biz-discoveryengine 端点
	// 格式: https://biz-discoveryengine.googleapis.com/download/v1alpha/{fullSession}:downloadFile?fileId={fileId}&alt=media
	downloadURL := fmt.Sprintf("https://biz-discoveryengine.googleapis.com/download/v1alpha/%s:downloadFile?fileId=%s&alt=media", fullSession, fileId)

	log.Printf("📥 下载图片 URL: %s", downloadURL)

	// 步骤2: 下载图片（使用 biz-discoveryengine 端点和 JWT）
	downloadReq, _ := http.NewRequest("GET", downloadURL, nil)
	for k, v := range getCommonHeaders(jwt, origAuth) {
		downloadReq.Header.Set(k, v)
	}

	downloadResp, err := httpClient.Do(downloadReq)
	if err != nil {
		return "", fmt.Errorf("下载图片失败: %w", err)
	}
	defer downloadResp.Body.Close()

	imgBody, _ := readResponseBody(downloadResp)

	if downloadResp.StatusCode != 200 {
		return "", fmt.Errorf("下载图片失败: HTTP %d: %s", downloadResp.StatusCode, string(imgBody))
	}

	// 响应是原始二进制图片数据，需要转为 base64
	return base64.StdEncoding.EncodeToString(imgBody), nil
}

// 将图片转换为 Markdown 格式的 data URI
func formatImageAsMarkdown(mimeType, base64Data string) string {
	return fmt.Sprintf("![image](data:%s;base64,%s)", mimeType, base64Data)
}

// 媒体信息（图片/视频）
type MediaInfo struct {
	MimeType  string
	Data      string // base64 数据
	URL       string // 原始 URL（如果有）
	IsURL     bool   // 是否使用 URL 直接上传
	MediaType string // "image" 或 "video"
}

// 别名，保持向后兼容
type ImageInfo = MediaInfo

// 解析消息内容，支持文本、图片和视频
func parseMessageContent(msg Message) (string, []MediaInfo) {
	var textContent string
	var medias []MediaInfo

	switch content := msg.Content.(type) {
	case string:
		textContent = content
	case []interface{}:
		for _, part := range content {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				continue
			}

			partType, _ := partMap["type"].(string)
			switch partType {
			case "text":
				if text, ok := partMap["text"].(string); ok {
					textContent += text
				}
			case "image_url":
				if imgURL, ok := partMap["image_url"].(map[string]interface{}); ok {
					if urlStr, ok := imgURL["url"].(string); ok {
						media := parseMediaURL(urlStr, "image")
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			case "video_url":
				// 支持视频 URL
				if videoURL, ok := partMap["video_url"].(map[string]interface{}); ok {
					if urlStr, ok := videoURL["url"].(string); ok {
						media := parseMediaURL(urlStr, "video")
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			case "file":
				// 支持通用文件类型
				if fileData, ok := partMap["file"].(map[string]interface{}); ok {
					if urlStr, ok := fileData["url"].(string); ok {
						mediaType := "image" // 默认图片
						if mime, ok := fileData["mime_type"].(string); ok {
							if strings.HasPrefix(mime, "video/") {
								mediaType = "video"
							}
						}
						media := parseMediaURL(urlStr, mediaType)
						if media != nil {
							medias = append(medias, *media)
						}
					}
				}
			}
		}
	}

	return textContent, medias
}

// 解析媒体 URL（图片或视频）
func parseMediaURL(urlStr, defaultType string) *MediaInfo {
	// 处理 base64 数据
	if strings.HasPrefix(urlStr, "data:") {
		// data:image/jpeg;base64,/9j/4AAQ... 或 data:video/mp4;base64,...
		parts := strings.SplitN(urlStr, ",", 2)
		if len(parts) != 2 {
			return nil
		}

		base64Data := parts[1]
		mediaType := defaultType
		var mimeType string

		// 检测媒体类型
		if strings.Contains(parts[0], "video/") {
			mediaType = "video"
			// 视频格式处理
			if strings.Contains(parts[0], "video/mp4") {
				mimeType = "video/mp4"
			} else if strings.Contains(parts[0], "video/webm") {
				mimeType = "video/webm"
			} else if strings.Contains(parts[0], "video/quicktime") || strings.Contains(parts[0], "video/mov") {
				// MOV 格式，尝试作为 mp4 上传
				mimeType = "video/mp4"
				log.Printf("ℹ️ MOV 视频将作为 MP4 上传")
			} else if strings.Contains(parts[0], "video/avi") || strings.Contains(parts[0], "video/x-msvideo") {
				mimeType = "video/mp4"
				log.Printf("ℹ️ AVI 视频将作为 MP4 上传")
			} else {
				// 其他视频格式默认作为 mp4
				mimeType = "video/mp4"
				log.Printf("ℹ️ 未知视频格式 %s 将作为 MP4 上传", parts[0])
			}
		} else {
			mediaType = "image"
			// 图片格式处理
			if strings.Contains(parts[0], "image/png") {
				mimeType = "image/png"
			} else if strings.Contains(parts[0], "image/jpeg") {
				mimeType = "image/jpeg"
			} else {
				// 其他图片格式需要转换为 PNG
				converted, err := convertBase64ToPNG(base64Data)
				if err != nil {
					log.Printf("⚠️ %s base64 转换失败: %v", parts[0], err)
					mimeType = "image/jpeg" // 回退
				} else {
					log.Printf("✅ %s base64 已转换为 PNG", parts[0])
					base64Data = converted
					mimeType = "image/png"
				}
			}
		}

		return &MediaInfo{
			MimeType:  mimeType,
			Data:      base64Data,
			IsURL:     false,
			MediaType: mediaType,
		}
	}

	// URL 媒体 - 优先尝试直接使用 URL 上传
	mediaType := defaultType
	// 根据 URL 后缀推断媒体类型
	lowerURL := strings.ToLower(urlStr)
	if strings.HasSuffix(lowerURL, ".mp4") || strings.HasSuffix(lowerURL, ".webm") ||
		strings.HasSuffix(lowerURL, ".mov") || strings.HasSuffix(lowerURL, ".avi") ||
		strings.HasSuffix(lowerURL, ".mkv") || strings.HasSuffix(lowerURL, ".m4v") {
		mediaType = "video"
	}

	return &MediaInfo{
		URL:       urlStr,
		IsURL:     true,
		MediaType: mediaType,
	}
}

func downloadImage(urlStr string) (string, string, error) {
	return downloadMedia(urlStr, "image")
}

// downloadMedia 下载媒体文件（图片或视频）
func downloadMedia(urlStr, mediaType string) (string, string, error) {
	resp, err := httpClient.Get(urlStr)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}

	mimeType := resp.Header.Get("Content-Type")

	if mediaType == "video" || strings.HasPrefix(mimeType, "video/") {
		// 视频处理
		if mimeType == "" {
			mimeType = "video/mp4"
		}
		// 规范化视频 MIME 类型
		mimeType = normalizeVideoMimeType(mimeType)
		return base64.StdEncoding.EncodeToString(data), mimeType, nil
	}

	// 图片处理
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	// 只有 jpeg 和 png 是支持的格式，其他都需要转换
	needConvert := !strings.Contains(mimeType, "jpeg") && !strings.Contains(mimeType, "png")
	if needConvert {
		converted, err := convertToPNG(data)
		if err != nil {
			log.Printf("⚠️ %s 转换失败: %v，尝试原格式", mimeType, err)
		} else {
			log.Printf("✅ %s 已转换为 PNG", mimeType)
			return base64.StdEncoding.EncodeToString(converted), "image/png", nil
		}
	}

	return base64.StdEncoding.EncodeToString(data), mimeType, nil
}

// normalizeVideoMimeType 规范化视频 MIME 类型
func normalizeVideoMimeType(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "mp4"):
		return "video/mp4"
	case strings.Contains(mimeType, "webm"):
		return "video/webm"
	case strings.Contains(mimeType, "quicktime"), strings.Contains(mimeType, "mov"):
		log.Printf("ℹ️ MOV 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "avi"), strings.Contains(mimeType, "x-msvideo"):
		log.Printf("ℹ️ AVI 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "x-matroska"), strings.Contains(mimeType, "mkv"):
		log.Printf("ℹ️ MKV 视频将作为 MP4 上传")
		return "video/mp4"
	case strings.Contains(mimeType, "3gpp"):
		return "video/3gpp"
	default:
		log.Printf("ℹ️ 未知视频格式 %s 将作为 MP4 上传", mimeType)
		return "video/mp4"
	}
}

// convertToPNG 将图片转换为 PNG 格式
func convertToPNG(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解码图片失败: %w", err)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("编码 PNG 失败: %w", err)
	}

	return buf.Bytes(), nil
}

// convertBase64ToPNG 将 base64 图片转换为 PNG
func convertBase64ToPNG(base64Data string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", fmt.Errorf("解码 base64 失败: %w", err)
	}

	converted, err := convertToPNG(data)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(converted), nil
}

const maxRetries = 3

func streamChat(c *gin.Context, req ChatRequest) {
	chatID := "chatcmpl-" + uuid.New().String()
	createdTime := time.Now().Unix()

	// 解析最后一条消息
	lastMsg := req.Messages[len(req.Messages)-1]
	textContent, images := parseMessageContent(lastMsg)

	var respBody []byte
	var lastErr error
	var usedAcc *Account
	var usedJWT, usedOrigAuth, usedConfigID, usedSession string

	for retry := 0; retry < maxRetries; retry++ {
		acc := pool.Next()
		if acc == nil {
			c.JSON(500, gin.H{"error": "没有可用账号"})
			return
		}
		usedAcc = acc

		if retry > 0 {
			log.Printf("🔄 第 %d 次重试，切换账号: %s", retry+1, acc.Data.Email)
		}

		jwt, configID, err := acc.GetJWT()
		if err != nil {
			log.Printf("❌ [%s] 获取 JWT 失败: %v", acc.Data.Email, err)
			lastErr = err
			continue
		}

		session, err := createSession(jwt, configID, acc.Data.Authorization)
		if err != nil {
			log.Printf("❌ [%s] 创建 Session 失败: %v", acc.Data.Email, err)
			lastErr = err
			continue
		}

		// 上传媒体文件并获取 fileIds
		var fileIds []string
		uploadFailed := false
		for _, media := range images {
			var fileId string
			var err error

			mediaTypeName := "图片"
			if media.MediaType == "video" {
				mediaTypeName = "视频"
			}

			if media.IsURL {
				// 优先尝试 URL 直接上传
				fileId, err = uploadContextFileByURL(jwt, configID, session, media.URL, acc.Data.Authorization)
				if err != nil {
					// URL 上传失败，回退到下载后上传
					mediaData, mimeType, dlErr := downloadMedia(media.URL, media.MediaType)
					if dlErr != nil {
						log.Printf("⚠️ [%s] %s下载失败: %v", acc.Data.Email, mediaTypeName, dlErr)
						uploadFailed = true
						break
					}
					fileId, err = uploadContextFile(jwt, configID, session, mimeType, mediaData, acc.Data.Authorization)
				}
			} else {
				// base64 数据直接上传
				fileId, err = uploadContextFile(jwt, configID, session, media.MimeType, media.Data, acc.Data.Authorization)
			}

			if err != nil {
				log.Printf("⚠️ [%s] %s上传失败: %v", acc.Data.Email, mediaTypeName, err)
				uploadFailed = true
				break
			}
			fileIds = append(fileIds, fileId)
		}
		if uploadFailed {
			lastErr = fmt.Errorf("媒体上传失败")
			continue
		}

		// 构建 query parts（只包含文本）
		queryParts := []map[string]interface{}{}
		if textContent != "" {
			queryParts = append(queryParts, map[string]interface{}{"text": textContent})
		}

		// 检查模型类型后缀
		isImageModel := strings.HasSuffix(req.Model, "-image")
		isVideoModel := strings.HasSuffix(req.Model, "-video")
		actualModel := strings.TrimSuffix(strings.TrimSuffix(req.Model, "-image"), "-video")

		// 构建 toolsSpec
		var toolsSpec map[string]interface{}
		if isImageModel {
			// -image 模型只启用图片生成
			toolsSpec = map[string]interface{}{
				"imageGenerationSpec": map[string]interface{}{},
			}
		} else if isVideoModel {
			// -video 模型只启用视频生成
			toolsSpec = map[string]interface{}{
				"videoGenerationSpec": map[string]interface{}{},
			}
		} else {
			// 普通模型启用所有工具
			toolsSpec = map[string]interface{}{
				"webGroundingSpec":    map[string]interface{}{},
				"toolRegistry":        "default_tool_registry",
				"imageGenerationSpec": map[string]interface{}{},
				"videoGenerationSpec": map[string]interface{}{},
			}
		}

		body := map[string]interface{}{
			"configId":         configID,
			"additionalParams": map[string]string{"token": "-"},
			"streamAssistRequest": map[string]interface{}{
				"session":              session,
				"query":                map[string]interface{}{"parts": queryParts},
				"filter":               "",
				"fileIds":              fileIds,
				"answerGenerationMode": "NORMAL",
				"toolsSpec":            toolsSpec,
				"languageCode":         "zh-CN",
				"userMetadata":         map[string]string{"timeZone": "Asia/Shanghai"},
				"assistSkippingMode":   "REQUEST_ASSIST",
			},
		}

		// 设置模型 ID（去掉 -image 后缀）
		if targetModelID, ok := modelMapping[actualModel]; ok && targetModelID != "" {
			body["streamAssistRequest"].(map[string]interface{})["assistGenerationConfig"] = map[string]interface{}{
				"modelId": targetModelID,
			}
		}

		bodyBytes, _ := json.Marshal(body)
		httpReq, _ := http.NewRequest("POST", "https://biz-discoveryengine.googleapis.com/v1alpha/locations/global/widgetStreamAssist", bytes.NewReader(bodyBytes))

		for k, v := range getCommonHeaders(jwt, acc.Data.Authorization) {
			httpReq.Header.Set(k, v)
		}

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			log.Printf("❌ [%s] 请求失败: %v", acc.Data.Email, err)
			lastErr = err
			continue
		}

		if resp.StatusCode != 200 {
			body, _ := readResponseBody(resp)
			resp.Body.Close()
			log.Printf("❌ [%s] Google 报错: %d %s (重试 %d/%d)", acc.Data.Email, resp.StatusCode, string(body), retry+1, maxRetries)
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
			// 429 限流，标记账号进入冷却，下次 Next() 会自动切换到其他账号
			if resp.StatusCode == 429 {
				acc.mu.Lock()
				acc.LastRefresh = time.Now() // 触发冷却
				acc.mu.Unlock()
				log.Printf("⏳ [%s] 429 限流，账号进入冷却", acc.Data.Email)
			}
			continue
		}

		// 成功，读取响应
		respBody, _ = readResponseBody(resp)
		resp.Body.Close()

		// 快速检查是否是认证错误响应
		if bytes.Contains(respBody, []byte("uToken")) && !bytes.Contains(respBody, []byte("streamAssistResponse")) {
			log.Printf("⚠️ [%s] 收到认证响应，标记账号需要刷新", acc.Data.Email)
			acc.InvalidateJWT()
			pool.MarkPending(acc)
			lastErr = fmt.Errorf("认证失败，需要刷新账号")
			continue
		}

		// 检查是否有实际内容（非空返回）
		hasContent := bytes.Contains(respBody, []byte(`"text"`)) || bytes.Contains(respBody, []byte(`"file"`)) || bytes.Contains(respBody, []byte(`"inlineData"`))
		if !hasContent && bytes.Contains(respBody, []byte(`"thought"`)) {
			// 只有思考内容，没有实际输出，重试
			log.Printf("⚠️ [%s] 响应只有思考内容，无实际输出，重试 (%d/%d)", acc.Data.Email, retry+1, maxRetries)
			lastErr = fmt.Errorf("空返回，只有思考内容")
			continue
		}

		usedJWT = jwt
		usedOrigAuth = acc.Data.Authorization
		usedConfigID = configID
		usedSession = session // 保存创建的 session 作为回退
		usedAcc = acc
		lastErr = nil
		break
	}

	if lastErr != nil {
		log.Printf("❌ 所有重试均失败: %v", lastErr)
		c.JSON(500, gin.H{"error": lastErr.Error()})
		return
	}

	_ = usedAcc

	// 检查空响应
	if len(respBody) == 0 {
		log.Printf("❌ 响应为空")
		c.JSON(500, gin.H{"error": "Empty response from Google"})
		return
	}

	// 解析响应：支持多种格式
	var dataList []map[string]interface{}
	var parseErr error

	// 1. 尝试标准 JSON 数组
	if parseErr = json.Unmarshal(respBody, &dataList); parseErr != nil {
		log.Printf("⚠️ JSON 数组解析失败: %v, 响应前100字符: %s", parseErr, string(respBody[:min(100, len(respBody))]))

		// 2. 尝试修复不完整的 JSON 数组
		dataList = parseIncompleteJSONArray(respBody)
		if dataList == nil {
			// 3. 尝试 NDJSON 格式
			log.Printf("⚠️ 尝试 NDJSON 格式...")
			dataList = parseNDJSON(respBody)
		}

		if len(dataList) == 0 {
			// 输出完整响应用于调试
			respStr := string(respBody)
			if len(respStr) > 500 {
				log.Printf("❌ 所有解析方式均失败, 响应长度: %d, 前500字符: %s", len(respBody), respStr[:500])
				log.Printf("❌ 后200字符: %s", respStr[len(respStr)-200:])
			} else {
				log.Printf("❌ 所有解析方式均失败, 响应长度: %d, 完整响应: %s", len(respBody), respStr)
			}
			c.JSON(500, gin.H{"error": "JSON Parse Error"})
			return
		}
		log.Printf("✅ 备用解析成功，共 %d 个对象", len(dataList))
	}

	// 检查是否有有效响应
	if len(dataList) > 0 {
		hasValidResponse := false
		hasFileContent := false
		for _, data := range dataList {
			if streamResp, ok := data["streamAssistResponse"].(map[string]interface{}); ok {
				hasValidResponse = true
				// 检查是否有文件内容
				if answer, ok := streamResp["answer"].(map[string]interface{}); ok {
					if replies, ok := answer["replies"].([]interface{}); ok {
						for _, reply := range replies {
							if replyMap, ok := reply.(map[string]interface{}); ok {
								if gc, ok := replyMap["groundedContent"].(map[string]interface{}); ok {
									if content, ok := gc["content"].(map[string]interface{}); ok {
										if _, ok := content["file"]; ok {
											hasFileContent = true
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if !hasValidResponse {
			log.Printf("⚠️ 响应中没有 streamAssistResponse，响应内容: %v", dataList[0])
		}
		log.Printf("📊 响应统计: %d 个数据块, 有效响应=%v, 包含文件=%v", len(dataList), hasValidResponse, hasFileContent)
	}

	// 从响应中提取 session（用于下载图片）
	var respSession string
	for _, data := range dataList {
		if streamResp, ok := data["streamAssistResponse"].(map[string]interface{}); ok {
			if sessionInfo, ok := streamResp["sessionInfo"].(map[string]interface{}); ok {
				if s, ok := sessionInfo["session"].(string); ok && s != "" {
					respSession = s
					break
				}
			}
		}
	}

	// 如果响应中没有 session，使用请求时创建的 session 作为回退
	if respSession == "" {
		if usedSession != "" {
			log.Printf("⚠️ 响应中未找到 session，使用请求时创建的 session: %s", usedSession)
			respSession = usedSession
		} else {
			log.Printf("⚠️ 响应中未找到 session 且无回退 session，图片/视频下载可能失败")
		}
	} else {
		log.Printf("✅ 获取到 session: %s", respSession)
	}

	// 待下载的文件信息
	type PendingFile struct {
		FileID   string
		MimeType string
	}

	if req.Stream {
		// 流式响应：文本/思考实时输出，图片最后处理
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")

		writer := c.Writer
		flusher, _ := writer.(http.Flusher)

		// 发送 role
		chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"role": "assistant"}, nil)
		fmt.Fprintf(writer, "data: %s\n\n", chunk)
		flusher.Flush()

		// 收集待下载的文件
		var pendingFiles []PendingFile

		// 第一遍：实时输出文本和思考，收集文件信息
		for _, data := range dataList {
			streamResp, ok := data["streamAssistResponse"].(map[string]interface{})
			if !ok {
				continue
			}
			answer, ok := streamResp["answer"].(map[string]interface{})
			if !ok {
				continue
			}
			replies, ok := answer["replies"].([]interface{})
			if !ok {
				continue
			}

			for _, reply := range replies {
				replyMap, ok := reply.(map[string]interface{})
				if !ok {
					continue
				}

				groundedContent, ok := replyMap["groundedContent"].(map[string]interface{})
				if !ok {
					continue
				}
				content, ok := groundedContent["content"].(map[string]interface{})
				if !ok {
					continue
				}

				// 检查是否是思考内容
				if thought, ok := content["thought"].(bool); ok && thought {
					if t, ok := content["text"].(string); ok && t != "" {
						chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"reasoning_content": t}, nil)
						fmt.Fprintf(writer, "data: %s\n\n", chunk)
						flusher.Flush()
					}
					continue
				}

				// 输出文本（实时）
				if t, ok := content["text"].(string); ok && t != "" {
					chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": t}, nil)
					fmt.Fprintf(writer, "data: %s\n\n", chunk)
					flusher.Flush()
				}

				// 处理 inlineData（直接有 base64 数据的图片）
				if inlineData, ok := content["inlineData"].(map[string]interface{}); ok {
					mime, _ := inlineData["mimeType"].(string)
					data, _ := inlineData["data"].(string)
					if mime != "" && data != "" {
						imgMarkdown := formatImageAsMarkdown(mime, data)
						chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": imgMarkdown}, nil)
						fmt.Fprintf(writer, "data: %s\n\n", chunk)
						flusher.Flush()
					}
				}

				// 收集需要下载的文件（图片/视频）
				if file, ok := content["file"].(map[string]interface{}); ok {
					fileId, _ := file["fileId"].(string)
					mimeType, _ := file["mimeType"].(string)
					if fileId != "" {
						pendingFiles = append(pendingFiles, PendingFile{FileID: fileId, MimeType: mimeType})
					}
				}
			}
		}

		// 第二遍：下载并输出文件（图片/视频）
		if len(pendingFiles) > 0 {
			log.Printf("📥 开始下载 %d 个文件...", len(pendingFiles))
			for _, pf := range pendingFiles {
				fileType := "文件"
				if strings.HasPrefix(pf.MimeType, "image/") {
					fileType = "图片"
				} else if strings.HasPrefix(pf.MimeType, "video/") {
					fileType = "视频"
				}
				log.Printf("📥 下载%s: fileId=%s", fileType, pf.FileID)

				data, err := downloadGeneratedFile(usedJWT, pf.FileID, respSession, usedConfigID, usedOrigAuth)
				if err != nil {
					log.Printf("❌ 下载%s失败: %v", fileType, err)
					continue
				}
				log.Printf("✅ %s下载成功, 大小: %d bytes", fileType, len(data))

				imgMarkdown := formatImageAsMarkdown(pf.MimeType, data)
				chunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{"content": imgMarkdown}, nil)
				fmt.Fprintf(writer, "data: %s\n\n", chunk)
				flusher.Flush()
			}
		}

		// 发送结束
		stopReason := "stop"
		finalChunk := createChunk(chatID, createdTime, req.Model, map[string]interface{}{}, &stopReason)
		fmt.Fprintf(writer, "data: %s\n\n", finalChunk)
		fmt.Fprintf(writer, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// 非流式响应：统一处理
		var fullContent strings.Builder
		var fullReasoning strings.Builder
		replyCount := 0
		hasFile := false

		for _, data := range dataList {
			streamResp, ok := data["streamAssistResponse"].(map[string]interface{})
			if !ok {
				continue
			}
			answer, ok := streamResp["answer"].(map[string]interface{})
			if !ok {
				continue
			}
			replies, ok := answer["replies"].([]interface{})
			if !ok {
				continue
			}

			for _, reply := range replies {
				replyMap, ok := reply.(map[string]interface{})
				if !ok {
					continue
				}
				replyCount++

				// 检查是否有 file 字段
				if gc, ok := replyMap["groundedContent"].(map[string]interface{}); ok {
					if content, ok := gc["content"].(map[string]interface{}); ok {
						if _, ok := content["file"]; ok {
							hasFile = true
						}
					}
				}

				text, imageData, imageMime, reasoning := extractContentFromReply(replyMap, usedJWT, respSession, usedConfigID, usedOrigAuth)

				if reasoning != "" {
					fullReasoning.WriteString(reasoning)
				}
				if text != "" {
					fullContent.WriteString(text)
				}
				if imageData != "" && imageMime != "" {
					fullContent.WriteString(formatImageAsMarkdown(imageMime, imageData))
				}
			}
		}

		// 调试日志
		log.Printf("📊 非流式响应统计: %d 个 reply, 包含文件=%v, content长度=%d, reasoning长度=%d",
			replyCount, hasFile, fullContent.Len(), fullReasoning.Len())

		// 构建响应消息
		message := gin.H{
			"role":    "assistant",
			"content": fullContent.String(),
		}
		if fullReasoning.Len() > 0 {
			message["reasoning_content"] = fullReasoning.String()
		}

		c.JSON(200, gin.H{
			"id":      chatID,
			"object":  "chat.completion",
			"created": createdTime,
			"model":   req.Model,
			"choices": []gin.H{{
				"index":         0,
				"message":       message,
				"finish_reason": "stop",
			}},
			"usage": gin.H{
				"prompt_tokens":     0,
				"completion_tokens": 0,
				"total_tokens":      0,
			},
		})
	}
}

// ==================== API Key 鉴权 ====================

func apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 如果没有配置 API Key，跳过鉴权
		if len(appConfig.APIKeys) == 0 {
			c.Next()
			return
		}

		// 从 Header 获取 API Key
		authHeader := c.GetHeader("Authorization")
		apiKey := ""

		if strings.HasPrefix(authHeader, "Bearer ") {
			apiKey = strings.TrimPrefix(authHeader, "Bearer ")
		} else {
			apiKey = c.GetHeader("X-API-Key")
		}

		if apiKey == "" {
			c.JSON(401, gin.H{"error": "Missing API key"})
			c.Abort()
			return
		}

		// 验证 API Key
		valid := false
		for _, key := range appConfig.APIKeys {
			if key == apiKey {
				valid = true
				break
			}
		}

		if !valid {
			c.JSON(401, gin.H{"error": "Invalid API key"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// ==================== 路由 ====================

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	// 加载配置
	loadAppConfig()

	// 初始化 HTTP 客户端（使用配置的代理）
	initHTTPClient()

	// 加载账号池（所有账号进入 pending 池）
	if err := pool.Load(DataDir); err != nil {
		log.Fatalf("❌ 加载账号失败: %v", err)
	}

	// 检查 CONFIG_ID
	if DefaultConfig != "" {
		log.Printf("✅ 使用默认 configId: %s", DefaultConfig)
	}

	// 检查 API Key 配置
	if len(appConfig.APIKeys) == 0 {
		log.Println("⚠️ 未配置 API Key，API 将无鉴权运行")
	}

	// 检查注册脚本
	if appConfig.Pool.RegisterScript != "" {
		scriptPath := appConfig.Pool.RegisterScript
		if !filepath.IsAbs(scriptPath) {
			scriptPath, _ = filepath.Abs(scriptPath)
		}
		if _, err := os.Stat(scriptPath); err != nil {
			log.Printf("⚠️ 注册脚本不存在: %s", scriptPath)
		}
	}

	// 异步启动号池管理器（负责刷新账号）
	if appConfig.Pool.RefreshOnStartup {
		pool.StartPoolManager()
	}

	// 如果账号数为 0，尝试自动注册
	if pool.TotalCount() == 0 && appConfig.Pool.RegisterScript != "" {
		needCount := appConfig.Pool.TargetCount
		log.Printf("📝 无账号，启动注册 %d 个...", needCount)
		startRegister(needCount)
	}

	// 启动号池维护协程（检查账号数量并触发注册）
	if appConfig.Pool.CheckIntervalMinutes > 0 && appConfig.Pool.RegisterScript != "" {
		go poolMaintainer()
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// 日志中间件
	r.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Printf("%s %s %d %v", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
	})

	// 健康检查（无需鉴权）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"time":    time.Now().UTC().Format(time.RFC3339),
			"ready":   pool.ReadyCount(),
			"pending": pool.PendingCount(),
		})
	})

	// 需要鉴权的路由组
	api := r.Group("/")
	api.Use(apiKeyAuth())

	// 模型列表
	api.GET("/v1/models", func(c *gin.Context) {
		now := time.Now().Unix()
		var models []gin.H
		for _, m := range FixedModels {
			models = append(models, gin.H{
				"id":         m,
				"object":     "model",
				"created":    now,
				"owned_by":   "google",
				"permission": []interface{}{},
			})
		}
		c.JSON(200, gin.H{"object": "list", "data": models})
	})

	// 聊天接口
	api.POST("/v1/chat/completions", func(c *gin.Context) {
		var req ChatRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		if req.Model == "" {
			req.Model = FixedModels[0]
		}

		streamChat(c, req)
	})

	// 管理接口
	admin := r.Group("/admin")
	admin.Use(apiKeyAuth())

	// 手动触发注册
	admin.POST("/register", func(c *gin.Context) {
		var req struct {
			Count int `json:"count"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Count <= 0 {
			req.Count = appConfig.Pool.TargetCount - pool.Count()
		}
		if req.Count <= 0 {
			c.JSON(200, gin.H{"message": "账号数量已足够", "count": pool.Count()})
			return
		}
		if err := startRegister(req.Count); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"message": "注册已启动", "target": req.Count})
	})

	// 刷新账号池
	admin.POST("/refresh", func(c *gin.Context) {
		pool.Load(DataDir)
		c.JSON(200, gin.H{
			"message": "刷新完成",
			"ready":   pool.ReadyCount(),
			"pending": pool.PendingCount(),
		})
	})

	// 获取状态
	admin.GET("/status", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"ready":          pool.ReadyCount(),
			"pending":        pool.PendingCount(),
			"total":          pool.TotalCount(),
			"target":         appConfig.Pool.TargetCount,
			"min":            appConfig.Pool.MinCount,
			"is_registering": atomic.LoadInt32(&isRegistering) == 1,
			"register_stats": registerStats.Get(),
		})
	})

	log.Printf("🚀 服务启动于 %s，账号: ready=%d, pending=%d", ListenAddr, pool.ReadyCount(), pool.PendingCount())
	if err := r.Run(ListenAddr); err != nil {
		log.Fatalf("❌ 服务启动失败: %v", err)
	}
}
