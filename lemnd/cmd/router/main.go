// LEMN Model Router
//
// Point Pi's model base_url at this service (http://localhost:8090/v1)
// and its api_key at LEMN_SHARED_SECRET's value — OpenAI-compatible
// clients send that as "Authorization: Bearer <api_key>" by convention,
// which is exactly what this service's auth check expects, so no
// Pi-side code changes are needed beyond the standard config fields.
//
// Every request's last user message is classified by the Laya service
// (see layarouter/server.py) for requires_reasoning, then the full
// original request is forwarded unmodified to whichever backend was
// chosen, and the response is streamed straight back.
//
// Fails open to the HEAVY backend on any classifier error or timeout:
// misrouting a genuinely hard query to the fast model degrades answer
// quality, while misrouting an easy query to the heavy model only costs
// latency. That asymmetry is why the failure direction is deliberate.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lemnd/internal/authmw"
)

// Content is raw because OpenAI-compatible clients send either a plain string
// or an array of typed parts; Pi sends the latter.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

type chatRequest struct {
	Messages []chatMessage `json:"messages"`
}

type classifyRequest struct {
	Query string `json:"query"`
}

type classifyResponse struct {
	RequiresReasoning bool    `json:"requires_reasoning"`
	Probability       float64 `json:"probability"`
}

var (
	layaEndpoint   = getenv("LEMN_LAYA_URL", "http://localhost:8002/classify")
	fastModelURL   = getenv("LEMN_FAST_MODEL_URL", "http://localhost:8010/v1/chat/completions")
	heavyModelURL  = getenv("LEMN_HEAVY_MODEL_URL", "http://localhost:8011/v1/chat/completions")
	fastModelName  = os.Getenv("LEMN_FAST_MODEL_NAME")
	heavyModelName = os.Getenv("LEMN_HEAVY_MODEL_NAME")
	backendAPIKey  = os.Getenv("LEMN_BACKEND_API_KEY")
	routerBind     = getenv("LEMN_ROUTER_BIND", "127.0.0.1:8090")
	// Laya is single-process and CPU-bound, so a classify can queue behind the
	// daemon's memory-worthiness call. Too low here silently routes everything
	// to the heavy model via the fail-open path.
	classifyTimeout = getenvDuration("LEMN_CLASSIFY_TIMEOUT", 5*time.Second)
	// Inactivity limit, not a total deadline: long generations on local models
	// run well past any fixed cap. Must also cover prefill before the first byte.
	backendIdleTimeout = getenvDuration("LEMN_BACKEND_IDLE_TIMEOUT", 25*time.Minute)
	// Prompts above this go to heavy regardless of the classifier: the fast model's
	// window is smaller, and a short follow-up deep in a task is not an easy task.
	longContextTokens = getenvInt("LEMN_LONG_CONTEXT_TOKENS", 30000)
	sharedSecret      string
)

var errBackendIdle = errors.New("backend idle timeout exceeded")

