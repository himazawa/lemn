package lemn

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const maxEvidenceSourceBytes = 2 << 20
const maxEvidenceQuoteBytes = 16 << 10

type verifiedEvidence struct {
	SourceURL      string
	Quote          string
	ObservedAt     time.Time
	SourceSHA256   string
	QuoteSHA256    string
	ContentType    string
	LastModifiedAt *time.Time
}

type evidenceVerifier func(context.Context, string, string) (verifiedEvidence, error)

func verifyEvidenceSource(ctx context.Context, sourceURL, quote string) (verifiedEvidence, error) {
	allowedHosts := configuredEvidenceHosts()
	if len(allowedHosts) == 0 {
		return verifiedEvidence{}, fmt.Errorf("no evidence hosts are approved; set LEMN_REVALIDATION_ALLOWED_HOSTS or use explicit user confirmation")
	}
	client := newEvidenceHTTPClient(allowedHosts)
	return verifyEvidenceSourceWithClient(ctx, sourceURL, quote, allowedHosts, client)
}

func configuredEvidenceHosts() map[string]struct{} {
	hosts := make(map[string]struct{})
	for _, value := range strings.Split(getenv("LEMN_REVALIDATION_ALLOWED_HOSTS", ""), ",") {
		host := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(value, ".")))
		if host != "" {
			hosts[host] = struct{}{}
		}
	}
	return hosts
}

func newEvidenceHTTPClient(allowedHosts map[string]struct{}) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		IdleConnTimeout:       10 * time.Second,
		DisableKeepAlives:     true,
		DialContext:           evidenceDialContext,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("evidence source exceeded the redirect limit")
			}
			if err := validateEvidenceURL(request.URL, allowedHosts); err != nil {
				return fmt.Errorf("unsafe evidence redirect: %w", err)
			}
			return nil
		},
	}
}

func evidenceDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid evidence address")
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil, fmt.Errorf("IP literal evidence hosts are not allowed")
	}
	resolver := net.Resolver{}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("could not resolve evidence host")
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("evidence host has no addresses")
	}
	var lastErr error
	for _, address := range addresses {
		if !isPublicEvidenceIP(address.IP) {
			return nil, fmt.Errorf("evidence host resolves to a non-public address")
		}
		conn, dialErr := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("could not connect to evidence host: %w", lastErr)
}

func isPublicEvidenceIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	}
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, cidr := range []string{
		"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32",
	} {
		_, specialRange, _ := net.ParseCIDR(cidr)
		if specialRange.Contains(ip) {
			return false
		}
	}
	return true
}

func validateEvidenceURL(source *url.URL, allowedHosts map[string]struct{}) error {
	if source == nil || source.Scheme != "https" || source.Hostname() == "" {
		return fmt.Errorf("evidence source must be an HTTPS URL")
	}
	if source.User != nil {
		return fmt.Errorf("evidence source URL must not contain credentials")
	}
	if source.RawQuery != "" {
		return fmt.Errorf("evidence source URL must not contain query parameters; they may expose secrets in audit history")
	}
	if len(source.String()) > 4096 {
		return fmt.Errorf("evidence source URL exceeds the 4096-byte limit")
	}
	if source.Port() != "" && source.Port() != "443" {
		return fmt.Errorf("evidence source must use HTTPS port 443")
	}
	host := strings.ToLower(strings.TrimSuffix(source.Hostname(), "."))
	if net.ParseIP(host) != nil {
		return fmt.Errorf("IP literal evidence hosts are not allowed")
	}
	if _, allowed := allowedHosts[host]; !allowed {
		return fmt.Errorf("evidence host %q is not in LEMN_REVALIDATION_ALLOWED_HOSTS", host)
	}
	return nil
}

