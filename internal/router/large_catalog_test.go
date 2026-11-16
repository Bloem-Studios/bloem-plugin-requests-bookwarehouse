package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLargeCatalogDoesNotBlockSubmission(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			var posts, lists atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/monitoring":
					emptyMonitoring(w)
				case "/api/v1/books":
					lists.Add(1)
					rows := make([]map[string]string, 100)
					for i := range rows {
						rows[i] = map[string]string{"id": fmt.Sprintf("book-%d", i), "file_format": "epub"}
					}
					jsonResponse(w, map[string]any{"books": rows, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 245000, "total_pages": 2450}})
				case "/api/v1/monitoring/add":
					posts.Add(1)
					if present {
						w.WriteHeader(http.StatusConflict)
						jsonResponse(w, map[string]string{"error": "book_exists"})
					} else {
						w.WriteHeader(http.StatusAccepted)
						jsonResponse(w, map[string]string{"request_id": "monitor-large", "status": "searching"})
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			out, err := New().Fulfill(context.Background(), req(conn(t, server.URL, nil)))
			if err != nil {
				t.Fatalf("large catalog blocked request: %v", err)
			}
			if posts.Load() != 1 || lists.Load() != 1 || len(out.Targets) != 1 {
				t.Fatalf("posts=%d lists=%d response=%v", posts.Load(), lists.Load(), out)
			}
			want := "queued"
			if present {
				want = "completed"
			}
			if out.Targets[0].Status != want {
				t.Fatalf("status=%s want=%s", out.Targets[0].Status, want)
			}
		})
	}
}

func TestPreflightFailureProvesNothingSubmitted(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusUnauthorized)
		jsonResponse(w, map[string]string{"error": "synthetic-secret"})
	}))
	defer server.Close()
	_, err := New().Fulfill(context.Background(), req(conn(t, server.URL, nil)))
	if status.Code(err) != codes.FailedPrecondition || posts.Load() != 0 {
		t.Fatalf("preflight must refuse before submission: code=%s posts=%d", status.Code(err), posts.Load())
	}
}
