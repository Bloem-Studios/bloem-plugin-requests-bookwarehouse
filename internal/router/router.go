// Package router implements the ordinary request_router.v1 Bookwarehouse plugin.
package router

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

const CapabilityID = "bookwarehouse"

type Router struct {
	pb.UnimplementedRequestRouterServer
	mu sync.Mutex
}

func New() *Router { return &Router{} }

type settings struct {
	allowHTTP, allowPDF bool
	format              string
}

func config(c *pb.RouterConnection) (settings, map[string]string) {
	s := settings{format: "epub"}
	issues := map[string]string{}
	if c == nil {
		issues["connection"] = "A connection is required."
		return s, issues
	}
	fields := c.GetConfig().GetFields()
	for _, name := range []string{"allow_http", "allow_pdf"} {
		if v, ok := fields[name]; ok {
			if _, ok := v.Kind.(*structpb.Value_BoolValue); !ok {
				issues[name] = "Use a boolean value."
			} else if name == "allow_http" {
				s.allowHTTP = v.GetBoolValue()
			} else {
				s.allowPDF = v.GetBoolValue()
			}
		}
	}
	if v, ok := fields["preferred_format"]; ok {
		if _, ok := v.Kind.(*structpb.Value_StringValue); !ok {
			issues["preferred_format"] = "Choose EPUB or PDF."
		} else {
			s.format = v.GetStringValue()
		}
	}
	if s.format != "epub" && (s.format != "pdf" || !s.allowPDF) {
		issues["preferred_format"] = "PDF requires the connection's Allow PDF setting; otherwise choose EPUB."
	}
	if _, ok := validBase(c.BaseUrl, s.allowHTTP); !ok {
		issues["base_url"] = "Use an HTTPS origin and optional plain path prefix; HTTP requires explicit opt-in."
	}
	if len(c.ApiKey) == 0 || len(c.ApiKey) > 8192 {
		issues["api_key"] = "An API key is required."
	} else {
		for _, r := range c.ApiKey {
			if r < 0x21 || r > 0x7e {
				issues["api_key"] = "Use a valid API key."
				break
			}
		}
	}
	if !validID(c.Id) {
		issues["id"] = "A connection ID is required."
	}
	return s, issues
}
func open(c *pb.RouterConnection) (*client, settings, error) {
	s, issues := config(c)
	if len(issues) > 0 {
		return nil, s, status.Error(codes.InvalidArgument, "Invalid Bookwarehouse connection configuration.")
	}
	cl, err := newClient(c.BaseUrl, c.ApiKey, s.allowHTTP)
	if err != nil {
		return nil, s, status.Error(codes.InvalidArgument, "Invalid Bookwarehouse connection configuration.")
	}
	return cl, s, nil
}
func requestIdentity(r *pb.RequestDescriptor) (string, string, error) {
	if r == nil || r.MediaType != "book" || len(r.Seasons) != 0 {
		return "", "", status.Error(codes.InvalidArgument, "Only ISBN ebook requests are supported.")
	}
	isbn := r.ExternalIds["isbn"]
	id := r.ExternalIds["bloem_request_id"]
	if !validISBN13(isbn) || !validID(id) {
		return "", "", status.Error(codes.InvalidArgument, "A valid canonical ISBN13 and stable Bloem request ID are required.")
	}
	return isbn, id, nil
}
func capability(id string) error {
	if id != CapabilityID {
		return status.Error(codes.InvalidArgument, "Unknown request capability.")
	}
	return nil
}
func validISBN13(s string) bool {
	if len(s) != 13 || (!strings.HasPrefix(s, "978") && !strings.HasPrefix(s, "979")) {
		return false
	}
	sum := 0
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		weight := 1
		if i%2 == 1 {
			weight = 3
		}
		sum += int(s[i]-'0') * weight
	}
	return sum%10 == 0
}
func isbn10To13(s string) string {
	if len(s) != 10 {
		return ""
	}
	sum := 0
	for i := range s {
		v := int(s[i] - '0')
		if i == 9 && s[i] == 'X' {
			v = 10
		} else if s[i] < '0' || s[i] > '9' {
			return ""
		}
		sum += (10 - i) * v
	}
	if sum%11 != 0 {
		return ""
	}
	out := "978" + s[:9]
	sum = 0
	for i := range out {
		weight := 1
		if i%2 == 1 {
			weight = 3
		}
		sum += int(out[i]-'0') * weight
	}
	return out + string(byte('0'+(10-sum%10)%10))
}

