package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const isbn = "9780547928227"

func conn(t *testing.T, base string, extra map[string]any) *pb.RouterConnection {
	t.Helper()
	config := map[string]any{"allow_http": true}
	for k, v := range extra {
		config[k] = v
	}
	s, err := structpb.NewStruct(config)
	if err != nil {
		t.Fatal(err)
	}
	return &pb.RouterConnection{Id: "test", BaseUrl: base, ApiKey: "synthetic-secret", Config: s}
}
func req(c *pb.RouterConnection) *pb.FulfillRequest {
	return &pb.FulfillRequest{CapabilityId: "bookwarehouse", Request: &pb.RequestDescriptor{MediaType: "book", Title: "Synthetic Book", ExternalIds: map[string]string{"isbn": isbn, "bloem_request_id": "stable-host-request"}}, Connections: []*pb.RouterConnection{c}}
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func emptyBooks(w http.ResponseWriter) {
	jsonResponse(w, map[string]any{"books": []any{}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 0, "total_pages": 0}})
}
func emptyMonitoring(w http.ResponseWriter) {
	jsonResponse(w, map[string]any{"requests": []any{}, "page": 1, "limit": 100, "total": 0})
}

func TestSubmitAndDurableStatus(t *testing.T) {
	var posts atomic.Int32
	completed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "synthetic-secret" {
			t.Error("missing auth")
		}
		switch r.URL.Path {
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring":
			if posts.Load() == 0 {
				emptyMonitoring(w)
			} else {
				jsonResponse(w, map[string]any{"requests": []any{map[string]any{"id": "monitor-1", "isbn13": isbn, "status": "searching", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
			}
		case "/api/v1/monitoring/add":
			posts.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["isbn"] != isbn || body["preferred_format"] != "epub" || len(body) != 2 {
				t.Errorf("body=%v", body)
			}
			w.WriteHeader(202)
			jsonResponse(w, map[string]string{"request_id": "monitor-1", "status": "searching"})
		case "/api/v1/monitoring/monitor-1":
			state := "downloading"
			if completed {
				state = "completed"
			}
			jsonResponse(w, map[string]any{"id": "monitor-1", "isbn13": isbn, "status": state, "format": "epub", "book_id": "book-1"})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := conn(t, server.URL, nil)
	router := New()
	out, err := router.Fulfill(context.Background(), req(c))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Targets) != 1 || out.Targets[0].Status != "queued" || out.Targets[0].ExternalId == "" {
		t.Fatalf("target=%v", out)
	}
	again, err := router.Fulfill(context.Background(), req(c))
	if err != nil || posts.Load() != 1 || again.Targets[0].ExternalId != out.Targets[0].ExternalId {
		t.Fatalf("duplicate posts=%d response=%v err=%v", posts.Load(), again, err)
	}
	target := out.Targets[0]
	check := &pb.CheckStatusRequest{CapabilityId: "bookwarehouse", Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{Quality: target.Quality, ConnectionId: target.ConnectionId, ExternalId: target.ExternalId}}}
	for _, state := range []string{"downloading", "completed"} {
		completed = state == "completed"
		got, err := New().CheckStatus(context.Background(), check)
		if err != nil || len(got.Statuses) != 1 || got.Statuses[0].Status != state {
			t.Fatalf("state=%s got=%v err=%v", state, got, err)
		}
		if completed && !strings.Contains(got.Statuses[0].Message, "awaiting") {
			t.Fatal("missing library scan message")
		}
	}
}

func TestPreflightExactISBNAndPresence(t *testing.T) {
	for _, kind := range []string{"monitoring", "book", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/books":
					if kind == "book" {
						jsonResponse(w, map[string]any{"books": []any{map[string]string{"id": "book-1", "isbn13": isbn, "file_format": "epub"}}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 1, "total_pages": 1}})
					} else {
						emptyBooks(w)
					}
				case "/api/v1/monitoring":
					if kind == "monitoring" {
						jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "existing", "isbn13": isbn, "status": "monitored", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
					} else {
						emptyMonitoring(w)
					}
				case "/api/v1/monitoring/add":
					posts++
					w.WriteHeader(409)
					jsonResponse(w, map[string]string{"error": "book_exists", "message": "synthetic-secret"})
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			out, err := New().Fulfill(context.Background(), req(conn(t, server.URL, nil)))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "conflict" {
				want = 1
			}
			if posts != want || len(out.Targets) != 1 {
				t.Fatalf("posts=%d result=%v", posts, out)
			}
			if kind != "monitoring" && out.Targets[0].Status != "completed" {
				t.Fatal(out)
			}
			if strings.Contains(out.String(), "synthetic-secret") {
				t.Fatal("secret exposed")
			}
		})
	}
}

