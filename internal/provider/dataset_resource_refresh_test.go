package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"terraform-provider-superset/internal/client"
)

// fakeSuperset is a hermetic stand-in for the Superset endpoints that
// datasetResource.Update touches. It records every request and lets a test
// choose the status code of the column-refresh call.
type fakeSuperset struct {
	mu            sync.Mutex
	requests      []string // "METHOD path"
	refreshCSRF   string   // X-CSRFToken seen on the refresh call
	refreshCookie string   // session cookie seen on the refresh call
	refreshStatus int
}

func (f *fakeSuperset) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/security/csrf_token/":
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "sess-123"})
		_, _ = w.Write([]byte(`{"result":"csrf-abc"}`))
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/dataset/42":
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/dataset/42/refresh":
		f.refreshCSRF = r.Header.Get("X-CSRFToken")
		if c, err := r.Cookie("session"); err == nil {
			f.refreshCookie = c.Value
		}
		w.WriteHeader(f.refreshStatus)
		if f.refreshStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"message":"OK"}`))
		} else {
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSuperset) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r == method+" "+path {
			n++
		}
	}
	return n
}

// runDatasetUpdate drives datasetResource.Update directly from prior and
// planned models against a fake Superset, the same way the framework calls it.
func runDatasetUpdate(t *testing.T, refreshStatus int, prior, planned datasetResourceModel) (*fakeSuperset, *resource.UpdateResponse) {
	t.Helper()
	ctx := context.Background()

	fake := &fakeSuperset{refreshStatus: refreshStatus}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)

	r := &datasetResource{client: &client.Client{Host: server.URL, Token: "fake-token"}}

	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics)
	}
	s := schemaResp.Schema

	empty := tftypes.NewValue(s.Type().TerraformType(ctx), nil)
	state := tfsdk.State{Schema: s, Raw: empty}
	if d := state.Set(ctx, prior); d.HasError() {
		t.Fatalf("state.Set: %v", d)
	}
	plan := tfsdk.Plan{Schema: s, Raw: empty}
	if d := plan.Set(ctx, planned); d.HasError() {
		t.Fatalf("plan.Set: %v", d)
	}

	req := resource.UpdateRequest{State: state, Plan: plan}
	// The framework seeds the response state with the prior state.
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: state.Raw.Copy()}}
	r.Update(ctx, req, resp)
	return fake, resp
}

func datasetModel(tableName, schema string, sql types.String) datasetResourceModel {
	return datasetResourceModel{
		ID:           types.Int64Value(42),
		TableName:    types.StringValue(tableName),
		DatabaseName: types.StringValue("SolidDB"),
		Schema:       types.StringValue(schema),
		SQL:          sql,
	}
}

func TestDatasetUpdate_SQLChange_RefreshesColumns(t *testing.T) {
	prior := datasetModel("roster", "reporting", types.StringValue("SELECT id FROM laborers"))
	planned := datasetModel("roster", "reporting", types.StringValue("SELECT id, birth_date FROM laborers"))

	fake, resp := runDatasetUpdate(t, http.StatusOK, prior, planned)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if got := fake.count("PUT", "/api/v1/dataset/42"); got != 1 {
		t.Fatalf("dataset PUT calls = %d, want 1", got)
	}
	if got := fake.count("PUT", "/api/v1/dataset/42/refresh"); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (requests: %v)", got, fake.requests)
	}
	// The refresh must come after the SQL PUT, or it re-syncs the old SQL.
	putIdx, refreshIdx := -1, -1
	for i, r := range fake.requests {
		switch r {
		case "PUT /api/v1/dataset/42":
			putIdx = i
		case "PUT /api/v1/dataset/42/refresh":
			refreshIdx = i
		}
	}
	if refreshIdx < putIdx {
		t.Fatalf("refresh ran before the SQL PUT: %v", fake.requests)
	}
	if fake.refreshCSRF != "csrf-abc" {
		t.Errorf("refresh X-CSRFToken = %q, want csrf-abc", fake.refreshCSRF)
	}
	if fake.refreshCookie != "sess-123" {
		t.Errorf("refresh session cookie = %q, want sess-123", fake.refreshCookie)
	}

	var got datasetResourceModel
	resp.State.Get(context.Background(), &got)
	if got.SQL.ValueString() != planned.SQL.ValueString() {
		t.Errorf("state sql = %q, want planned sql", got.SQL.ValueString())
	}
}

func TestDatasetUpdate_SQLAddedOrRemoved_RefreshesColumns(t *testing.T) {
	cases := map[string]struct{ prior, planned types.String }{
		"physical to virtual": {types.StringNull(), types.StringValue("SELECT 1 AS x")},
		"virtual to physical": {types.StringValue("SELECT 1 AS x"), types.StringNull()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake, resp := runDatasetUpdate(t, http.StatusOK,
				datasetModel("roster", "reporting", tc.prior),
				datasetModel("roster", "reporting", tc.planned))
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
			}
			if got := fake.count("PUT", "/api/v1/dataset/42/refresh"); got != 1 {
				t.Fatalf("refresh calls = %d, want 1", got)
			}
		})
	}
}

func TestDatasetUpdate_NonSQLChange_DoesNotRefresh(t *testing.T) {
	sql := types.StringValue("SELECT id FROM laborers")
	prior := datasetModel("roster", "reporting", sql)
	planned := datasetModel("roster_renamed", "reporting_v2", sql)

	fake, resp := runDatasetUpdate(t, http.StatusOK, prior, planned)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	if got := fake.count("PUT", "/api/v1/dataset/42"); got != 1 {
		t.Fatalf("dataset PUT calls = %d, want 1", got)
	}
	if got := fake.count("PUT", "/api/v1/dataset/42/refresh"); got != 0 {
		t.Fatalf("refresh calls = %d, want 0 when sql is unchanged", got)
	}
}

func TestDatasetUpdate_RefreshFailure_IsErrorAndKeepsPriorState(t *testing.T) {
	prior := datasetModel("roster", "reporting", types.StringValue("SELECT id FROM laborers"))
	planned := datasetModel("roster", "reporting", types.StringValue("SELECT id, birth_date FROM laborers"))

	fake, resp := runDatasetUpdate(t, http.StatusInternalServerError, prior, planned)

	if got := fake.count("PUT", "/api/v1/dataset/42/refresh"); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic when the column refresh fails")
	}
	var found bool
	for _, d := range resp.Diagnostics.Errors() {
		if d.Summary() == "Error refreshing dataset columns" &&
			strings.Contains(d.Detail(), "status code: 500") {
			found = true
		}
	}
	if !found {
		t.Fatalf("refresh error diagnostic missing or lacks the status: %v", resp.Diagnostics)
	}

	// Prior state is kept so the next plan still shows the SQL diff and the
	// next apply retries the PUT + refresh.
	var got datasetResourceModel
	resp.State.Get(context.Background(), &got)
	if got.SQL.ValueString() != prior.SQL.ValueString() {
		t.Errorf("state sql = %q, want prior sql %q", got.SQL.ValueString(), prior.SQL.ValueString())
	}
}
