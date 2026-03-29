package apiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client wraps the Blockyard server API.
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
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
	return c.HTTPClient.Do(req)
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
