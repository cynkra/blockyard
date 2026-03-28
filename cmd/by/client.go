package main

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

// client wraps the Blockyard server API.
type client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func newClient(baseURL, token string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// newStreamingClient returns a client with no timeout for streaming endpoints.
func newStreamingClient(baseURL, token string) *client {
	return &client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{},
	}
}

// apiError is returned when the server responds with an error status.
type apiError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Code)
}

func (c *client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	u := c.baseURL + path
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.httpClient.Do(req)
}

func (c *client) get(path string) (*http.Response, error) {
	return c.do(http.MethodGet, path, nil, "")
}

func (c *client) postJSON(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(http.MethodPost, path, bytes.NewReader(data), "application/json")
}

func (c *client) patchJSON(path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(http.MethodPatch, path, bytes.NewReader(data), "application/json")
}

func (c *client) delete(path string) (*http.Response, error) {
	return c.do(http.MethodDelete, path, nil, "")
}

func (c *client) post(path string, body io.Reader, contentType string) (*http.Response, error) {
	return c.do(http.MethodPost, path, body, contentType)
}

// checkResponse reads the response and returns an error if status is not in 2xx.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	var apiErr apiError
	apiErr.Status = resp.StatusCode
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &apiErr)
	if apiErr.Message == "" {
		apiErr.Message = string(data)
	}
	return &apiErr
}

// decodeJSON reads the response body and decodes it into v.
func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// readBodyRaw reads the response body and returns it as raw bytes.
func readBodyRaw(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// buildQuery builds a URL path with query parameters.
func buildQuery(path string, params map[string]string) string {
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