// backend is the chosen destination. model is non-empty for servers that host
// several models behind a single URL (oMLX), where the request body — not the
// address — selects which one runs.
type backend struct {
	url   string
	model string
	label string  // "fast" or "heavy", surfaced to the client
	prob  float64 // negative when the classifier did not answer
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func main() {
	sharedSecret = os.Getenv("LEMN_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatal("LEMN_SHARED_SECRET is not set — refusing to start unauthenticated. " +
			"Set it to the same value configured on the daemon and Laya service, " +
			"and set it as Pi's model api_key.")
	}

	http.HandleFunc("/v1/chat/completions", authmw.Require(sharedSecret, handleChatCompletions))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("LEMN model router listening on %s (fast=%s, heavy=%s, laya=%s)",
		routerBind, fastModelURL, heavyModelURL, layaEndpoint)
	log.Fatal(http.ListenAndServe(routerBind, nil))
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var req chatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "invalid chat request: "+err.Error(), http.StatusBadRequest)
		return
	}

	lastUserMsg := ""
	var streamRequest struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(bodyBytes, &streamRequest)
	if streamRequest.Stream {
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(bodyBytes, &raw)
		var options map[string]interface{}
		_ = json.Unmarshal(raw["stream_options"], &options)
		if options == nil {
			options = make(map[string]interface{})
		}
		options["include_usage"] = true
		encoded, _ := json.Marshal(options)
		raw["stream_options"] = encoded
		bodyBytes, _ = json.Marshal(raw)
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserMsg = contentText(req.Messages[i].Content)
			break
		}
	}

	// ~3 bytes per token over-estimates for JSON-escaped code, erring toward heavy.
	chosen := chooseBackend(lastUserMsg, len(bodyBytes)/3)
	if chosen.model != "" {
		rewritten, err := withModel(bodyBytes, chosen.model)
		if err != nil {
			log.Printf("[Router] failed to rewrite model field: %v — forwarding original body", err)
		} else {
			bodyBytes = rewritten
		}
	}

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	idle := time.AfterFunc(backendIdleTimeout, func() { cancel(errBackendIdle) })
	defer idle.Stop()

	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chosen.url, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	if backendAPIKey != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+backendAPIKey)
	}

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		http.Error(w, "backend model request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// Lets clients show which model actually answered; the Pi extension reads these.
	w.Header().Set("X-Lemn-Route", chosen.label)
	if chosen.model != "" {
		w.Header().Set("X-Lemn-Model", chosen.model)
	}
	if chosen.prob >= 0 {
		w.Header().Set("X-Lemn-Probability", strconv.FormatFloat(chosen.prob, 'f', 2, 64))
	}
	w.WriteHeader(resp.StatusCode)

	// Flush each SSE line and record whether the upstream completed its stream.
	flusher, canFlush := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	var bytesWritten int64
	sawFinishReason := false
	sawDone := false
	startedAt := time.Now()
	var pendingFinish []byte
	var budget struct {
		MaxTokens           int `json:"max_tokens"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(bodyBytes, &budget)
	if budget.MaxCompletionTokens > 0 {
		budget.MaxTokens = budget.MaxCompletionTokens
	}
	completionTokens := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			idle.Reset(backendIdleTimeout)
			if isSSE {
				var usage struct {
					Usage struct {
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
				}
				data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
				if json.Unmarshal(data, &usage) == nil && usage.Usage.CompletionTokens > 0 {
					completionTokens = usage.Usage.CompletionTokens
				}
				if bytes.Equal(data, []byte("[DONE]")) && len(pendingFinish) > 0 {
					if _, err := w.Write(terminalSSEEvent(pendingFinish, completionTokens, budget.MaxTokens)); err != nil {
						return
					}
					pendingFinish = nil
				}
				var terminal struct {
					Choices []struct {
						FinishReason string `json:"finish_reason"`
					} `json:"choices"`
				}
				if json.Unmarshal(data, &terminal) == nil {
					for _, choice := range terminal.Choices {
						if choice.FinishReason == "stop" {
							pendingFinish = append([]byte(nil), line...)
							sawFinishReason = true
							line = nil
							break
						}
					}
				}
			}
			if _, writeErr := w.Write(line); writeErr != nil {
				return
			}
			bytesWritten += int64(len(line))
			if isSSE {
				data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
				if bytes.Equal(data, []byte("[DONE]")) {
					sawDone = true
				} else {
					var chunk struct {
						Choices []struct {
							FinishReason *string `json:"finish_reason"`
						} `json:"choices"`
					}
					if json.Unmarshal(data, &chunk) == nil {
						for _, choice := range chunk.Choices {
							if choice.FinishReason != nil && *choice.FinishReason != "" {
								sawFinishReason = true
							}
						}
					}
				}
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if len(pendingFinish) > 0 {
				_, _ = w.Write(terminalSSEEvent(pendingFinish, completionTokens, budget.MaxTokens))
				if canFlush {
					flusher.Flush()
				}
			}
			if cause := context.Cause(ctx); cause != nil {
				readErr = cause
			}
			if isSSE && !sawFinishReason {
				log.Printf("[Router] incomplete SSE from %s model=%q status=%d bytes=%d finish_reason=false done=%t duration=%s read_error=%v",
					chosen.label, chosen.model, resp.StatusCode, bytesWritten, sawDone, time.Since(startedAt).Round(time.Millisecond), readErr)
			} else if !errors.Is(readErr, io.EOF) {
				log.Printf("[Router] upstream stream read failed from %s model=%q status=%d bytes=%d finish_reason=%t done=%t duration=%s error=%v",
					chosen.label, chosen.model, resp.StatusCode, bytesWritten, sawFinishReason, sawDone, time.Since(startedAt).Round(time.Millisecond), readErr)
			}
			return
		}
	}
}

func terminalSSEEvent(line []byte, used, budget int) []byte {
	data := bytes.TrimRight(markBudgetFinish(line, used, budget), "\r\n")
	return append(append([]byte(nil), data...), '\n', '\n')
}

func markBudgetFinish(line []byte, used, budget int) []byte {
	if budget <= 0 || used < budget {
		return line
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	var chunk map[string]json.RawMessage
	if json.Unmarshal(data, &chunk) != nil {
		return line
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(chunk["choices"], &choices) != nil {
		return line
	}
	for _, choice := range choices {
		if string(choice["finish_reason"]) == `"stop"` {
			choice["finish_reason"] = json.RawMessage(`"length"`)
		}
	}
	encoded, err := json.Marshal(choices)
	if err != nil {
		return line
	}
	chunk["choices"] = encoded
	encoded, err = json.Marshal(chunk)
	if err != nil {
		return line
	}
	return append(append([]byte("data: "), encoded...), '\n')
}

// withModel replaces the model field while preserving every other key the
// client sent, including ones this proxy does not model.
func withModel(body []byte, model string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	raw["model"] = encoded
	return json.Marshal(raw)
}

func chooseBackend(userMessage string, estTokens int) backend {
	fast := backend{url: fastModelURL, model: fastModelName, label: "fast", prob: -1}
	heavy := backend{url: heavyModelURL, model: heavyModelName, label: "heavy", prob: -1}

	if estTokens > longContextTokens {
		log.Printf("[Router] long context (~%d tokens > %d) -> heavy model", estTokens, longContextTokens)
		return heavy
	}

	if userMessage == "" {
		return heavy
	}

	ctx, cancel := context.WithTimeout(context.Background(), classifyTimeout)
	defer cancel()

	reqBody, _ := json.Marshal(classifyRequest{Query: userMessage})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, layaEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[Router] failed to build classify request: %v — failing open to heavy model", err)
		return heavy
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sharedSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Router] Laya classify call failed: %v — failing open to heavy model", err)
		return heavy
	}
	defer resp.Body.Close()

	var result classifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[Router] failed to decode Laya response: %v — failing open to heavy model", err)
		return heavy
	}

	if result.RequiresReasoning {
		log.Printf("[Router] requires_reasoning=true (p=%.2f) -> heavy model", result.Probability)
		heavy.prob = result.Probability
		return heavy
	}
	log.Printf("[Router] requires_reasoning=false (p=%.2f) -> fast model", result.Probability)
	fast.prob = result.Probability
	return fast
}
