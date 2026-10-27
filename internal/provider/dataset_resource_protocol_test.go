package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"terraform-provider-superset/internal/client"
)

// protoSuperset is a stateful fake Superset for driving the provider through
// the real framework (tfprotov6) server, so private state round-trips exactly
// as it does between Terraform and the provider.
type protoSuperset struct {
	mu            sync.Mutex
	remoteSQL     string
	refreshStatus int
	refreshCalls  int
	putSQL        []string
}

func (f *protoSuperset) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/security/login":
		_, _ = w.Write([]byte(`{"access_token":"fake-token","refresh_token":"x"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/security/csrf_token/":
		_, _ = w.Write([]byte(`{"result":"csrf-abc"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/database/":
		_, _ = w.Write([]byte(`{"result":[{"id":1,"database_name":"SolidDB","backend":"postgresql"}]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/dataset/42":
		body, _ := json.Marshal(map[string]interface{}{"result": map[string]interface{}{
			"id": 42, "table_name": "roster", "schema": "reporting", "sql": f.remoteSQL,
			"database": map[string]interface{}{"id": 1},
		}})
		_, _ = w.Write(body)
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/dataset/42":
		var req struct {
			SQL string `json:"sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.putSQL = append(f.putSQL, req.SQL)
		f.remoteSQL = req.SQL
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/dataset/42/refresh":
		f.refreshCalls++
		w.WriteHeader(f.refreshStatus)
		_, _ = w.Write([]byte(`{"message":"refresh result"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *protoSuperset) set(fn func(*protoSuperset)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func objectWith(typ tftypes.Type, set map[string]tftypes.Value) tftypes.Value {
	obj := typ.(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(obj.AttributeTypes))
	for name, at := range obj.AttributeTypes {
		if v, ok := set[name]; ok {
			vals[name] = v
		} else {
			vals[name] = tftypes.NewValue(at, nil)
		}
	}
	return tftypes.NewValue(typ, vals)
}

func dynamicValue(t *testing.T, typ tftypes.Type, v tftypes.Value) *tfprotov6.DynamicValue {
	t.Helper()
	dv, err := tfprotov6.NewDynamicValue(typ, v)
	if err != nil {
		t.Fatalf("NewDynamicValue: %v", err)
	}
	return &dv
}

func failOnErrors(t *testing.T, step string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("%s: unexpected error: %s: %s", step, d.Summary, d.Detail)
		}
	}
}

func privateHasPendingKey(t *testing.T, private []byte) bool {
	t.Helper()
	if len(private) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(private, &m); err != nil {
		t.Fatalf("private state is not JSON: %v (%q)", err, private)
	}
	_, ok := m[datasetColumnsRefreshPendingKey]
	return ok
}

// Through the real framework server: a failed refresh is recorded in private
// state, survives Read, makes Read force an sql diff even after the operator
// reverts the config, and the next apply re-PUTs the configured SQL, retries
// the refresh, and clears the marker.
func TestDatasetProtocol_FailedRefresh_RetriedAfterRevert(t *testing.T) {
	ctx := context.Background()
	for _, k := range []string{"SUPERSET_HOST", "SUPERSET_USERNAME", "SUPERSET_PASSWORD",
		"SUPERSET_OIDC_TOKEN_URL", "SUPERSET_CLIENT_ID", "SUPERSET_CLIENT_SECRET"} {
		t.Setenv(k, "")
	}
	client.ClearGlobalDatabaseCache()
	t.Cleanup(client.ClearGlobalDatabaseCache)

	const sqlA = "SELECT id FROM laborers"
	const sqlB = "SELECT id, birth_date FROM laborers"
	fake := &protoSuperset{remoteSQL: sqlA, refreshStatus: http.StatusInternalServerError}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(server.Close)

	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	failOnErrors(t, "schema", schemas.Diagnostics)
	provType := schemas.Provider.ValueType()
	dsType := schemas.ResourceSchemas["superset_dataset"].ValueType()

	cfg := objectWith(provType, map[string]tftypes.Value{
		"host":     tftypes.NewValue(tftypes.String, server.URL),
		"username": tftypes.NewValue(tftypes.String, "u"),
		"password": tftypes.NewValue(tftypes.String, "p"),
	})
	conf, err := srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: dynamicValue(t, provType, cfg)})
	if err != nil {
		t.Fatal(err)
	}
	failOnErrors(t, "configure", conf.Diagnostics)

	dataset := func(id interface{}, sql string) tftypes.Value {
		return objectWith(dsType, map[string]tftypes.Value{
			"id":            tftypes.NewValue(tftypes.Number, id),
			"table_name":    tftypes.NewValue(tftypes.String, "roster"),
			"database_name": tftypes.NewValue(tftypes.String, "SolidDB"),
			"schema":        tftypes.NewValue(tftypes.String, "reporting"),
			"sql":           tftypes.NewValue(tftypes.String, sql),
		})
	}
	stateSQL := func(dv *tfprotov6.DynamicValue) string {
		v, err := dv.Unmarshal(dsType)
		if err != nil {
			t.Fatal(err)
		}
		var attrs map[string]tftypes.Value
		if err := v.As(&attrs); err != nil {
			t.Fatal(err)
		}
		var s string
		if err := attrs["sql"].As(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	apply := func(prior *tfprotov6.DynamicValue, sql string, private []byte) *tfprotov6.ApplyResourceChangeResponse {
		resp, err := srv.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{
			TypeName:       "superset_dataset",
			PriorState:     prior,
			PlannedState:   dynamicValue(t, dsType, dataset(42, sql)),
			Config:         dynamicValue(t, dsType, dataset(nil, sql)),
			PlannedPrivate: private,
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	read := func(current *tfprotov6.DynamicValue, private []byte) *tfprotov6.ReadResourceResponse {
		resp, err := srv.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
			TypeName: "superset_dataset", CurrentState: current, Private: private,
		})
		if err != nil {
			t.Fatal(err)
		}
		failOnErrors(t, "read", resp.Diagnostics)
		return resp
	}

	// Apply 1: A -> B. PUT lands, refresh fails.
	a1 := apply(dynamicValue(t, dsType, dataset(42, sqlA)), sqlB, nil)
	var refreshErr bool
	for _, d := range a1.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityError && d.Summary == "Error refreshing dataset columns" {
			refreshErr = true
		}
	}
	if !refreshErr {
		t.Fatalf("apply 1: want refresh error diagnostic, got %v", a1.Diagnostics)
	}
	if !privateHasPendingKey(t, a1.Private) {
		t.Fatalf("apply 1: pending marker missing from returned private state %q", a1.Private)
	}

	// Refresh: Superset holds B; Read must not store A or B (either would
	// let some config plan as "no changes"), and must keep the marker.
	r1 := read(a1.NewState, a1.Private)
	got := stateSQL(r1.NewState)
	if got == sqlA || got == sqlB || !strings.HasPrefix(got, sqlB) {
		t.Fatalf("read while pending stored sql %q; want Superset's sql with the pending marker", got)
	}
	if !privateHasPendingKey(t, r1.Private) {
		t.Fatal("read dropped the pending marker")
	}

	// Operator reverts config to A. Apply 2: refresh works now.
	fake.set(func(f *protoSuperset) { f.refreshStatus = http.StatusOK })
	a2 := apply(r1.NewState, sqlA, r1.Private)
	failOnErrors(t, "apply 2", a2.Diagnostics)
	if privateHasPendingKey(t, a2.Private) {
		t.Fatal("apply 2: pending marker not cleared after a successful refresh")
	}
	fake.set(func(f *protoSuperset) {
		if f.refreshCalls != 2 {
			t.Errorf("refresh calls = %d, want 2", f.refreshCalls)
		}
		if n := len(f.putSQL); n != 2 || f.putSQL[1] != sqlA {
			t.Errorf("PUT sql history = %q, want the reverted SQL re-PUT", f.putSQL)
		}
	})

	// Read after recovery reports Superset's SQL verbatim.
	if got := stateSQL(read(a2.NewState, a2.Private).NewState); got != sqlA {
		t.Fatalf("read after recovery stored %q, want %q", got, sqlA)
	}
}
