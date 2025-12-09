// Package flow 实现 Flow Token 池管理
package flow

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// TokenPool Flow Token 池管理器
type TokenPool struct {
	mu        sync.RWMutex
	tokens    map[string]*FlowToken
	dataDir   string
	client    *FlowClient
	stopChan  chan struct{}
	watcher   *fsnotify.Watcher
	fileIndex map[string]string // fileName -> tokenID
}

// NewTokenPool 创建新的 Token 池
func NewTokenPool(dataDir string, client *FlowClient) *TokenPool {
	return &TokenPool{
		tokens:    make(map[string]*FlowToken),
		dataDir:   dataDir,
		client:    client,
		stopChan:  make(chan struct{}),
		fileIndex: make(map[string]string),
	}
}

// LoadFromDir 从目录加载所有 Token
// 每个文件包含一个完整的 cookie，自动提取 __Secure-next-auth.session-token
func (p *TokenPool) LoadFromDir() (int, error) {
	atDir := filepath.Join(p.dataDir, "at")

	// 确保目录存在
	if err := os.MkdirAll(atDir, 0755); err != nil {
		return 0, fmt.Errorf("创建目录失败: %w", err)
	}

	files, err := os.ReadDir(atDir)
	if err != nil {
		return 0, fmt.Errorf("读取目录失败: %w", err)
	}

	loaded := 0
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		filePath := filepath.Join(atDir, f.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			log.Printf("[FlowPool] 读取文件失败 %s: %v", f.Name(), err)
			continue
		}

		// 提取 session-token
		st := extractSessionToken(string(content))
		if st == "" {
			log.Printf("[FlowPool] 文件 %s 中未找到有效的 session-token", f.Name())
			continue
		}

		// 生成唯一ID
		tokenID := generateTokenID(st)

		p.mu.Lock()
		if _, exists := p.tokens[tokenID]; !exists {
			token := &FlowToken{
				ID: tokenID,
				ST: st,
			}
			p.tokens[tokenID] = token
			if p.client != nil {
				p.client.AddToken(token)
			}
			loaded++
			log.Printf("[FlowPool] 加载 Token: %s (来自 %s)", tokenID[:16]+"...", f.Name())
		}
		p.mu.Unlock()
	}

	return loaded, nil
}

// AddFromCookie 从完整 cookie 字符串添加 Token
func (p *TokenPool) AddFromCookie(cookie string) (string, error) {
	st := extractSessionToken(cookie)
	if st == "" {
		return "", fmt.Errorf("cookie 中未找到有效的 session-token")
	}

	tokenID := generateTokenID(st)

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.tokens[tokenID]; exists {
		return tokenID, fmt.Errorf("Token 已存在")
	}

	token := &FlowToken{
		ID: tokenID,
		ST: st,
	}
	p.tokens[tokenID] = token
	if p.client != nil {
		p.client.AddToken(token)
	}

	// 保存到文件
	if err := p.saveTokenToFile(tokenID, cookie); err != nil {
		log.Printf("[FlowPool] 保存 Token 到文件失败: %v", err)
	}

	return tokenID, nil
}

// saveTokenToFile 保存 Token 到文件
func (p *TokenPool) saveTokenToFile(tokenID, cookie string) error {
	atDir := filepath.Join(p.dataDir, "at")
	if err := os.MkdirAll(atDir, 0755); err != nil {
		return err
	}

	fileName := fmt.Sprintf("%s.txt", tokenID[:16])
	filePath := filepath.Join(atDir, fileName)

	return os.WriteFile(filePath, []byte(cookie), 0600)
}

// RemoveToken 移除 Token
func (p *TokenPool) RemoveToken(tokenID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.tokens[tokenID]; !exists {
		return fmt.Errorf("Token 不存在")
	}

	delete(p.tokens, tokenID)

	// 删除文件
	atDir := filepath.Join(p.dataDir, "at")
	files, _ := os.ReadDir(atDir)
	for _, f := range files {
		if strings.HasPrefix(f.Name(), tokenID[:16]) {
			os.Remove(filepath.Join(atDir, f.Name()))
			break
		}
	}

	return nil
}

// Count 返回 Token 数量
func (p *TokenPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.tokens)
}

