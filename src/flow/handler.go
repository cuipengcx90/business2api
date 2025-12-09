package flow

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// GenerationHandler Flow 生成处理器
type GenerationHandler struct {
	client *FlowClient
}

// NewGenerationHandler 创建生成处理器
func NewGenerationHandler(client *FlowClient) *GenerationHandler {
	return &GenerationHandler{client: client}
}

// GenerationRequest 生成请求
type GenerationRequest struct {
	Model  string   `json:"model"`
	Prompt string   `json:"prompt"`
	Images [][]byte `json:"images,omitempty"` // 图片字节数据
	Stream bool     `json:"stream"`
}

// GenerationResult 生成结果
type GenerationResult struct {
	Success  bool   `json:"success"`
	Type     string `json:"type"` // "image" 或 "video"
	URL      string `json:"url"`
	Error    string `json:"error,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Message  string `json:"message,omitempty"`
}

// StreamCallback 流式回调函数
type StreamCallback func(chunk string)

// HandleGeneration 处理生成请求
func (h *GenerationHandler) HandleGeneration(req GenerationRequest, streamCb StreamCallback) (*GenerationResult, error) {
	// 验证模型
	modelConfig, ok := GetFlowModelConfig(req.Model)
	if !ok {
		return &GenerationResult{
			Success: false,
			Error:   fmt.Sprintf("不支持的模型: %s", req.Model),
		}, nil
	}

	// 选择 Token
	token := h.client.SelectToken()
	if token == nil {
		return &GenerationResult{
			Success: false,
			Error:   "没有可用的 Flow Token",
		}, nil
	}

	// 确保 AT 有效
	if err := h.ensureATValid(token); err != nil {
		return &GenerationResult{
			Success: false,
			Error:   fmt.Sprintf("Token 认证失败: %v", err),
		}, nil
	}

	// 更新余额信息 (异步)
	go h.updateTokenCredits(token)

	// 确保 Project 存在
	if err := h.ensureProjectExists(token); err != nil {
		return &GenerationResult{
			Success: false,
			Error:   fmt.Sprintf("创建项目失败: %v", err),
		}, nil
	}

	// 根据类型处理
	if modelConfig.Type == ModelTypeImage {
		return h.handleImageGeneration(token, modelConfig, req, streamCb)
	} else {
		return h.handleVideoGeneration(token, modelConfig, req, streamCb)
	}
}

// ensureATValid 确保 AT 有效
func (h *GenerationHandler) ensureATValid(token *FlowToken) error {
	token.mu.Lock()
	defer token.mu.Unlock()

	// AT 还有效且未过期
	if token.AT != "" && time.Now().Before(token.ATExpires.Add(-5*time.Minute)) {
		return nil
	}

	// 刷新 AT
	resp, err := h.client.STToAT(token.ST)
	if err != nil {
		return err
	}

	token.AT = resp.AccessToken
	if resp.Expires != "" {
		if t, err := time.Parse(time.RFC3339, resp.Expires); err == nil {
			token.ATExpires = t
		}
	}
	token.Email = resp.Email

	log.Printf("[Flow] Token %s AT 已刷新, 过期时间: %v", token.ID, token.ATExpires)
	return nil
}

// updateTokenCredits 更新 Token 余额信息
func (h *GenerationHandler) updateTokenCredits(token *FlowToken) {
	if token.AT == "" {
		return
	}

	resp, err := h.client.GetCredits(token.AT)
	if err != nil {
		log.Printf("[Flow] 查询余额失败: %v", err)
		return
	}

	token.mu.Lock()
	token.Credits = resp.Credits
	token.UserPaygateTier = resp.UserPaygateTier
	token.mu.Unlock()

	log.Printf("[Flow] Token %s 余额: %d, Tier: %s", token.ID[:16]+"...", resp.Credits, resp.UserPaygateTier)
}

// ensureProjectExists 确保 Project 存在
func (h *GenerationHandler) ensureProjectExists(token *FlowToken) error {
	token.mu.Lock()
	defer token.mu.Unlock()

	if token.ProjectID != "" {
		return nil
	}

	projectID, err := h.client.CreateProject(token.ST, "Flow2API")
	if err != nil {
		return err
	}

	token.ProjectID = projectID
	log.Printf("[Flow] Token %s 创建项目: %s", token.ID, projectID)
	return nil
}

// handleImageGeneration 处理图片生成
func (h *GenerationHandler) handleImageGeneration(token *FlowToken, modelConfig ModelConfig, req GenerationRequest, streamCb StreamCallback) (*GenerationResult, error) {
	if streamCb != nil {
		streamCb(h.createStreamChunk("✨ 图片生成任务已启动\n", false))
	}

	// 上传图片 (如果有)
	var imageInputs []map[string]interface{}
	if len(req.Images) > 0 {
		if streamCb != nil {
			streamCb(h.createStreamChunk(fmt.Sprintf("上传 %d 张参考图片...\n", len(req.Images)), false))
		}

		for i, imgBytes := range req.Images {
			mediaID, err := h.client.UploadImage(token.AT, imgBytes, modelConfig.AspectRatio)
			if err != nil {
				return &GenerationResult{
					Success: false,
					Error:   fmt.Sprintf("上传图片失败: %v", err),
				}, nil
			}
			imageInputs = append(imageInputs, map[string]interface{}{
				"name":           mediaID,
				"imageInputType": "IMAGE_INPUT_TYPE_REFERENCE",
			})
			if streamCb != nil {
				streamCb(h.createStreamChunk(fmt.Sprintf("已上传第 %d/%d 张图片\n", i+1, len(req.Images)), false))
			}
		}
	}

	if streamCb != nil {
		streamCb(h.createStreamChunk("正在生成图片...\n", false))
	}

	// 调用生成 API
	result, err := h.client.GenerateImage(
		token.AT,
		token.ProjectID,
		req.Prompt,
		modelConfig.ModelName,
		modelConfig.AspectRatio,
		imageInputs,
	)
	if err != nil {
		token.mu.Lock()
		token.ErrorCount++
		token.mu.Unlock()
		return &GenerationResult{
			Success: false,
			Error:   fmt.Sprintf("生成图片失败: %v", err),
		}, nil
	}

	if result.ImageURL == "" {
		return &GenerationResult{
			Success: false,
			Error:   "生成结果为空",
		}, nil
	}

	// 更新 Token 使用
	token.mu.Lock()
	token.LastUsed = time.Now()
	token.ErrorCount = 0
	token.mu.Unlock()

	if streamCb != nil {
		streamCb(h.createStreamChunk(fmt.Sprintf("![Generated Image](%s)", result.ImageURL), true))
	}

	return &GenerationResult{
		Success: true,
		Type:    "image",
		URL:     result.ImageURL,
	}, nil
}

// handleVideoGeneration 处理视频生成
func (h *GenerationHandler) handleVideoGeneration(token *FlowToken, modelConfig ModelConfig, req GenerationRequest, streamCb StreamCallback) (*GenerationResult, error) {
	if streamCb != nil {
		streamCb(h.createStreamChunk("✨ 视频生成任务已启动\n", false))
	}

	imageCount := len(req.Images)

	// 验证图片数量
	if modelConfig.VideoType == VideoTypeT2V {
		if imageCount > 0 {
			if streamCb != nil {
				streamCb(h.createStreamChunk("⚠️ 文生视频模型不支持图片，将忽略图片仅使用文本提示词\n", false))
			}
			req.Images = nil
			imageCount = 0
		}
	} else if modelConfig.VideoType == VideoTypeI2V {
		if imageCount < modelConfig.MinImages || imageCount > modelConfig.MaxImages {
			return &GenerationResult{
				Success: false,
				Error:   fmt.Sprintf("首尾帧模型需要 %d-%d 张图片，当前提供了 %d 张", modelConfig.MinImages, modelConfig.MaxImages, imageCount),
			}, nil
		}
	}

	// 上传图片
	var startMediaID, endMediaID string
	var referenceImages []map[string]interface{}

	if modelConfig.VideoType == VideoTypeI2V && len(req.Images) > 0 {
		if streamCb != nil {
			streamCb(h.createStreamChunk("上传首帧图片...\n", false))
		}
		var err error
		startMediaID, err = h.client.UploadImage(token.AT, req.Images[0], modelConfig.AspectRatio)
		if err != nil {
			return &GenerationResult{Success: false, Error: fmt.Sprintf("上传首帧失败: %v", err)}, nil
		}

		if len(req.Images) == 2 {
			if streamCb != nil {
				streamCb(h.createStreamChunk("上传尾帧图片...\n", false))
			}
			endMediaID, err = h.client.UploadImage(token.AT, req.Images[1], modelConfig.AspectRatio)
			if err != nil {
				return &GenerationResult{Success: false, Error: fmt.Sprintf("上传尾帧失败: %v", err)}, nil
			}
		}
	} else if modelConfig.VideoType == VideoTypeR2V && len(req.Images) > 0 {
		if streamCb != nil {
			streamCb(h.createStreamChunk(fmt.Sprintf("上传 %d 张参考图片...\n", len(req.Images)), false))
		}
		for _, imgBytes := range req.Images {
			mediaID, err := h.client.UploadImage(token.AT, imgBytes, modelConfig.AspectRatio)
			if err != nil {
				return &GenerationResult{Success: false, Error: fmt.Sprintf("上传图片失败: %v", err)}, nil
			}
			referenceImages = append(referenceImages, map[string]interface{}{
				"imageUsageType": "IMAGE_USAGE_TYPE_ASSET",
				"mediaId":        mediaID,
			})
		}
	}

	if streamCb != nil {
		streamCb(h.createStreamChunk("提交视频生成任务...\n", false))
	}

	// 调用生成 API
	var videoResp *GenerateVideoResponse
	var err error

	userTier := token.UserPaygateTier
	if userTier == "" {
		userTier = "PAYGATE_TIER_ONE"
	}

	switch modelConfig.VideoType {
	case VideoTypeI2V:
		videoResp, err = h.client.GenerateVideoStartEnd(
			token.AT, token.ProjectID, req.Prompt,
			modelConfig.ModelKey, modelConfig.AspectRatio,
			startMediaID, endMediaID, userTier,
		)
	case VideoTypeR2V:
		videoResp, err = h.client.GenerateVideoReferenceImages(
			token.AT, token.ProjectID, req.Prompt,
			modelConfig.ModelKey, modelConfig.AspectRatio,
			referenceImages, userTier,
		)
	default: // T2V
		videoResp, err = h.client.GenerateVideoText(
			token.AT, token.ProjectID, req.Prompt,
			modelConfig.ModelKey, modelConfig.AspectRatio, userTier,
		)
	}

	if err != nil {
		token.mu.Lock()
		token.ErrorCount++
		token.mu.Unlock()
		return &GenerationResult{Success: false, Error: fmt.Sprintf("提交任务失败: %v", err)}, nil
	}

	if videoResp.TaskID == "" {
		return &GenerationResult{Success: false, Error: "任务创建失败"}, nil
	}

	if streamCb != nil {
		streamCb(h.createStreamChunk("视频生成中...\n", false))
	}

	// 轮询结果
	videoURL, err := h.pollVideoResult(token, videoResp.TaskID, videoResp.SceneID, streamCb)
	if err != nil {
		return &GenerationResult{Success: false, Error: err.Error()}, nil
	}

	// 更新 Token 使用
	token.mu.Lock()
	token.LastUsed = time.Now()
	token.ErrorCount = 0
	token.mu.Unlock()

	if streamCb != nil {
		streamCb(h.createStreamChunk(fmt.Sprintf("<video src='%s' controls style='max-width:100%%'></video>", videoURL), true))
	}

	return &GenerationResult{
		Success: true,
		Type:    "video",
		URL:     videoURL,
	}, nil
}

// pollVideoResult 轮询视频生成结果
func (h *GenerationHandler) pollVideoResult(token *FlowToken, taskID, sceneID string, streamCb StreamCallback) (string, error) {
	operations := []map[string]interface{}{{
		"operation": map[string]interface{}{
			"name": taskID,
		},
		"sceneId": sceneID,
	}}

	maxAttempts := h.client.config.MaxPollAttempts
	pollInterval := h.client.config.PollInterval

	for i := 0; i < maxAttempts; i++ {
		time.Sleep(time.Duration(pollInterval) * time.Second)

		resp, err := h.client.CheckVideoStatus(token.AT, operations)
		if err != nil {
			continue
		}

		// 进度更新
		if streamCb != nil && i%7 == 0 {
			progress := min(i*100/maxAttempts, 95)
			streamCb(h.createStreamChunk(fmt.Sprintf("生成进度: %d%%\n", progress), false))
		}

		switch resp.Status {
		case "MEDIA_GENERATION_STATUS_SUCCESSFUL":
			if resp.VideoURL != "" {
				return resp.VideoURL, nil
			}
		case "MEDIA_GENERATION_STATUS_ERROR_UNKNOWN",
			"MEDIA_GENERATION_STATUS_ERROR_NSFW",
			"MEDIA_GENERATION_STATUS_ERROR_PERSON",
			"MEDIA_GENERATION_STATUS_ERROR_SAFETY":
			return "", fmt.Errorf("视频生成失败: %s", resp.Status)
		}
	}

	return "", fmt.Errorf("视频生成超时 (已轮询 %d 次)", maxAttempts)
}

// createStreamChunk 创建流式响应块
func (h *GenerationHandler) createStreamChunk(content string, isFinish bool) string {
	chunk := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "flow2api",
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": nil,
		}},
	}

	if isFinish {
		chunk["choices"].([]map[string]interface{})[0]["delta"].(map[string]interface{})["content"] = content
		chunk["choices"].([]map[string]interface{})[0]["finish_reason"] = "stop"
	} else {
		chunk["choices"].([]map[string]interface{})[0]["delta"].(map[string]interface{})["reasoning_content"] = content
	}

	data, _ := json.Marshal(chunk)
	return fmt.Sprintf("data: %s\n\n", string(data))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
