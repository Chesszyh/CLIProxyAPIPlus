package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// OpenCodeExecutor talks to a local OpenCode server. OpenCode owns provider
// credentials; the proxy only drives its local HTTP API.
type OpenCodeExecutor struct {
	cfg *config.Config
}

// NewOpenCodeExecutor creates an executor for local OpenCode servers.
func NewOpenCodeExecutor(cfg *config.Config) *OpenCodeExecutor {
	return &OpenCodeExecutor{cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenCodeExecutor) Identifier() string { return "opencode" }

// PrepareRequest injects OpenCode server authentication and custom headers.
func (e *OpenCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if password := openCodeServerPassword(auth); password != "" {
		token := base64.StdEncoding.EncodeToString([]byte("opencode:" + password))
		req.Header.Set("Authorization", "Basic "+token)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenCode server authentication and executes the request.
func (e *OpenCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("opencode executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *OpenCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	openAIResp, translated, headers, err := e.executeOpenCodeChat(ctx, auth, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, opts.SourceFormat, req.Model, opts.OriginalRequest, translated, openAIResp, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *OpenCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	streamOpts := opts
	streamOpts.Stream = true
	openAIResp, translated, headers, err := e.executeOpenCodeChat(ctx, auth, req, streamOpts)
	if err != nil {
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk, 3)
	go func() {
		defer close(out)
		model := gjson.GetBytes(openAIResp, "model").String()
		if model == "" {
			model = thinking.ParseSuffix(req.Model).ModelName
		}
		content := gjson.GetBytes(openAIResp, "choices.0.message.content").String()
		var param any
		for _, line := range buildOpenCodeSSEChunks(model, content) {
			chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, opts.SourceFormat, req.Model, opts.OriginalRequest, translated, line, &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *OpenCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = auth
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FormatOpenAI
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("opencode executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("opencode executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op because OpenCode manages provider credentials itself.
func (e *OpenCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *OpenCodeExecutor) executeOpenCodeChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ []byte, _ []byte, _ http.Header, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FormatOpenAI
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel, requestPath)
	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, nil, nil, err
	}

	promptBody, promptText, resolvedModel, err := e.buildOpenCodePrompt(ctx, auth, translated, baseModel)
	if err != nil {
		return nil, nil, nil, err
	}

	if errStart := e.ensureOpenCodeServer(ctx, auth); errStart != nil {
		return nil, nil, nil, errStart
	}

	sessionID, sessionHeaders, err := e.createOpenCodeSession(ctx, auth)
	if err != nil {
		return nil, nil, nil, err
	}
	messageBody, messageHeaders, err := e.sendOpenCodeMessage(ctx, auth, sessionID, promptBody)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(messageHeaders) == 0 {
		messageHeaders = sessionHeaders
	}

	content, reasoning, finish := parseOpenCodeAssistantMessage(messageBody)
	if content == "" && reasoning == "" {
		polledContent, polledReasoning, polledFinish, errPoll := e.pollOpenCodeAssistantMessage(ctx, auth, sessionID)
		if errPoll == nil {
			content, reasoning, finish = polledContent, polledReasoning, polledFinish
		}
		if content == "" && reasoning == "" {
			content = strings.TrimSpace(string(messageBody))
		}
	}

	openAIResp := buildOpenCodeOpenAIResponse(resolvedModel, content, reasoning, finish, promptText)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(openAIResp))
	reporter.EnsurePublished(ctx)
	return openAIResp, translated, messageHeaders, nil
}

func (e *OpenCodeExecutor) pollOpenCodeAssistantMessage(ctx context.Context, auth *cliproxyauth.Auth, sessionID string) (content, reasoning, finish string, err error) {
	path := "/session/" + url.PathEscape(sessionID) + "/message"
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, _, errPoll := e.doOpenCodeRequest(ctx, auth, http.MethodGet, path, nil)
		if errPoll == nil {
			content, reasoning, finish = parseOpenCodeAssistantMessage(body)
			if content != "" || reasoning != "" || finish != "" {
				return content, reasoning, finish, nil
			}
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", "", ctxErr
		} else {
			err = errPoll
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", "", "", err
			}
			return "", "", "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *OpenCodeExecutor) buildOpenCodePrompt(ctx context.Context, auth *cliproxyauth.Auth, payload []byte, fallbackModel string) (map[string]any, string, string, error) {
	modelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if modelName == "" {
		modelName = fallbackModel
	}
	modelName = thinking.ParseSuffix(modelName).ModelName
	modelName = e.resolveConfiguredModel(auth, modelName)
	providerID, modelID := splitOpenCodeModel(modelName)

	systemChunks, parts := openCodePromptParts(payload)
	if len(parts) == 0 {
		return nil, "", modelName, statusErr{code: http.StatusBadRequest, msg: "opencode: request must contain at least one non-system text message"}
	}

	body := map[string]any{
		"model": map[string]string{
			"providerID": providerID,
			"modelID":    modelID,
		},
		"parts": parts,
	}
	if len(systemChunks) > 0 {
		body["system"] = strings.Join(systemChunks, "\n\n")
	}
	if maxTokens := gjson.GetBytes(payload, "max_tokens"); maxTokens.Exists() {
		body["max_tokens"] = maxTokens.Value()
	}
	if temperature := gjson.GetBytes(payload, "temperature"); temperature.Exists() {
		body["temperature"] = temperature.Value()
	}
	if topP := gjson.GetBytes(payload, "top_p"); topP.Exists() {
		body["top_p"] = topP.Value()
	}
	if stop := gjson.GetBytes(payload, "stop"); stop.Exists() {
		body["stop"] = stop.Value()
	}
	if openCodeDisableTools(auth) {
		if tools := e.fetchDisabledOpenCodeTools(ctx, auth); len(tools) > 0 {
			body["tools"] = tools
		}
	}

	promptText := make([]string, 0, len(parts))
	for _, part := range parts {
		promptText = append(promptText, part.Text)
	}
	return body, strings.Join(promptText, "\n\n"), providerID + "/" + modelID, nil
}

type openCodeTextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func openCodePromptParts(payload []byte) ([]string, []openCodeTextPart) {
	systemChunks := make([]string, 0)
	parts := make([]openCodeTextPart, 0)
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, message gjson.Result) bool {
			role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
			text := strings.TrimSpace(openCodeContentText(message.Get("content")))
			if text == "" {
				return true
			}
			if role == "system" || role == "developer" {
				systemChunks = append(systemChunks, text)
				return true
			}
			if role == "" {
				role = "user"
			}
			parts = append(parts, openCodeTextPart{
				Type: "text",
				Text: strings.ToUpper(role) + ": " + text,
			})
			return true
		})
	}
	if len(parts) > 0 {
		return systemChunks, parts
	}

	input := gjson.GetBytes(payload, "input")
	if input.Type == gjson.String {
		if text := strings.TrimSpace(input.String()); text != "" {
			parts = append(parts, openCodeTextPart{Type: "text", Text: "USER: " + text})
		}
		return systemChunks, parts
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			if role == "" {
				role = "user"
			}
			text := strings.TrimSpace(openCodeContentText(item.Get("content")))
			if text == "" {
				text = strings.TrimSpace(item.Get("text").String())
			}
			if text != "" {
				parts = append(parts, openCodeTextPart{Type: "text", Text: strings.ToUpper(role) + ": " + text})
			}
			return true
		})
	}
	return systemChunks, parts
}

func openCodeContentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		out := make([]string, 0)
		content.ForEach(func(_, part gjson.Result) bool {
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			switch partType {
			case "", "text", "input_text", "output_text":
				if text := strings.TrimSpace(part.Get("text").String()); text != "" {
					out = append(out, text)
				}
			}
			return true
		})
		return strings.Join(out, "\n")
	}
	if content.Exists() {
		return content.Raw
	}
	return ""
}

func (e *OpenCodeExecutor) createOpenCodeSession(ctx context.Context, auth *cliproxyauth.Auth) (string, http.Header, error) {
	body, headers, err := e.doOpenCodeRequest(ctx, auth, http.MethodPost, "/session", []byte(`{}`))
	if err != nil {
		return "", headers, err
	}
	id := strings.TrimSpace(gjson.GetBytes(body, "id").String())
	if id == "" {
		id = strings.TrimSpace(gjson.GetBytes(body, "data.id").String())
	}
	if id == "" {
		return "", headers, fmt.Errorf("opencode executor: create session response missing id")
	}
	return id, headers, nil
}

func (e *OpenCodeExecutor) sendOpenCodeMessage(ctx context.Context, auth *cliproxyauth.Auth, sessionID string, body map[string]any) ([]byte, http.Header, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("opencode executor: marshal message request: %w", err)
	}
	return e.doOpenCodeRequest(ctx, auth, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", payload)
}

