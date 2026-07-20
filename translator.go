package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type geminiTranslator struct {
	httpClient *http.Client
	baseURL    string
	model      string
	sttURL     string
}

type translateRequest struct {
	Text string `json:"text"`
}

type translateResponse struct {
	SourceText string `json:"source_text"`
	ResultText string `json:"result_text"`
	Model      string `json:"model,omitempty"`
}

type audioTranslateResult struct {
	Transcript  string `json:"transcript"`
	Translation string `json:"translation"`
}

func newGeminiTranslatorFromEnv() *geminiTranslator {
	baseURL := strings.TrimRight(strings.TrimSpace(getenvDefault("TRANSLATOR_BASE_URL", "http://10.109.190.12:11434")), "/")
	if baseURL == "" {
		return nil
	}

	model := strings.TrimSpace(getenvDefault("TRANSLATOR_MODEL", "gemma4:31b"))
	if model == "" {
		model = "gemma4:31b"
	}

	return &geminiTranslator{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    baseURL,
		model:      model,
		sttURL:     strings.TrimSpace(getenvDefault("TRANSLATOR_STT_URL", "")),
	}
}

func (t *geminiTranslator) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t == nil {
			http.Error(w, "translator unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req translateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json body", http.StatusBadRequest)
			return
		}

		req.Text = strings.TrimSpace(req.Text)
		if req.Text == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}
		if len(req.Text) > 1200 {
			http.Error(w, "text too long", http.StatusBadRequest)
			return
		}

		translated, err := t.translateATC(r.Context(), req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeTranslateJSON(w, translateResponse{
			SourceText: req.Text,
			ResultText: translated,
			Model:      t.model,
		})
	}
}

func (t *geminiTranslator) audioHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t == nil {
			http.Error(w, "translator unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if t.sttURL == "" {
			http.Error(w, "ollama translator is enabled, but audio transcription is not configured; set TRANSLATOR_STT_URL", http.StatusNotImplemented)
			return
		}

		const maxAudioBytes = 8 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, maxAudioBytes+1))
		if err != nil {
			http.Error(w, "failed to read audio body", http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			http.Error(w, "audio body is required", http.StatusBadRequest)
			return
		}
		if len(body) > maxAudioBytes {
			http.Error(w, "audio body too large", http.StatusRequestEntityTooLarge)
			return
		}

		mimeType := strings.TrimSpace(r.Header.Get("Content-Type"))
		if mimeType == "" {
			mimeType = "audio/webm"
		}
		if idx := strings.Index(mimeType, ";"); idx >= 0 {
			mimeType = strings.TrimSpace(mimeType[:idx])
		}

		result, err := t.translateATCAudio(r.Context(), mimeType, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeTranslateJSON(w, translateResponse{
			SourceText: result.Transcript,
			ResultText: result.Translation,
			Model:      t.model,
		})
	}
}

func writeTranslateJSON(w http.ResponseWriter, payload translateResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func (t *geminiTranslator) translateATC(ctx context.Context, text string) (string, error) {
	prompt := strings.Join([]string{
		"你是航空無線電通話翻譯助手。",
		"請把輸入內容翻成繁體中文，使用台灣常見用語。",
		"保留航班號、跑道號、方位、高度、速度、數字、字母代號與 ATC 專有名詞。",
		"如果原文已經是中文，就只做輕微整理。",
		"輸出只要翻譯結果，不要加說明，不要加引號。",
		"",
		text,
	}, "\n")

	return t.translateViaOllama(ctx, prompt)
}

func (t *geminiTranslator) translateATCAudio(ctx context.Context, mimeType string, audio []byte) (*audioTranslateResult, error) {
	transcript, err := t.transcribeAudio(ctx, mimeType, audio)
	if err != nil {
		return nil, err
	}

	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return &audioTranslateResult{}, nil
	}

	translation, err := t.translateATC(ctx, transcript)
	if err != nil {
		return nil, err
	}

	return &audioTranslateResult{
		Transcript:  transcript,
		Translation: strings.TrimSpace(translation),
	}, nil
}

func (t *geminiTranslator) translateViaOllama(ctx context.Context, prompt string) (string, error) {
	reqBody := map[string]any{
		"model":  t.model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"temperature": 0.2,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	endpoint := t.baseURL + "/api/generate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama generate error: %s", strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("ollama generate parse failed: %w", err)
	}

	result := strings.TrimSpace(parsed.Response)
	if result == "" {
		return "", fmt.Errorf("ollama returned empty translation")
	}
	return result, nil
}

func (t *geminiTranslator) transcribeAudio(ctx context.Context, mimeType string, audio []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.sttURL, bytes.NewReader(audio))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mimeType)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("stt error: %s", strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Text       string `json:"text"`
		Transcript string `json:"transcript"`
		Output     string `json:"output"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		if text := firstNonEmpty(parsed.Text, parsed.Transcript, parsed.Output); text != "" {
			return strings.TrimSpace(text), nil
		}
	}

	return strings.TrimSpace(string(respBody)), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
