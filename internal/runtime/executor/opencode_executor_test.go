package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestFetchOpenCodeModelsFromServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/providers" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"providers": [
				{
					"id": "opencode",
					"name": "OpenCode Zen",
					"models": {
						"big-pickle": {"name": "Big Pickle", "limit": {"context": 200000, "output": 16000}},
						"minimax-m2.5-free": {"name": "MiniMax M2.5", "limit": {"context": 100000, "output": 8000}}
					}
				}
			],
			"default": {}
		}`))
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "opencode-auth",
		Provider: "opencode",
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}

	models := FetchOpenCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "opencode/big-pickle" {
		t.Fatalf("expected first model opencode/big-pickle, got %s", models[0].ID)
	}
	if models[1].ID != "opencode/minimax-m2.5-free" {
		t.Fatalf("expected second model opencode/minimax-m2.5-free, got %s", models[1].ID)
	}
}

func TestOpenCodeExecutorExecuteCreatesSessionPrompt(t *testing.T) {
	var promptBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.2.26"}`))
		case "/session":
			if r.Method != http.MethodPost {
				t.Fatalf("expected POST /session, got %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"id":"ses_test"}`))
		case "/session/ses_test/message":
			if r.Method != http.MethodPost {
				t.Fatalf("expected POST prompt, got %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&promptBody); err != nil {
				t.Fatalf("decode prompt body: %v", err)
			}
			_, _ = w.Write([]byte(`{
				"info": {"id":"msg_test","sessionID":"ses_test","role":"assistant","finish":"stop"},
				"parts": [
					{"id":"prt_reason","type":"reasoning","text":"thinking"},
					{"id":"prt_text","type":"text","text":"hello from opencode"}
				]
			}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "opencode-auth",
		Provider: "opencode",
		Attributes: map[string]string{
			"base_url":      server.URL,
			"auto_start":    "false",
			"disable_tools": "false",
		},
	}
	payload := []byte(`{
		"model": "opencode/big-pickle",
		"messages": [
			{"role":"system","content":"be concise"},
			{"role":"user","content":"hi"}
		],
		"stream": false
	}`)

	exec := NewOpenCodeExecutor(&config.Config{})
	resp, err := exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "opencode/big-pickle",
		Payload: payload,
	}, coreexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "hello from opencode" {
		t.Fatalf("unexpected content %q in %s", got, string(resp.Payload))
	}
	model, _ := promptBody["model"].(map[string]any)
	if model["providerID"] != "opencode" || model["modelID"] != "big-pickle" {
		t.Fatalf("unexpected opencode model payload: %#v", model)
	}
	if promptBody["system"] != "be concise" {
		t.Fatalf("unexpected system prompt %q", promptBody["system"])
	}
	parts, _ := promptBody["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("expected one prompt part, got %#v", parts)
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	if !strings.Contains(text, "USER: hi") {
		t.Fatalf("expected user text in prompt part, got %q", text)
	}
}

func TestOpenCodeExecutorExecutePollsSessionMessagesWhenPromptReturnsEmpty(t *testing.T) {
	getMessages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"1.2.26"}`))
		case r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_test"}`))
		case r.URL.Path == "/session/ses_test/message" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/session/ses_test/message" && r.Method == http.MethodGet:
			getMessages++
			if getMessages == 1 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{
					"info": {"id":"msg_user","sessionID":"ses_test","role":"user"}
				},
				{
					"info": {"id":"msg_assistant","sessionID":"ses_test","role":"assistant","finish":"stop"},
					"parts": [{"id":"part_text","type":"text","text":"polled response"}]
				}
			]`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	auth := &coreauth.Auth{
		ID:       "opencode-auth",
		Provider: "opencode",
		Attributes: map[string]string{
			"base_url":      server.URL,
			"auto_start":    "false",
			"disable_tools": "false",
		},
	}
	payload := []byte(`{"model":"opencode/big-pickle","messages":[{"role":"user","content":"hi"}]}`)

	exec := NewOpenCodeExecutor(&config.Config{})
	resp, err := exec.Execute(context.Background(), auth, coreexecutor.Request{
		Model:   "opencode/big-pickle",
		Payload: payload,
	}, coreexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("execute returned error: %v", err)
	}
	if getMessages < 2 {
		t.Fatalf("expected executor to keep polling session messages, got %d GETs", getMessages)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "polled response" {
		t.Fatalf("unexpected content %q in %s", got, string(resp.Payload))
	}
}