func TestUnknownReceiptNeverSubmits(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	c := conn(t, server.URL, nil)
	receipt := encodeReceipt(receiptData{Kind: "monitoring", ID: "missing", ISBN: isbn, Format: "epub", RequestID: "stable-host-request"})
	out, err := New().CheckStatus(context.Background(), &pb.CheckStatusRequest{CapabilityId: "bookwarehouse", Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: receipt}}})
	if err != nil || len(out.Statuses) != 1 || out.Statuses[0].Status != "failed" || posts != 0 {
		t.Fatalf("out=%v err=%v posts=%d", out, err, posts)
	}
}

func TestConnectionAndHTTPFailures(t *testing.T) {
	for _, mode := range []string{"ok", "denied", "redirect", "large", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			hits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				switch mode {
				case "ok":
					emptyMonitoring(w)
				case "denied":
					w.WriteHeader(401)
					jsonResponse(w, map[string]string{"error": "synthetic-secret"})
				case "redirect":
					http.Redirect(w, r, "/escaped", 302)
				case "large":
					_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
				case "malformed":
					_, _ = w.Write([]byte(`{"requests":null}`))
				}
			}))
			defer server.Close()
			got, err := New().TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: "bookwarehouse", Connection: conn(t, server.URL, nil)})
			if err != nil {
				t.Fatal(err)
			}
			if got.Ok != (mode == "ok") || strings.Contains(got.Message, "synthetic-secret") || strings.Contains(got.Message, server.URL) || hits != 1 {
				t.Fatalf("got=%v hits=%d", got, hits)
			}
		})
	}
}

func TestValidationAndPDFGate(t *testing.T) {
	c := conn(t, "http://localhost", nil)
	r := New()
	input := req(c)
	for _, mutate := range []func(){func() { input.Request.MediaType = "audiobook" }, func() { input.Request.MediaType = "book"; input.Request.ExternalIds["isbn"] = "9780547928228" }, func() { input.Request.ExternalIds["isbn"] = isbn; input.Request.ExternalIds["bloem_request_id"] = "" }} {
		mutate()
		if _, err := r.Fulfill(context.Background(), input); err == nil {
			t.Fatal("invalid request admitted")
		}
	}
	for _, config := range []map[string]any{{"preferred_format": "pdf"}, {"allow_pdf": "true"}, {"preferred_format": "mobi"}} {
		got, err := r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: "bookwarehouse", Connection: conn(t, "http://localhost", config)})
		if err != nil || len(got.FieldErrors) == 0 {
			t.Fatalf("config=%v out=%v err=%v", config, got, err)
		}
	}
	got, err := r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: "bookwarehouse", Connection: conn(t, "http://localhost", map[string]any{"preferred_format": "pdf", "allow_pdf": true})})
	if err != nil || len(got.FieldErrors) != 0 {
		t.Fatalf("valid PDF rejected: %v %v", got, err)
	}
}

func TestAmbiguousSubmissionRecoversWithoutAnotherPOST(t *testing.T) {
	posts := 0
	known := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring":
			if known {
				jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "accepted", "isbn13": isbn, "status": "searching_now", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
			} else {
				emptyMonitoring(w)
			}
		case "/api/v1/monitoring/add":
			posts++
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`not-json synthetic-secret`))
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := conn(t, server.URL, nil)
	out, err := New().Fulfill(context.Background(), req(c))
	if err != nil || len(out.Targets) != 1 || out.Targets[0].ExternalStatus != "unknown" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	target := out.Targets[0]
	check := &pb.CheckStatusRequest{CapabilityId: "bookwarehouse", Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: target.ExternalId}}}
	got, err := New().CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].ExternalStatus != "unknown" || got.Statuses[0].Status != "queued" {
		t.Fatalf("absent out=%v err=%v", got, err)
	}
	known = true
	got, err = New().CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].ExternalStatus != "searching_now" || posts != 1 {
		t.Fatalf("recovered out=%v err=%v posts=%d", got, err, posts)
	}
}

