package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client wraps the Blockyard server API.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client

	// Version is the CLI version. When set, the client checks the
	// X-Blockyard-Version response header on the first request and
	// warns on major-version mismatch.
	Version string

	// Stderr is the writer for version-mismatch warnings.
	// Defaults to os.Stderr when nil.
	Stderr io.Writer

	checkVersion sync.Once
}

// New creates an API client with a 30-second timeout.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewStreaming returns a client with no timeout for streaming endpoints.
func NewStreaming(baseURL, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{},
	}
}

// APIError is returned when the server responds with an error status.
type APIError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Code)
}

func (c *Client) Do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	u := c.BaseURL + path
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	c.warnVersionMismatch(resp)
	return resp, nil
}

func (c *Client) Get(path string) (*http.Response, error) {
	return c.Do(http.MethodGet, path, nil, "")
}

func (c *Client) PostJSON(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.Do(http.MethodPost, path, bytes.NewReader(data), "application/json")
}

func (c *Client) PatchJSON(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.Do(http.MethodPatch, path, bytes.NewReader(data), "application/json")
}

func (c *Client) Delete(path string) (*http.Response, error) {
	return c.Do(http.MethodDelete, path, nil, "")
}

func (c *Client) Post(path string, body io.Reader, contentType string) (*http.Response, error) {
	return c.Do(http.MethodPost, path, body, contentType)
}

// CheckResponse reads the response and returns an error if status is not in 2xx.
func CheckResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	var apiErr APIError
	apiErr.Status = resp.StatusCode
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &apiErr)
	if apiErr.Message == "" {
		apiErr.Message = string(data)
	}
	return &apiErr
}

// DecodeJSON reads the response body and decodes it into v.
func DecodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ReadBodyRaw reads the response body and returns it as raw bytes.
func ReadBodyRaw(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// warnVersionMismatch prints a one-time warning to stderr if the server's
// major version differs from the client's.
func (c *Client) warnVersionMismatch(resp *http.Response) {
	if c.Version == "" {
		return
	}
	c.checkVersion.Do(func() {
		sv := resp.Header.Get("X-Blockyard-Version")
		if sv == "" {
			return
		}
		cm := parseMajor(c.Version)
		sm := parseMajor(sv)
		if cm < 0 || sm < 0 {
			return // non-semver (e.g. "dev"), skip check
		}
		if cm != sm {
			w := c.Stderr
			if w == nil {
				w = os.Stderr
			}
			fmt.Fprintf(w, "Warning: by %s may be incompatible with server %s (major version mismatch)\n",
				c.Version, sv)
		}
	})
}

// parseMajor extracts the major version number from a semver-like string.
// Returns -1 for non-numeric versions like "dev".
func parseMajor(v string) int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

// BuildQuery builds a URL path with query parameters.
func BuildQuery(path string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