func (e *OpenCodeExecutor) doOpenCodeRequest(ctx context.Context, auth *cliproxyauth.Auth, method, path string, body []byte) ([]byte, http.Header, error) {
	baseURL := openCodeBaseURL(auth)
	if baseURL == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "opencode executor: missing base_url"}
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-opencode")
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, nil, err
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       httpReq.URL.String(),
		Method:    method,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, httpResp.Header.Clone(), err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, respBody)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, httpResp.Header.Clone(), statusErr{code: httpResp.StatusCode, msg: string(respBody)}
	}
	return respBody, httpResp.Header.Clone(), nil
}

func parseOpenCodeAssistantMessage(body []byte) (content, reasoning, finish string) {
	if len(body) == 0 {
		return "", "", ""
	}
	root := gjson.ParseBytes(body)
	if root.IsArray() {
		root.ForEach(func(_, item gjson.Result) bool {
			role := strings.ToLower(strings.TrimSpace(item.Get("info.role").String()))
			if role == "" {
				role = strings.ToLower(strings.TrimSpace(item.Get("role").String()))
			}
			if role == "assistant" {
				content, reasoning, finish = parseOpenCodeAssistantMessage([]byte(item.Raw))
			}
			return true
		})
		return content, reasoning, finish
	}
	if v := strings.TrimSpace(root.Get("info.finish").String()); v != "" {
		finish = v
	}
	root.Get("parts").ForEach(func(_, part gjson.Result) bool {
		partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
		text := part.Get("text").String()
		if text == "" {
			text = part.Get("delta").String()
		}
		switch partType {
		case "text":
			content += text
		case "reasoning":
			reasoning += text
		}
		return true
	})
	if content == "" {
		content = root.Get("message.content").String()
	}
	return content, reasoning, finish
}

func buildOpenCodeOpenAIResponse(model, content, reasoning, finish, promptText string) []byte {
	if finish == "" {
		finish = "stop"
	}
	promptTokens := int64((len(promptText) + 3) / 4)
	completionTokens := int64((len(content) + len(reasoning) + 3) / 4)
	resp := map[string]any{
		"id":      "chatcmpl-" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": finish,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	out, _ := json.Marshal(resp)
	return out
}

func buildOpenCodeSSEChunks(model, content string) [][]byte {
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	first := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{"role": "assistant", "content": content},
				"finish_reason": nil,
			},
		},
	}
	last := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			},
		},
	}
	firstJSON, _ := json.Marshal(first)
	lastJSON, _ := json.Marshal(last)
	return [][]byte{
		[]byte("data: " + string(firstJSON)),
		[]byte("data: " + string(lastJSON)),
		[]byte("data: [DONE]"),
	}
}

// FetchOpenCodeModels fetches and flattens OpenCode's provider/model catalog.
func FetchOpenCodeModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	exec := NewOpenCodeExecutor(cfg)
	models, err := exec.fetchOpenCodeModels(ctx, auth)
	if err != nil && openCodeAutoStart(auth) {
		if errStart := exec.ensureOpenCodeServer(ctx, auth); errStart == nil {
			models, err = exec.fetchOpenCodeModels(ctx, auth)
		} else {
			log.Warnf("opencode: failed to auto-start server for model discovery: %v", errStart)
		}
	}
	if err != nil {
		log.Warnf("opencode: failed to fetch models: %v", err)
		return nil
	}
	return models
}

