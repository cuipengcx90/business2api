package utils

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"time"

	"business2api/src/logger"
	"business2api/src/pool"
)

// ==================== HTTP 客户端 ====================

var HTTPClient *http.Client

// NewHTTPClient 创建 HTTP 客户端
func NewHTTPClient(proxy string) *http.Client {
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}

	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   1800 * time.Second,
	}
}

// InitHTTPClient 初始化全局 HTTP 客户端
func InitHTTPClient(proxy string) {
	HTTPClient = NewHTTPClient(proxy)
	pool.HTTPClient = HTTPClient
	if proxy != "" {
		logger.Info("✅ 使用代理: %s", proxy)
	}
}

// ReadResponseBody 读取 HTTP 响应体（支持 gzip）
func ReadResponseBody(resp *http.Response) ([]byte, error) {
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

// ParseNDJSON 解析 NDJSON 格式数据
func ParseNDJSON(data []byte) []map[string]interface{} {
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

// ParseIncompleteJSONArray 解析可能不完整的 JSON 数组
func ParseIncompleteJSONArray(data []byte) []map[string]interface{} {
	var result []map[string]interface{}
	if err := json.Unmarshal(data, &result); err == nil {
		return result
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if trimmed[len(trimmed)-1] != ']' {
			lastBrace := bytes.LastIndex(trimmed, []byte("}"))
			if lastBrace > 0 {
				fixed := append(trimmed[:lastBrace+1], ']')
				if err := json.Unmarshal(fixed, &result); err == nil {
					logger.Warn("JSON 数组不完整，已修复")
					return result
				}
			}
		}
	}
	return nil
}

// TruncateString 截断字符串
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Min 返回两个整数中的较小值
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