func verifyEvidenceSourceWithClient(ctx context.Context, sourceURL, quote string, allowedHosts map[string]struct{}, client *http.Client) (verifiedEvidence, error) {
	parsed, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return verifiedEvidence{}, fmt.Errorf("invalid evidence source URL: %w", err)
	}
	if err := validateEvidenceURL(parsed, allowedHosts); err != nil {
		return verifiedEvidence{}, err
	}
	if strings.TrimSpace(quote) == "" {
		return verifiedEvidence{}, fmt.Errorf("evidence quote cannot be empty")
	}
	if len(quote) > maxEvidenceQuoteBytes {
		return verifiedEvidence{}, fmt.Errorf("evidence quote exceeds the %d-byte limit", maxEvidenceQuoteBytes)
	}
	if client == nil {
		return verifiedEvidence{}, fmt.Errorf("evidence source HTTP client is not configured")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return verifiedEvidence{}, fmt.Errorf("could not build evidence request: %w", err)
	}
	request.Header.Set("Accept", "text/html, text/plain, application/json, application/xml, text/xml")
	request.Header.Set("User-Agent", "LEMN-evidence-verifier/1.0")
	response, err := client.Do(request)
	if err != nil {
		return verifiedEvidence{}, fmt.Errorf("could not fetch evidence source: %w", err)
	}
	defer response.Body.Close()
	finalURL := parsed
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL
	}
	if err := validateEvidenceURL(finalURL, allowedHosts); err != nil {
		return verifiedEvidence{}, fmt.Errorf("unsafe final evidence URL: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return verifiedEvidence{}, fmt.Errorf("evidence source returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxEvidenceSourceBytes+1))
	if err != nil {
		return verifiedEvidence{}, fmt.Errorf("could not read evidence source: %w", err)
	}
	if len(body) > maxEvidenceSourceBytes {
		return verifiedEvidence{}, fmt.Errorf("evidence source exceeds the %d-byte limit", maxEvidenceSourceBytes)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	var searchable string
	switch contentType {
	case "text/html", "application/xhtml+xml":
		searchable, err = visibleHTMLText(body)
	case "application/json":
		searchable, err = jsonEvidenceText(body)
	case "application/xml", "text/xml":
		searchable, err = xmlEvidenceText(body)
	case "text/plain":
		searchable = string(body)
	default:
		return verifiedEvidence{}, fmt.Errorf("unsupported evidence source content type %q", contentType)
	}
	if err != nil {
		return verifiedEvidence{}, fmt.Errorf("could not extract evidence text: %w", err)
	}
	if !containsNormalized(searchable, quote) {
		return verifiedEvidence{}, fmt.Errorf("the cited evidence passage was not found in the fetched source")
	}

	now := time.Now().UTC()
	observedAt := now
	var lastModified *time.Time
	if rawAge := strings.TrimSpace(response.Header.Get("Age")); rawAge != "" {
		ageSeconds, parseErr := strconv.ParseUint(rawAge, 10, 64)
		if parseErr != nil || ageSeconds > uint64((100*365*24*time.Hour)/time.Second) {
			return verifiedEvidence{}, fmt.Errorf("evidence source returned an invalid or excessive Age header")
		}
		cacheObservedAt := now.Add(-time.Duration(ageSeconds) * time.Second)
		if cacheObservedAt.Before(observedAt) {
			observedAt = cacheObservedAt
		}
	}
	if raw := strings.TrimSpace(response.Header.Get("Last-Modified")); raw != "" {
		parsedTime, parseErr := http.ParseTime(raw)
		if parseErr != nil {
			return verifiedEvidence{}, fmt.Errorf("evidence source returned an invalid Last-Modified timestamp")
		}
		parsedTime = parsedTime.UTC()
		if parsedTime.After(now) {
			return verifiedEvidence{}, fmt.Errorf("evidence source Last-Modified timestamp is in the future")
		}
		if parsedTime.Before(observedAt) {
			observedAt = parsedTime
		}
		lastModified = &parsedTime
	}
	sourceDigest := sha256.Sum256(body)
	quoteDigest := sha256.Sum256([]byte(strings.TrimSpace(quote)))
	return verifiedEvidence{
		SourceURL:      finalURL.String(),
		Quote:          strings.TrimSpace(quote),
		ObservedAt:     observedAt,
		SourceSHA256:   hex.EncodeToString(sourceDigest[:]),
		QuoteSHA256:    hex.EncodeToString(quoteDigest[:]),
		ContentType:    contentType,
		LastModifiedAt: lastModified,
	}, nil
}

func visibleHTMLText(body []byte) (string, error) {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	var text strings.Builder
	var visit func(*html.Node, bool)
	visit = func(node *html.Node, hidden bool) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "head", "title", "script", "style", "noscript", "template":
				hidden = true
			}
			for _, attribute := range node.Attr {
				if attribute.Key == "hidden" || (strings.EqualFold(attribute.Key, "aria-hidden") && strings.EqualFold(attribute.Val, "true")) {
					hidden = true
				}
				if strings.EqualFold(attribute.Key, "style") {
					style := strings.ReplaceAll(strings.ToLower(attribute.Val), " ", "")
					if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
						hidden = true
					}
				}
			}
		}
		if node.Type == html.TextNode && !hidden {
			text.WriteByte(' ')
			text.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, hidden)
		}
	}
	visit(document, false)
	return text.String(), nil
}

func jsonEvidenceText(body []byte) (string, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", err
	}
	var text strings.Builder
	var visit func(any)
	visit = func(item any) {
		switch typed := item.(type) {
		case string:
			text.WriteByte(' ')
			text.WriteString(typed)
		case []any:
			for _, child := range typed {
				visit(child)
			}
		case map[string]any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return text.String(), nil
}

func xmlEvidenceText(body []byte) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return text.String(), nil
		}
		if err != nil {
			return "", err
		}
		if content, ok := token.(xml.CharData); ok {
			text.WriteByte(' ')
			text.Write(content)
		}
	}
}

func containsNormalized(document, quote string) bool {
	normalize := func(value string) string {
		return strings.ToLower(strings.Join(strings.Fields(value), " "))
	}
	return strings.Contains(normalize(document), normalize(quote))
}
