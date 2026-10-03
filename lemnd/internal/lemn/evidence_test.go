package lemn

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestVerifyEvidenceSourceFetchesAndChecksVisibleHTMLQuote(t *testing.T) {
	body := `<html><head><title>API</title><script>Ignore this hidden text.</script></head><body><p>The API uses <strong>HTTP/2</strong> for requests.</p></body></html>`
	lastModified := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("request method = %q, want GET", request.Method)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("evidence fetch unexpectedly forwarded authorization")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Last-Modified": []string{lastModified.Format(http.TimeFormat)},
				"Age":           []string{"60"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: request,
		}, nil
	})}
	quote := "The API uses HTTP/2 for requests."
	result, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", quote, map[string]struct{}{"docs.example.test": {}}, client)
	if err != nil {
		t.Fatalf("verifyEvidenceSourceWithClient() error = %v", err)
	}
	if result.Quote != quote || result.SourceURL != "https://docs.example.test/api" {
		t.Fatalf("verified source metadata = %+v", result)
	}
	if result.ObservedAt.After(lastModified.Add(time.Second)) {
		t.Fatalf("observed at %s, expected conservative Last-Modified no later than %s", result.ObservedAt, lastModified)
	}
	if result.LastModifiedAt == nil || !result.LastModifiedAt.Equal(lastModified) {
		t.Fatalf("LastModifiedAt = %v, want %s", result.LastModifiedAt, lastModified)
	}
	if result.SourceSHA256 == "" || result.QuoteSHA256 == "" || result.ContentType != "text/html" {
		t.Fatalf("verified source omitted audit metadata: %+v", result)
	}
}

func TestVerifyEvidenceSourceRejectsMissingQuoteAndOversizedBody(t *testing.T) {
	allowed := map[string]struct{}{"docs.example.test": {}}
	newClient := func(body string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		})}
	}
	if _, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", "not present", allowed, newClient("different content")); err == nil || !strings.Contains(err.Error(), "passage was not found") {
		t.Fatalf("missing-quote verification error = %v, want passage-not-found", err)
	}
	if _, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", "anything", allowed, newClient(strings.Repeat("x", maxEvidenceSourceBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("oversize verification error = %v, want size-limit error", err)
	}
	if _, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", strings.Repeat("q", maxEvidenceQuoteBytes+1), allowed, newClient("unused")); err == nil || !strings.Contains(err.Error(), "quote exceeds") {
		t.Fatalf("oversize quote error = %v, want quote-size error", err)
	}
}

func TestVisibleHTMLTextExcludesMetadataAndHiddenNodes(t *testing.T) {
	body := `<html><head><title>Secret phrase in title</title></head><body><p>Visible evidence.</p><div hidden>Hidden attribute phrase.</div><div aria-hidden="true">ARIA hidden phrase.</div><div style="display: none">CSS hidden phrase.</div><script>Script phrase.</script><template>Template phrase.</template></body></html>`
	text, err := visibleHTMLText([]byte(body))
	if err != nil {
		t.Fatalf("visibleHTMLText() error = %v", err)
	}
	if !containsNormalized(text, "Visible evidence.") {
		t.Fatalf("visible text %q omitted visible body content", text)
	}
	for _, hidden := range []string{"Secret phrase in title", "Hidden attribute phrase", "ARIA hidden phrase", "CSS hidden phrase", "Script phrase", "Template phrase"} {
		if containsNormalized(text, hidden) {
			t.Errorf("visibleHTMLText() included hidden content %q in %q", hidden, text)
		}
	}
}

func TestVerifyEvidenceSourceExtractsXMLAndUsesCacheAge(t *testing.T) {
	body := `<policy><summary>Production retries failed requests up to three times.</summary></policy>`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/xml"},
				"Age":          []string{"7200"},
			},
			Body:    io.NopCloser(strings.NewReader(body)),
			Request: request,
		}, nil
	})}
	quote := "Production retries failed requests up to three times."
	result, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/policy", quote, map[string]struct{}{"docs.example.test": {}}, client)
	if err != nil {
		t.Fatalf("verifyEvidenceSourceWithClient() error = %v", err)
	}
	if age := time.Since(result.ObservedAt); age < 2*time.Hour || age > 2*time.Hour+time.Second {
		t.Fatalf("observed source age = %s, want approximately 2h from Age header", age)
	}
}

