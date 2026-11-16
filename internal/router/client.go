package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const maxBody = 8 << 20
const maxPages = 100

var errBackend = errors.New("Bookwarehouse response unavailable or invalid")

type client struct {
	base      url.URL
	key       string
	http      *http.Client
	transport *http.Transport
}

func validBase(raw string, allowHTTP bool) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 4096 || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) || strings.HasSuffix(u.Host, ":") {
		return nil, false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, false
		}
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(u.Path, "\\") {
		return nil, false
	}
	for _, p := range strings.Split(u.Path, "/") {
		if p == "." || p == ".." {
			return nil, false
		}
	}
	for _, r := range u.Host + u.Path {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, false
		}
	}
	return u, true
}
func newClient(base, key string, allowHTTP bool) (*client, error) {
	u, ok := validBase(base, allowHTTP)
	if !ok || len(key) == 0 || len(key) > 8192 {
		return nil, errBackend
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return nil, errBackend
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1"
	u.RawPath = ""
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 1 << 20, DisableCompression: true}
	return &client{base: *u, key: key, transport: transport, http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *client) close() { c.transport.CloseIdleConnections() }

// No caller receives transport errors or backend error text: either may contain credentials.
func (c *client) json(ctx context.Context, method, path string, query url.Values, body any, out any) (int, error) {
	u := c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, errBackend
		}
	}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(data))
	if err != nil {
		return 0, errBackend
	}
	r.Header.Set("X-API-Key", c.key)
	r.Header.Set("Accept", "application/json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(r)
	if err != nil {
		return 0, errBackend
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		return response.StatusCode, errBackend
	}
	if response.StatusCode >= 300 && response.StatusCode != 409 {
		return response.StatusCode, errBackend
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return response.StatusCode, errBackend
	}
	return response.StatusCode, nil
}

type backendRow struct {
	ID             string `json:"id"`
	ISBN13         string `json:"isbn13"`
	ISBN10         string `json:"isbn10"`
	Status         string `json:"status"`
	Format         string `json:"format"`
	SelectedFormat string `json:"selected_format"`
	FileFormat     string `json:"file_format"`
}

func (r backendRow) matches(isbn, format string, book bool) bool {
	actual := r.SelectedFormat
	if actual == "" {
		actual = r.Format
	}
	if book {
		actual = r.FileFormat
	}
	return (r.ISBN13 == isbn || isbn10To13(r.ISBN10) == isbn) && actual == format
}
func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

type monitoringPage struct {
	Rows  []backendRow `json:"requests"`
	Total *int         `json:"total"`
	Page  *int         `json:"page"`
	Limit *int         `json:"limit"`
}

func (c *client) monitoring(ctx context.Context, page int) (monitoringPage, error) {
	var p monitoringPage
	code, err := c.json(ctx, http.MethodGet, "/monitoring", url.Values{"page": {strconv.Itoa(page)}, "limit": {"100"}}, nil, &p)
	if err != nil || code != 200 || p.Rows == nil || p.Total == nil || p.Page == nil || p.Limit == nil || *p.Page != page || *p.Limit != 100 || *p.Total < 0 || *p.Total > maxPages*100 || len(p.Rows) > 100 {
		return p, errBackend
	}
	expected := min(100, max(0, *p.Total-(page-1)*100))
	if len(p.Rows) != expected {
		return p, errBackend
	}
	return p, nil
}
func (c *client) preflight(ctx context.Context, isbn, format string) (backendRow, string, error) {
	var match backendRow
	total := -1
	seen := map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		p, err := c.monitoring(ctx, page)
		if err != nil {
			return backendRow{}, "", err
		}
		if total >= 0 && total != *p.Total {
			return backendRow{}, "", errBackend
		}
		total = *p.Total
		for _, row := range p.Rows {
			if !validID(row.ID) || seen[row.ID] {
				return backendRow{}, "", errBackend
			}
			seen[row.ID] = true
			if row.matches(isbn, format, false) {
				if match.ID != "" && match.ID != row.ID {
					return backendRow{}, "", errBackend
				}
				match = row
			}
		}
		if page*100 >= total {
			break
		}
	}
	// Prefer actual book presence over monitoring state; provider presence is never host fulfillment.
	total = -1
	seen = map[string]bool{}
	for page := 1; page <= maxPages; page++ {
		var p struct {
			Books      []backendRow `json:"books"`
			Pagination *struct {
				Page  int `json:"page"`
				Limit int `json:"limit"`
				Total int `json:"total_items"`
				Pages int `json:"total_pages"`
			} `json:"pagination"`
		}
		code, err := c.json(ctx, http.MethodGet, "/books", url.Values{"page": {strconv.Itoa(page)}, "limit": {"100"}, "format": {format}, "sort": {"updated_at"}, "order": {"asc"}}, nil, &p)
		if err != nil || code != 200 || p.Books == nil || p.Pagination == nil {
			return backendRow{}, "", errBackend
		}
		q := p.Pagination
		pages := q.Total / 100
		if q.Total%100 != 0 {
			pages++
		}
		if q.Page != page || q.Limit != 100 || q.Total < 0 || q.Pages != pages || len(p.Books) != min(100, max(0, q.Total-(page-1)*100)) || (total >= 0 && q.Total != total) {
			return backendRow{}, "", errBackend
		}
		total = q.Total
		for _, row := range p.Books {
			if !validID(row.ID) || seen[row.ID] {
				return backendRow{}, "", errBackend
			}
			seen[row.ID] = true
			if row.matches(isbn, format, true) {
				row.Status = "completed"
				return row, "book", nil
			}
		}
		// Book presence is an optimization, not a required global inventory.
		// On large catalogs, the provider's atomic book_exists response to add
		// supplies presence evidence without scanning hundreds of thousands of
		// books. Monitoring preflight still completes before any submission.
		if total > maxPages*100 || page*100 >= total {
			break
		}
	}
	if match.ID != "" {
		return match, "monitoring", nil
	}
	return backendRow{}, "", nil
}