// ReadyCount 返回可用 Token 数量
func (p *TokenPool) ReadyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, t := range p.tokens {
		if !t.Disabled && t.ErrorCount < 3 {
			count++
		}
	}
	return count
}

// Stats 返回统计信息
func (p *TokenPool) Stats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ready := 0
	disabled := 0
	errored := 0

	tokenInfos := make([]map[string]interface{}, 0)

	for _, t := range p.tokens {
		t.mu.RLock()
		info := map[string]interface{}{
			"id":          t.ID[:16] + "...",
			"email":       t.Email,
			"credits":     t.Credits,
			"disabled":    t.Disabled,
			"error_count": t.ErrorCount,
			"last_used":   t.LastUsed.Format(time.RFC3339),
		}
		t.mu.RUnlock()

		tokenInfos = append(tokenInfos, info)

		if t.Disabled {
			disabled++
		} else if t.ErrorCount >= 3 {
			errored++
		} else {
			ready++
		}
	}

	return map[string]interface{}{
		"total":    len(p.tokens),
		"ready":    ready,
		"disabled": disabled,
		"errored":  errored,
		"tokens":   tokenInfos,
	}
}

// StartRefreshWorker 启动定期刷新 AT 的 worker
func (p *TokenPool) StartRefreshWorker(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.refreshAllAT()
			case <-p.stopChan:
				return
			}
		}
	}()
	log.Printf("[FlowPool] 刷新 worker 已启动，间隔: %v", interval)
}

// Stop 停止 Token 池
func (p *TokenPool) Stop() {
	close(p.stopChan)
	if p.watcher != nil {
		p.watcher.Close()
	}
}

// StartWatcher 启动文件监听
func (p *TokenPool) StartWatcher() error {
	atDir := filepath.Join(p.dataDir, "at")

	// 确保目录存在
	if err := os.MkdirAll(atDir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建文件监听器失败: %w", err)
	}
	p.watcher = watcher

	go p.watchLoop()

	if err := watcher.Add(atDir); err != nil {
		return fmt.Errorf("添加监听目录失败: %w", err)
	}

	log.Printf("[FlowPool] 文件监听已启动: %s", atDir)
	return nil
}

// watchLoop 文件监听循环
func (p *TokenPool) watchLoop() {
	for {
		select {
		case event, ok := <-p.watcher.Events:
			if !ok {
				return
			}
			p.handleFileEvent(event)
		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[FlowPool] 文件监听错误: %v", err)
		case <-p.stopChan:
			return
		}
	}
}

// handleFileEvent 处理文件事件
func (p *TokenPool) handleFileEvent(event fsnotify.Event) {
	fileName := filepath.Base(event.Name)

	// 忽略 README 和隐藏文件
	if strings.HasPrefix(fileName, ".") || strings.EqualFold(fileName, "README.md") {
		return
	}

	switch {
	case event.Op&fsnotify.Create == fsnotify.Create:
		// 新文件创建
		time.Sleep(100 * time.Millisecond) // 等待文件写入完成
		p.loadTokenFromFile(event.Name)

	case event.Op&fsnotify.Write == fsnotify.Write:
		// 文件修改
		time.Sleep(100 * time.Millisecond)
		p.loadTokenFromFile(event.Name)

	case event.Op&fsnotify.Remove == fsnotify.Remove:
		// 文件删除
		p.removeTokenByFile(fileName)

	case event.Op&fsnotify.Rename == fsnotify.Rename:
		// 文件重命名 (视为删除)
		p.removeTokenByFile(fileName)
	}
}

// loadTokenFromFile 从单个文件加载 Token
func (p *TokenPool) loadTokenFromFile(filePath string) {
	fileName := filepath.Base(filePath)

	content, err := os.ReadFile(filePath)
	if err != nil {
		log.Printf("[FlowPool] 读取文件失败 %s: %v", fileName, err)
		return
	}

	st := extractSessionToken(string(content))
	if st == "" {
		log.Printf("[FlowPool] 文件 %s 中未找到有效的 session-token", fileName)
		return
	}

	tokenID := generateTokenID(st)

	p.mu.Lock()
	defer p.mu.Unlock()

	// 检查是否已存在
	if existingID, ok := p.fileIndex[fileName]; ok {
		if existingID == tokenID {
			// 同一个 Token，无需更新
			return
		}
		// 文件内容变了，移除旧 Token
		delete(p.tokens, existingID)
		log.Printf("[FlowPool] Token 已更新: %s", fileName)
	}

	if _, exists := p.tokens[tokenID]; !exists {
		token := &FlowToken{
			ID: tokenID,
			ST: st,
		}
		p.tokens[tokenID] = token
		p.fileIndex[fileName] = tokenID
		if p.client != nil {
			p.client.AddToken(token)
		}
		log.Printf("[FlowPool] 自动加载 Token: %s (来自 %s)", tokenID[:16]+"...", fileName)

		// 立即尝试刷新 AT
		go p.refreshSingleToken(token)
	}
}