func TestVerifyEvidenceSourceRejectsBadStatusAndUnknownContent(t *testing.T) {
	allowed := map[string]struct{}{"docs.example.test": {}}
	tests := []struct {
		name        string
		status      int
		contentType string
		wantErr     string
	}{
		{name: "not found", status: http.StatusNotFound, contentType: "text/plain", wantErr: "HTTP 404"},
		{name: "binary content", status: http.StatusOK, contentType: "application/pdf", wantErr: "unsupported evidence source content type"},
		{name: "missing content type", status: http.StatusOK, wantErr: "unsupported evidence source content type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				header := make(http.Header)
				if test.contentType != "" {
					header.Set("Content-Type", test.contentType)
				}
				return &http.Response{
					StatusCode: test.status,
					Header:     header,
					Body:       io.NopCloser(strings.NewReader("The API uses HTTP/2.")),
					Request:    request,
				}, nil
			})}
			_, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", "The API uses HTTP/2.", allowed, client)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("verifyEvidenceSourceWithClient() error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestVerifyEvidenceSourceRejectsInvalidCacheDates(t *testing.T) {
	allowed := map[string]struct{}{"docs.example.test": {}}
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{name: "invalid age", header: http.Header{"Age": []string{"many"}}, want: "invalid or excessive Age"},
		{name: "future last modified", header: http.Header{"Last-Modified": []string{time.Now().UTC().Add(time.Hour).Format(http.TimeFormat)}}, want: "Last-Modified timestamp is in the future"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				header := test.header.Clone()
				header.Set("Content-Type", "text/plain")
				return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("The API uses HTTP/2.")), Request: request}, nil
			})}
			_, err := verifyEvidenceSourceWithClient(context.Background(), "https://docs.example.test/api", "The API uses HTTP/2.", allowed, client)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verifyEvidenceSourceWithClient() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestXMLQuoteExtractionIgnoresMarkup(t *testing.T) {
	text, err := xmlEvidenceText([]byte(`<root><entry>Production retries <count>three</count> times.</entry></root>`))
	if err != nil {
		t.Fatalf("xmlEvidenceText() error = %v", err)
	}
	if !containsNormalized(text, "Production retries three times.") {
		t.Fatalf("XML text %q did not contain the visible quote", text)
	}
}

func TestValidateEvidenceURLAndRedirectAllowlist(t *testing.T) {
	allowed := map[string]struct{}{"docs.example.test": {}}
	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "plain HTTP", url: "http://docs.example.test/api", want: "HTTPS"},
		{name: "embedded credentials", url: "https://user:secret@docs.example.test/api", want: "credentials"},
		{name: "query token", url: "https://docs.example.test/api?token=secret", want: "must not contain query parameters"},
		{name: "host not allowlisted", url: "https://other.example.test/api", want: "not in LEMN_REVALIDATION_ALLOWED_HOSTS"},
		{name: "IP literal", url: "https://8.8.8.8/api", want: "IP literal"},
		{name: "nonstandard port", url: "https://docs.example.test:8443/api", want: "port 443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := url.Parse(test.url)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateEvidenceURL(parsed, allowed); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateEvidenceURL() error = %v, want text %q", err, test.want)
			}
		})
	}

	client := newEvidenceHTTPClient(allowed)
	request, _ := http.NewRequest(http.MethodGet, "https://docs.example.test/api", nil)
	unsafeRedirect, _ := http.NewRequest(http.MethodGet, "https://127.0.0.1/private", nil)
	if err := client.CheckRedirect(unsafeRedirect, []*http.Request{request}); err == nil {
		t.Fatal("redirect to an unapproved IP host was allowed")
	}
}

func TestConfiguredEvidenceHosts(t *testing.T) {
	t.Setenv("LEMN_REVALIDATION_ALLOWED_HOSTS", " docs.example.test,git.example.test.,docs.example.test ")
	hosts := configuredEvidenceHosts()
	if len(hosts) != 2 {
		t.Fatalf("configuredEvidenceHosts() = %v, want two exact normalized hosts", hosts)
	}
	for _, host := range []string{"docs.example.test", "git.example.test"} {
		if _, exists := hosts[host]; !exists {
			t.Errorf("configured host %q missing from %v", host, hosts)
		}
	}
}

func TestIsPublicEvidenceIPBlocksPrivateAndMetadataRanges(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "127.0.0.1"},
		{value: "10.1.2.3"},
		{value: "192.168.1.1"},
		{value: "169.254.169.254"},
		{value: "100.64.0.1"},
		{value: "198.18.0.1"},
		{value: "2001:db8::1"},
		{value: "::ffff:10.1.2.3"},
		{value: "::1"},
		{value: "fd00::1"},
		{value: "8.8.8.8", want: true},
		{value: "2606:4700:4700::1111", want: true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			ip := net.ParseIP(test.value)
			if got := isPublicEvidenceIP(ip); got != test.want {
				t.Fatalf("isPublicEvidenceIP(%s) = %v, want %v (%s)", test.value, got, test.want, fmt.Sprint(ip))
			}
		})
	}
}