// Field names/order form the documented deterministic host recovery receipt.
type receiptData struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ISBN      string `json:"isbn"`
	Format    string `json:"format"`
	RequestID string `json:"request_id"`
}

func encodeReceipt(r receiptData) string {
	b, _ := json.Marshal(r)
	return "bwh1." + base64.RawURLEncoding.EncodeToString(b)
}
func decodeReceipt(raw string) (receiptData, error) {
	var out receiptData
	if len(raw) > 2048 || !strings.HasPrefix(raw, "bwh1.") {
		return out, errBackend
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "bwh1."))
	if err != nil || json.Unmarshal(b, &out) != nil || !validISBN13(out.ISBN) || !validID(out.RequestID) || (out.Format != "epub" && out.Format != "pdf") {
		return out, errBackend
	}
	switch out.Kind {
	case "monitoring", "book":
		if !validID(out.ID) {
			return out, errBackend
		}
	case "presence", "unknown":
		if out.ID != "" {
			return out, errBackend
		}
	default:
		return out, errBackend
	}
	return out, nil
}
func normalize(raw string) (string, string) {
	switch raw {
	case "completed":
		return "completed", "Downloaded; awaiting linked library scan."
	case "downloading", "importing", "processing":
		return "downloading", "Bookwarehouse is downloading the ebook."
	case "pending", "queued", "searching", "searching_now", "found", "monitored", "retrying", "not_found":
		return "queued", "Bookwarehouse is searching for the ebook."
	case "failed", "cancelled", "canceled", "expired":
		return "failed", "Bookwarehouse could not complete this request."
	case "unknown":
		return "queued", "Submission outcome unknown; awaiting backend evidence."
	case "book_exists":
		return "completed", "Book already present; awaiting linked library scan."
	default:
		return "failed", "Bookwarehouse returned an unsupported request state."
	}
}
func safeExternal(raw string) string {
	switch raw {
	case "completed", "downloading", "importing", "processing", "pending", "queued", "searching", "searching_now", "found", "monitored", "retrying", "not_found", "failed", "cancelled", "canceled", "expired", "unknown", "book_exists", "missing":
		return raw
	default:
		return "unsupported"
	}
}
func target(c *pb.RouterConnection, receipt receiptData, raw string) *pb.FulfillmentTarget {
	state, message := normalize(raw)
	return &pb.FulfillmentTarget{ConnectionId: c.Id, Quality: receipt.Format, ExternalId: encodeReceipt(receipt), ExternalStatus: safeExternal(raw), Status: state, Message: message}
}

