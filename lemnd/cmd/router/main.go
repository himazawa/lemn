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
	"bytes"
	"context"
	"encoding/json"
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
	layaEndpoint    = getenv("LEMN_LAYA_URL", "http://localhost:8002/classify")
	fastModelURL    = getenv("LEMN_FAST_MODEL_URL", "http://localhost:8010/v1/chat/completions")
	heavyModelURL   = getenv("LEMN_HEAVY_MODEL_URL", "http://localhost:8011/v1/chat/completions")
	fastModelName   = os.Getenv("LEMN_FAST_MODEL_NAME")
	heavyModelName  = os.Getenv("LEMN_HEAVY_MODEL_NAME")
	backendAPIKey   = os.Getenv("LEMN_BACKEND_API_KEY")
	routerBind      = getenv("LEMN_ROUTER_BIND", "127.0.0.1:8090")
	// Laya is single-process and CPU-bound, so a classify can queue behind the
	// daemon's memory-worthiness call. Too low here silently routes everything
	// to the heavy model via the fail-open path.
	classifyTimeout = getenvDuration("LEMN_CLASSIFY_TIMEOUT", 5*time.Second)
	backendTimeout  = 5 * time.Minute // generous — this covers full generation, not just connect
	sharedSecret    string
)

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
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserMsg = contentText(req.Messages[i].Content)
			break
		}
	}

	chosen := chooseBackend(lastUserMsg)
	if chosen.model != "" {
		rewritten, err := withModel(bodyBytes, chosen.model)
		if err != nil {
			log.Printf("[Router] failed to rewrite model field: %v — forwarding original body", err)
		} else {
			bodyBytes = rewritten
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()

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

	// Flush after every write so streamed (SSE) completions reach Pi
	// incrementally instead of being buffered until the backend finishes.
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
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

func chooseBackend(userMessage string) backend {
	fast := backend{url: fastModelURL, model: fastModelName, label: "fast", prob: -1}
	heavy := backend{url: heavyModelURL, model: heavyModelName, label: "heavy", prob: -1}

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
