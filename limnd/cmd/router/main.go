// LIMN Model Router
//
// Point Pi's model base_url at this service (http://localhost:8090/v1)
// and its api_key at LIMN_SHARED_SECRET's value — OpenAI-compatible
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
	"time"

	"limnd/internal/authmw"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	layaEndpoint    = getenv("LIMN_LAYA_URL", "http://localhost:8002/classify")
	fastModelURL    = getenv("LIMN_FAST_MODEL_URL", "http://localhost:8010/v1/chat/completions")
	heavyModelURL   = getenv("LIMN_HEAVY_MODEL_URL", "http://localhost:8011/v1/chat/completions")
	routerBind      = getenv("LIMN_ROUTER_BIND", "127.0.0.1:8090")
	classifyTimeout = 2 * time.Second
	backendTimeout  = 5 * time.Minute // generous — this covers full generation, not just connect
	sharedSecret    string
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	sharedSecret = os.Getenv("LIMN_SHARED_SECRET")
	if sharedSecret == "" {
		log.Fatal("LIMN_SHARED_SECRET is not set — refusing to start unauthenticated. " +
			"Set it to the same value configured on the daemon and Laya service, " +
			"and set it as Pi's model api_key.")
	}

	http.HandleFunc("/v1/chat/completions", authmw.Require(sharedSecret, handleChatCompletions))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	log.Printf("LIMN model router listening on %s (fast=%s, heavy=%s, laya=%s)",
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
			lastUserMsg = req.Messages[i].Content
			break
		}
	}

	backend := chooseBackend(lastUserMsg)

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()

	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, backend, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")

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

func chooseBackend(userMessage string) string {
	if userMessage == "" {
		return heavyModelURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), classifyTimeout)
	defer cancel()

	reqBody, _ := json.Marshal(classifyRequest{Query: userMessage})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, layaEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[Router] failed to build classify request: %v — failing open to heavy model", err)
		return heavyModelURL
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sharedSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Router] Laya classify call failed: %v — failing open to heavy model", err)
		return heavyModelURL
	}
	defer resp.Body.Close()

	var result classifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[Router] failed to decode Laya response: %v — failing open to heavy model", err)
		return heavyModelURL
	}

	if result.RequiresReasoning {
		log.Printf("[Router] requires_reasoning=true (p=%.2f) -> heavy model", result.Probability)
		return heavyModelURL
	}
	log.Printf("[Router] requires_reasoning=false (p=%.2f) -> fast model", result.Probability)
	return fastModelURL
}