func (r *Router) Fulfill(ctx context.Context, in *pb.FulfillRequest) (*pb.FulfillResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if err := capability(in.CapabilityId); err != nil {
		return nil, err
	}
	isbn, id, err := requestIdentity(in.Request)
	if err != nil {
		return nil, err
	}
	if len(in.Connections) != 1 {
		return nil, status.Error(codes.InvalidArgument, "Exactly one Bookwarehouse connection is required.")
	}
	c := in.Connections[0]
	cl, s, err := open(c)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	if len(in.Qualities) > 1 || (len(in.Qualities) == 1 && (in.Qualities[0].GetId() != s.format || in.Qualities[0].GetIs4K())) {
		return nil, status.Error(codes.InvalidArgument, "Requested format must match the connection configuration.")
	}
	// This serializes callers sharing a plugin process; host ISBN leases are still required across processes.
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	row, kind, err := cl.preflight(ctx, isbn, s.format)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "Bookwarehouse preflight unavailable; nothing submitted.")
	}
	receipt := receiptData{Kind: kind, ID: row.ID, ISBN: isbn, Format: s.format, RequestID: id}
	raw := row.Status
	if kind == "" {
		receipt.Kind = "unknown"
		var response struct {
			ID     string `json:"request_id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		code, err := cl.json(ctx, "POST", "/monitoring/add", nil, map[string]string{"isbn": isbn, "preferred_format": s.format}, &response)
		switch {
		case err == nil && code == 202 && validID(response.ID):
			receipt.Kind = "monitoring"
			receipt.ID = response.ID
			raw = "searching"
		case err == nil && code == 409 && response.Error == "book_exists":
			receipt.Kind = "presence"
			raw = "book_exists"
		case code >= 400 && code < 500 && code != 409:
			return nil, status.Error(codes.FailedPrecondition, "Bookwarehouse rejected the ebook submission.")
		default:
			raw = "unknown"
		}
	}
	return &pb.FulfillResponse{Targets: []*pb.FulfillmentTarget{target(c, receipt, raw)}}, nil
}

func (r *Router) CheckStatus(ctx context.Context, in *pb.CheckStatusRequest) (*pb.CheckStatusResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if err := capability(in.CapabilityId); err != nil {
		return nil, err
	}
	isbn, id, err := requestIdentity(in.Request)
	if err != nil {
		return nil, err
	}
	if len(in.Connections) != 1 || len(in.Targets) != 1 {
		return nil, status.Error(codes.InvalidArgument, "Exactly one Bookwarehouse connection and receipt are required.")
	}
	c := in.Connections[0]
	ref := in.Targets[0]
	receipt, err := decodeReceipt(ref.GetExternalId())
	if err != nil || receipt.ISBN != isbn || receipt.RequestID != id || c == nil || ref.ConnectionId != c.Id || ref.Quality != receipt.Format {
		return nil, status.Error(codes.InvalidArgument, "Receipt does not match the request and connection.")
	}
	cl, s, err := open(c)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	if receipt.Format != s.format {
		return nil, status.Error(codes.FailedPrecondition, "Connection format differs from the durable receipt.")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw := "unknown"
	switch receipt.Kind {
	case "unknown", "presence":
		row, kind, err := cl.preflight(ctx, isbn, receipt.Format)
		if err != nil {
			return nil, status.Error(codes.Unavailable, "Bookwarehouse status unavailable.")
		}
		if kind != "" {
			raw = row.Status
		} else if receipt.Kind == "presence" {
			raw = "book_exists"
		}
	case "monitoring", "book":
		var row backendRow
		path := "/monitoring/"
		book := receipt.Kind == "book"
		if book {
			path = "/books/"
		}
		code, err := cl.json(ctx, "GET", path+receipt.ID, nil, nil, &row)
		if code == 404 {
			raw = "missing"
		} else if err != nil || code != 200 {
			return nil, status.Error(codes.Unavailable, "Bookwarehouse status unavailable.")
		} else if row.ID != receipt.ID || !row.matches(isbn, receipt.Format, book) {
			raw = "unsupported"
		} else {
			raw = row.Status
			if book {
				raw = "completed"
			}
		}
	}
	state, message := normalize(raw)
	if raw == "missing" {
		state = "failed"
		message = "Backend receipt missing; review required before any retry."
	}
	return &pb.CheckStatusResponse{Statuses: []*pb.TargetStatus{{ConnectionId: c.Id, Quality: receipt.Format, ExternalStatus: safeExternal(raw), Status: state, Message: message}}}, nil
}

func (r *Router) Validate(ctx context.Context, in *pb.ValidateRequest) (*pb.ValidateResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if err := capability(in.CapabilityId); err != nil {
		return nil, err
	}
	_, issues := config(in.Connection)
	out := &pb.ValidateResponse{FieldErrors: issues}
	if len(in.Siblings) > 0 {
		out.FormError = "Only one Bookwarehouse connection is supported."
	}
	return out, nil
}
func (r *Router) ListConfigOptions(ctx context.Context, in *pb.ListConfigOptionsRequest) (*pb.ListConfigOptionsResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if err := capability(in.CapabilityId); err != nil {
		return nil, err
	}
	options := []*pb.ConfigOption{{Value: "epub", Label: "EPUB"}}
	s, _ := config(in.Connection)
	if s.allowPDF {
		options = append(options, &pb.ConfigOption{Value: "pdf", Label: "PDF"})
	}
	return &pb.ListConfigOptionsResponse{OptionsByField: map[string]*pb.ConfigOptionList{"preferred_format": {Options: options}}}, nil
}
func (r *Router) TestConnection(ctx context.Context, in *pb.TestConnectionRequest) (*pb.TestConnectionResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if err := capability(in.CapabilityId); err != nil {
		return nil, err
	}
	cl, _, err := open(in.Connection)
	if err != nil {
		return &pb.TestConnectionResponse{Message: "Invalid Bookwarehouse connection configuration."}, nil
	}
	defer cl.close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := cl.monitoring(ctx, 1); err != nil {
		return &pb.TestConnectionResponse{Message: "Bookwarehouse monitoring authentication or response check failed."}, nil
	}
	return &pb.TestConnectionResponse{Ok: true, Message: "Bookwarehouse monitoring API authenticated."}, nil
}
