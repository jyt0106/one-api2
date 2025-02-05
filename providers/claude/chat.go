package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/image"
	"one-api/common/requester"
	"one-api/types"
	"strings"
)

type claudeStreamHandler struct {
	Usage   *types.Usage
	Request *types.ChatCompletionRequest
}

func (p *ClaudeProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	claudeResponse := &ClaudeResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, claudeResponse, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	return p.convertToChatOpenai(claudeResponse, request)
}

func (p *ClaudeProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	req, errWithCode := p.getChatRequest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 发送请求
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	chatHandler := &claudeStreamHandler{
		Usage:   p.Usage,
		Request: request,
	}

	return requester.RequestStream[string](p.Requester, resp, chatHandler.handlerStream)
}

func (p *ClaudeProvider) getChatRequest(request *types.ChatCompletionRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	url, errWithCode := p.GetSupportedAPIUri(common.RelayModeChatCompletions)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 获取请求地址
	fullRequestURL := p.GetFullRequestURL(url, request.Model)
	if fullRequestURL == "" {
		return nil, common.ErrorWrapper(nil, "invalid_claude_config", http.StatusInternalServerError)
	}

	headers := p.GetRequestHeaders()
	if request.Stream {
		headers["Accept"] = "text/event-stream"
	}

	claudeRequest, errWithCode := convertFromChatOpenai(request)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 创建请求
	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(claudeRequest), p.Requester.WithHeader(headers))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}

	return req, nil
}

func convertFromChatOpenai(request *types.ChatCompletionRequest) (*ClaudeRequest, *types.OpenAIErrorWithStatusCode) {
	claudeRequest := ClaudeRequest{
		Model:         request.Model,
		Messages:      []Message{},
		System:        "",
		MaxTokens:     request.MaxTokens,
		StopSequences: nil,
		Temperature:   request.Temperature,
		TopP:          request.TopP,
		Stream:        request.Stream,
	}
	if claudeRequest.MaxTokens == 0 {
		claudeRequest.MaxTokens = 4096
	}

	for _, message := range request.Messages {
		if message.Role == "system" {
			claudeRequest.System = message.Content.(string)
			continue
		}
		content := Message{
			Role:    convertRole(message.Role),
			Content: []MessageContent{},
		}

		openaiContent := message.ParseContent()
		for _, part := range openaiContent {
			if part.Type == types.ContentTypeText {
				content.Content = append(content.Content, MessageContent{
					Type: "text",
					Text: part.Text,
				})
				continue
			}

			if part.Type == types.ContentTypeImageURL {
				mimeType, data, err := image.GetImageFromUrl(part.ImageURL.URL)
				if err != nil {
					return nil, common.ErrorWrapper(err, "image_url_invalid", http.StatusBadRequest)
				}
				content.Content = append(content.Content, MessageContent{
					Type: "image",
					Source: &ContentSource{
						Type:      "base64",
						MediaType: mimeType,
						Data:      data,
					},
				})
			}
		}
		claudeRequest.Messages = append(claudeRequest.Messages, content)
	}

	return &claudeRequest, nil
}

func (p *ClaudeProvider) convertToChatOpenai(response *ClaudeResponse, request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	error := errorHandle(&response.Error)
	if error != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *error,
			StatusCode:  http.StatusBadRequest,
		}
		return
	}

	choice := types.ChatCompletionChoice{
		Index: 0,
		Message: types.ChatCompletionMessage{
			Role:    response.Role,
			Content: strings.TrimPrefix(response.Content[0].Text, " "),
			Name:    nil,
		},
		FinishReason: stopReasonClaude2OpenAI(response.StopReason),
	}
	openaiResponse = &types.ChatCompletionResponse{
		ID:      response.Id,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Choices: []types.ChatCompletionChoice{choice},
		Model:   request.Model,
		Usage: &types.Usage{
			CompletionTokens: 0,
			PromptTokens:     0,
			TotalTokens:      0,
		},
	}

	completionTokens := response.Usage.OutputTokens

	promptTokens := response.Usage.InputTokens

	openaiResponse.Usage.PromptTokens = promptTokens
	openaiResponse.Usage.CompletionTokens = completionTokens
	openaiResponse.Usage.TotalTokens = promptTokens + completionTokens

	*p.Usage = *openaiResponse.Usage

	return openaiResponse, nil
}

// 转换为OpenAI聊天流式请求体
func (h *claudeStreamHandler) handlerStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), `data: {"type"`) {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[6:]

	var claudeResponse ClaudeStreamResponse
	err := json.Unmarshal(*rawLine, &claudeResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	error := errorHandle(&claudeResponse.Error)
	if error != nil {
		errChan <- error
		return
	}

	if claudeResponse.Type == "message_stop" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	switch claudeResponse.Type {
	case "message_start":
		h.convertToOpenaiStream(&claudeResponse, dataChan)
		h.Usage.PromptTokens = claudeResponse.Message.Usage.InputTokens

	case "message_delta":
		h.convertToOpenaiStream(&claudeResponse, dataChan)
		h.Usage.CompletionTokens = claudeResponse.Usage.OutputTokens
		h.Usage.TotalTokens = h.Usage.PromptTokens + h.Usage.CompletionTokens

	case "content_block_delta":
		h.convertToOpenaiStream(&claudeResponse, dataChan)

	default:
		return
	}
}

func (h *claudeStreamHandler) convertToOpenaiStream(claudeResponse *ClaudeStreamResponse, dataChan chan string) {
	choice := types.ChatCompletionStreamChoice{
		Index: claudeResponse.Index,
	}

	if claudeResponse.Message.Role != "" {
		choice.Delta.Role = claudeResponse.Message.Role
	}

	if claudeResponse.Delta.Text != "" {
		choice.Delta.Content = claudeResponse.Delta.Text
	}

	finishReason := stopReasonClaude2OpenAI(claudeResponse.Delta.StopReason)
	if finishReason != "" {
		choice.FinishReason = &finishReason
	}
	chatCompletion := types.ChatCompletionStreamResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", common.GetUUID()),
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   h.Request.Model,
		Choices: []types.ChatCompletionStreamChoice{choice},
	}

	responseBody, _ := json.Marshal(chatCompletion)
	dataChan <- string(responseBody)
}