func (e *OpenCodeExecutor) fetchOpenCodeModels(ctx context.Context, auth *cliproxyauth.Auth) ([]*registry.ModelInfo, error) {
	body, _, err := e.doOpenCodeRequest(ctx, auth, http.MethodGet, "/config/providers", nil)
	if err != nil {
		return nil, err
	}

	type openCodeModel struct {
		Name  string `json:"name"`
		Limit struct {
			Context int `json:"context"`
			Output  int `json:"output"`
		} `json:"limit"`
		Capabilities struct {
			Reasoning bool `json:"reasoning"`
		} `json:"capabilities"`
	}
	type openCodeProvider struct {
		ID     string                   `json:"id"`
		Name   string                   `json:"name"`
		Models map[string]openCodeModel `json:"models"`
	}
	var parsed struct {
		Providers []openCodeProvider `json:"providers"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("opencode: parse provider catalog: %w", err)
	}

	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0)
	for _, provider := range parsed.Providers {
		providerID := strings.TrimSpace(provider.ID)
		if providerID == "" {
			continue
		}
		for modelID, model := range provider.Models {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			fullID := providerID + "/" + modelID
			display := strings.TrimSpace(model.Name)
			if display == "" {
				display = fullID
			}
			info := &registry.ModelInfo{
				ID:                  fullID,
				Object:              "model",
				Created:             now,
				OwnedBy:             providerID,
				Type:                "opencode",
				DisplayName:         display,
				ContextLength:       model.Limit.Context,
				MaxCompletionTokens: model.Limit.Output,
				SupportedEndpoints:  []string{"/chat/completions", "/responses"},
			}
			if model.Capabilities.Reasoning {
				info.Thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
			}
			models = append(models, info)
		}
	}
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID)
	})
	return models, nil
}

func (e *OpenCodeExecutor) ensureOpenCodeServer(ctx context.Context, auth *cliproxyauth.Auth) error {
	if err := e.checkOpenCodeHealth(ctx, auth); err == nil {
		return nil
	} else if !openCodeAutoStart(auth) {
		return err
	}
	return e.startOpenCodeServer(ctx, auth)
}

func (e *OpenCodeExecutor) checkOpenCodeHealth(ctx context.Context, auth *cliproxyauth.Auth) error {
	body, _, err := e.doOpenCodeRequest(ctx, auth, http.MethodGet, "/global/health", nil)
	if err != nil {
		return err
	}
	if healthy := gjson.GetBytes(body, "healthy"); healthy.Exists() && !healthy.Bool() {
		return fmt.Errorf("opencode: health check unhealthy")
	}
	return nil
}

var (
	openCodeProcessMu sync.Mutex
	openCodeProcesses = make(map[string]*exec.Cmd)
)

func (e *OpenCodeExecutor) startOpenCodeServer(ctx context.Context, auth *cliproxyauth.Auth) error {
	baseURL := openCodeBaseURL(auth)
	if baseURL == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "opencode executor: missing base_url"}
	}

	openCodeProcessMu.Lock()
	if cmd := openCodeProcesses[baseURL]; cmd != nil && cmd.Process != nil {
		openCodeProcessMu.Unlock()
		return e.waitForOpenCodeServer(ctx, auth)
	}
	command := openCodeCommand(auth)
	args, err := openCodeServeArgs(baseURL)
	if err != nil {
		openCodeProcessMu.Unlock()
		return err
	}
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	if password := openCodeServerPassword(auth); password != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+password)
	}
	if err = cmd.Start(); err != nil {
		openCodeProcessMu.Unlock()
		return fmt.Errorf("opencode: start %s %s: %w", command, strings.Join(args, " "), err)
	}
	openCodeProcesses[baseURL] = cmd
	openCodeProcessMu.Unlock()

	go func() {
		errWait := cmd.Wait()
		openCodeProcessMu.Lock()
		if openCodeProcesses[baseURL] == cmd {
			delete(openCodeProcesses, baseURL)
		}
		openCodeProcessMu.Unlock()
		if errWait != nil {
			log.Debugf("opencode server exited: %v", errWait)
		}
	}()

	return e.waitForOpenCodeServer(ctx, auth)
}

func (e *OpenCodeExecutor) waitForOpenCodeServer(ctx context.Context, auth *cliproxyauth.Auth) error {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := e.checkOpenCodeHealth(waitCtx, auth); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("opencode: server did not become healthy: %w", lastErr)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func openCodeServeArgs(baseURL string) ([]string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("opencode: invalid base_url %q: %w", baseURL, err)
	}
	host := parsed.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := parsed.Port()
	if port == "" {
		port = "4096"
	}
	if _, err = strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("opencode: invalid base_url port %q: %w", port, err)
	}
	return []string{"serve", "--port", port, "--hostname", host}, nil
}

func (e *OpenCodeExecutor) fetchDisabledOpenCodeTools(ctx context.Context, auth *cliproxyauth.Auth) map[string]bool {
	body, _, err := e.doOpenCodeRequest(ctx, auth, http.MethodGet, "/experimental/tool/ids", nil)
	if err != nil {
		log.Debugf("opencode: tool id fetch failed: %v", err)
		return nil
	}
	tools := make(map[string]bool)
	root := gjson.ParseBytes(body)
	if root.IsArray() {
		root.ForEach(func(_, item gjson.Result) bool {
			if id := strings.TrimSpace(item.String()); id != "" {
				tools[id] = false
			}
			return true
		})
	}
	return tools
}

func (e *OpenCodeExecutor) resolveConfiguredModel(auth *cliproxyauth.Auth, model string) string {
	model = strings.TrimSpace(model)
	if model == "" || e == nil || e.cfg == nil {
		return model
	}
	entry := e.resolveConfig(auth)
	if entry == nil {
		return model
	}
	modelKey := strings.ToLower(thinking.ParseSuffix(model).ModelName)
	for i := range entry.Models {
		candidate := entry.Models[i]
		name := strings.TrimSpace(candidate.Name)
		alias := strings.TrimSpace(candidate.Alias)
		if alias != "" && strings.EqualFold(strings.ToLower(thinking.ParseSuffix(alias).ModelName), modelKey) && name != "" {
			return name
		}
		if name != "" && strings.EqualFold(strings.ToLower(thinking.ParseSuffix(name).ModelName), modelKey) {
			return name
		}
	}
	return model
}

func (e *OpenCodeExecutor) resolveConfig(auth *cliproxyauth.Auth) *config.OpenCodeKey {
	if e == nil || e.cfg == nil {
		return nil
	}
	attrBase := openCodeBaseURL(auth)
	attrPassword := openCodeServerPassword(auth)
	for i := range e.cfg.OpenCode {
		entry := &e.cfg.OpenCode[i]
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if cfgBase == "" {
			cfgBase = config.DefaultOpenCodeBaseURL
		}
		if strings.TrimSuffix(cfgBase, "/") != attrBase {
			continue
		}
		cfgPassword := strings.TrimSpace(entry.ServerPassword)
		if attrPassword != "" && cfgPassword != attrPassword {
			continue
		}
		return entry
	}
	return nil
}

func splitOpenCodeModel(model string) (string, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "opencode", "big-pickle"
	}
	providerID, modelID, ok := strings.Cut(model, "/")
	if !ok || strings.TrimSpace(modelID) == "" {
		return "opencode", model
	}
	return strings.TrimSpace(providerID), strings.TrimSpace(modelID)
}

func openCodeBaseURL(auth *cliproxyauth.Auth) string {
	baseURL := ""
	if auth != nil && auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if baseURL == "" {
		baseURL = config.DefaultOpenCodeBaseURL
	}
	return strings.TrimSuffix(baseURL, "/")
}

func openCodeServerPassword(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["server_password"])
}

func openCodeCommand(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if command := strings.TrimSpace(auth.Attributes["command"]); command != "" {
			return command
		}
	}
	return "opencode"
}

func openCodeAutoStart(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if value, ok := auth.Attributes["auto_start"]; ok {
			return !strings.EqualFold(strings.TrimSpace(value), "false")
		}
	}
	return true
}

func openCodeDisableTools(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if value, ok := auth.Attributes["disable_tools"]; ok {
			return !strings.EqualFold(strings.TrimSpace(value), "false")
		}
	}
	return true
}