func TestPreflightRefusesInconsistentPagination(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		jsonResponse(w, map[string]any{"requests": []any{}, "page": 1, "limit": 100, "total": 1, "books": []any{}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 1, "total_pages": 1}})
	}))
	defer server.Close()
	if _, err := New().Fulfill(context.Background(), req(conn(t, server.URL, nil))); err == nil || posts != 0 {
		t.Fatalf("err=%v posts=%d", err, posts)
	}
}

func TestCancellationAfterSubmissionPreservesUnknownReceipt(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/monitoring":
			emptyMonitoring(w)
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring/add":
			close(started)
			<-release
			w.WriteHeader(202)
		}
	}))
	defer server.Close()
	c := conn(t, server.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out *pb.FulfillResponse
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := New().Fulfill(ctx, req(c)); done <- result{out, err} }()
	<-started
	cancel()
	got := <-done
	close(release)
	if got.err != nil || len(got.out.Targets) != 1 || got.out.Targets[0].ExternalStatus != "unknown" {
		t.Fatalf("out=%v err=%v", got.out, got.err)
	}
	receipt, err := decodeReceipt(got.out.Targets[0].ExternalId)
	if err != nil || receipt.Kind != "unknown" || receipt.RequestID != "stable-host-request" || receipt.ISBN != isbn {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func TestPDFSubmissionAndOptions(t *testing.T) {
	posted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/monitoring":
			emptyMonitoring(w)
		case "/api/v1/books":
			if r.URL.Query().Get("format") != "pdf" {
				t.Error("wrong list format")
			}
			emptyBooks(w)
		case "/api/v1/monitoring/add":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["preferred_format"] != "pdf" {
				t.Error("wrong format")
			}
			posted = true
			w.WriteHeader(202)
			jsonResponse(w, map[string]string{"request_id": "pdf-1"})
		}
	}))
	defer server.Close()
	c := conn(t, server.URL, map[string]any{"allow_pdf": true, "preferred_format": "pdf"})
	out, err := New().Fulfill(context.Background(), req(c))
	if err != nil || !posted || out.Targets[0].Quality != "pdf" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	for _, allow := range []bool{false, true} {
		options, err := New().ListConfigOptions(context.Background(), &pb.ListConfigOptionsRequest{CapabilityId: CapabilityID, Connection: conn(t, server.URL, map[string]any{"allow_pdf": allow})})
		want := 1
		if allow {
			want = 2
		}
		if err != nil || len(options.OptionsByField["preferred_format"].Options) != want {
			t.Fatalf("options=%v err=%v", options, err)
		}
	}
}

func TestReceiptAndBackendIdentityFailClosed(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		jsonResponse(w, map[string]string{"id": "monitor-1", "isbn13": "9780306406157", "format": "epub", "status": "completed", "error_message": "synthetic-secret"})
	}))
	defer server.Close()
	c := conn(t, server.URL, nil)
	r := receiptData{Kind: "monitoring", ID: "monitor-1", ISBN: isbn, Format: "epub", RequestID: "stable-host-request"}
	check := &pb.CheckStatusRequest{CapabilityId: CapabilityID, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: encodeReceipt(r)}}}
	got, err := New().CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].Status != "failed" || strings.Contains(got.String(), "synthetic-secret") {
		t.Fatalf("got=%v err=%v", got, err)
	}
	check.Request.ExternalIds["bloem_request_id"] = "another-request"
	if _, err := New().CheckStatus(context.Background(), check); err == nil || hits != 1 {
		t.Fatal("foreign receipt read backend")
	}
}

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	leaks := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/capture", 302) }))
	defer origin.Close()
	got, err := New().TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: CapabilityID, Connection: conn(t, origin.URL, nil)})
	if err != nil || got.Ok || leaks != 0 {
		t.Fatalf("got=%v err=%v leaked=%d", got, err, leaks)
	}
}

func TestOnlyExactISBNFormatIsAdopted(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/monitoring":
			jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "other", "isbn13": "9780306406157", "status": "completed", "selected_format": "epub"}, map[string]string{"id": "different-format", "isbn13": isbn, "status": "searching", "selected_format": "pdf"}}, "page": 1, "limit": 100, "total": 2})
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring/add":
			posts++
			w.WriteHeader(202)
			jsonResponse(w, map[string]string{"request_id": "new-epub"})
		}
	}))
	defer server.Close()
	out, err := New().Fulfill(context.Background(), req(conn(t, server.URL, nil)))
	if err != nil || posts != 1 || out.Targets[0].Status != "queued" {
		t.Fatalf("got=%v err=%v posts=%d", out, err, posts)
	}
}
