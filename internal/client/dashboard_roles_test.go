package client

import (
	"encoding/json"
	"testing"
)

func dashboardPayloadRoles(t *testing.T, roles []int64) (present bool, value []int64) {
	t.Helper()
	encoded, err := json.Marshal(newDashboardPayload(Dashboard{Title: "d", Roles: roles}, ""))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	raw, ok := body["roles"]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return true, value
}

func TestDashboardPayloadOmitsRolesWhenNotConfigured(t *testing.T) {
	if present, _ := dashboardPayloadRoles(t, nil); present {
		t.Fatal("roles must not be sent when the configuration does not set them: Superset hooks that tag dashboards would be undone")
	}
}

func TestDashboardPayloadSendsAnEmptyRoleSetToRevoke(t *testing.T) {
	present, roles := dashboardPayloadRoles(t, []int64{})
	if !present || len(roles) != 0 {
		t.Fatalf("an explicit empty role set must be sent as [], got present=%v roles=%v", present, roles)
	}
}

func TestDashboardPayloadSendsConfiguredRoles(t *testing.T) {
	present, roles := dashboardPayloadRoles(t, []int64{5, 7})
	if !present || len(roles) != 2 || roles[0] != 5 || roles[1] != 7 {
		t.Fatalf("configured roles must be sent, got present=%v roles=%v", present, roles)
	}
}