// removeTokenByFile 根据文件名移除 Token
func (p *TokenPool) removeTokenByFile(fileName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	tokenID, ok := p.fileIndex[fileName]
	if !ok {
		return
	}

	delete(p.tokens, tokenID)
	delete(p.fileIndex, fileName)
	log.Printf("[FlowPool] Token 已移除: %s (文件 %s 已删除)", tokenID[:16]+"...", fileName)
}

// refreshSingleToken 刷新单个 Token 的 AT
func (p *TokenPool) refreshSingleToken(token *FlowToken) {
	if p.client == nil {
		return
	}

	resp, err := p.client.STToAT(token.ST)
	if err != nil {
		token.mu.Lock()
		token.ErrorCount++
		token.mu.Unlock()
		log.Printf("[FlowPool] Token %s AT 刷新失败: %v", token.ID[:16]+"...", err)
		return
	}

	token.mu.Lock()
	token.AT = resp.AccessToken
	if resp.Expires != "" {
		if t, err := time.Parse(time.RFC3339, resp.Expires); err == nil {
			token.ATExpires = t
		}
	}
	token.Email = resp.Email
	token.ErrorCount = 0
	token.Disabled = false
	token.mu.Unlock()

	log.Printf("[FlowPool] Token %s AT 已刷新, Email: %s", token.ID[:16]+"...", resp.Email)
}

// refreshAllAT 刷新所有 Token 的 AT
func (p *TokenPool) refreshAllAT() {
	p.mu.RLock()
	tokens := make([]*FlowToken, 0, len(p.tokens))
	for _, t := range p.tokens {
		tokens = append(tokens, t)
	}
	p.mu.RUnlock()

	for _, token := range tokens {
		token.mu.Lock()
		// 检查是否需要刷新
		needRefresh := token.AT == "" || time.Now().After(token.ATExpires.Add(-5*time.Minute))
		token.mu.Unlock()

		if !needRefresh {
			continue
		}

		if p.client == nil {
			continue
		}

		resp, err := p.client.STToAT(token.ST)
		if err != nil {
			token.mu.Lock()
			token.ErrorCount++
			if token.ErrorCount >= 3 {
				token.Disabled = true
				log.Printf("[FlowPool] Token %s 刷新失败次数过多，已禁用: %v", token.ID[:16]+"...", err)
			}
			token.mu.Unlock()
			continue
		}

		token.mu.Lock()
		token.AT = resp.AccessToken
		if resp.Expires != "" {
			if t, err := time.Parse(time.RFC3339, resp.Expires); err == nil {
				token.ATExpires = t
			}
		}
		token.Email = resp.Email
		token.ErrorCount = 0
		token.Disabled = false
		token.mu.Unlock()

		log.Printf("[FlowPool] Token %s AT 已刷新, Email: %s", token.ID[:16]+"...", resp.Email)
	}
}

// extractSessionToken 从 cookie 字符串提取 __Secure-next-auth.session-token
func extractSessionToken(cookie string) string {
	// 正则匹配 __Secure-next-auth.session-token=...
	// Token 可能以 ; 或空格或行尾结束
	patterns := []string{
		`__Secure-next-auth\.session-token=([^;\s]+)`,
		`__Secure-next-auth\.session-token=([^\s;]+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(cookie)
		if len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}

	// 如果输入本身就是 token（不包含 = 的长字符串）
	cookie = strings.TrimSpace(cookie)
	if !strings.Contains(cookie, "=") && len(cookie) > 100 {
		return cookie
	}

	return ""
}

// generateTokenID 根据 ST 生成唯一 ID
func generateTokenID(st string) string {
	hash := md5.Sum([]byte(st))
	return hex.EncodeToString(hash[:])
}
