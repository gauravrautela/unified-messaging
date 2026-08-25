package outlook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticTokens struct{}

func (staticTokens) AccessToken(context.Context, string, bool) (string, error) {
	return "test-token", nil
}

// Graph types the /attachments collection as the base microsoft.graph.attachment,
// which has no contentId property — selecting it is a 400 on the real service,
// so the fake enforces the same rule.
func TestListAttachmentsSelectsOnlyBaseProperties(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/attachments") {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if sel := r.URL.Query().Get("$select"); strings.Contains(sel, "contentId") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"BadRequest","message":"Parsing OData Select and Expand failed: Could not find a property named 'contentId' on type 'microsoft.graph.attachment'."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[{"id":"att-1","name":"report.pdf","contentType":"application/pdf","size":12345,"isInline":false}]}`))
	}))
	defer srv.Close()

	oldBase := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = oldBase }()

	c := newClient(staticTokens{})
	atts, err := c.ListAttachments(context.Background(), "acc-1", "msg-1")
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("got %d attachments, want 1", len(atts))
	}
	a := atts[0]
	if a.ID != "att-1" || a.Name != "report.pdf" || a.MimeType != "application/pdf" || a.Size != 12345 || a.IsInline {
		t.Fatalf("attachment mapped wrong: %+v", a)
	}
}
