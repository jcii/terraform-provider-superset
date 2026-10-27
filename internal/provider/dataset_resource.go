package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"terraform-provider-superset/internal/client"
)

// Ensure the implementation satisfies the expected interfaces.
var (
	_ resource.Resource                = &datasetResource{}
	_ resource.ResourceWithConfigure   = &datasetResource{}
	_ resource.ResourceWithImportState = &datasetResource{}
)

// NewDatasetResource is a helper function to simplify the provider implementation.
func NewDatasetResource() resource.Resource {
	return &datasetResource{}
}

// datasetResource is the resource implementation.
type datasetResource struct {
	client *client.Client
}

// datasetResourceModel maps the resource schema data.
type datasetResourceModel struct {
	ID           types.Int64  `tfsdk:"id"`
	TableName    types.String `tfsdk:"table_name"`
	DatabaseName types.String `tfsdk:"database_name"`
	Schema       types.String `tfsdk:"schema"`
	SQL          types.String `tfsdk:"sql"`
}

// Metadata returns the resource type name.
func (r *datasetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dataset"
}

// Schema defines the schema for the resource.
func (r *datasetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a dataset in Superset.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Description: "Numeric identifier of the dataset.",
				Computed:    true,
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"table_name": schema.StringAttribute{
				Description: "Name of the table or dataset.",
				Required:    true,
			},
			"database_name": schema.StringAttribute{
				Description: "Name of the database where the dataset resides. Cannot be changed after creation.",
				Required:    true,
			},
			"schema": schema.StringAttribute{
				Description: "Database schema name (optional).",
				Optional:    true,
			},
			"sql": schema.StringAttribute{
				Description: "SQL query for the dataset (optional, for SQL-based datasets). When this changes on update, the provider re-syncs the dataset's columns from source (PUT /api/v1/dataset/{id}/refresh) so columns added by the new SQL are usable by charts in the same apply.",
				Optional:    true,
			},
		},
	}
}

// Create creates the resource and sets the initial Terraform state.
func (r *datasetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan datasetResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	tflog.Debug(ctx, "Creating dataset", map[string]interface{}{
		"table_name":    plan.TableName.ValueString(),
		"database_name": plan.DatabaseName.ValueString(),
	})

	// Get database ID by name
	databaseID, err := r.client.GetDatabaseIDByName(plan.DatabaseName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error finding database",
			fmt.Sprintf("Could not find database '%s': %s", plan.DatabaseName.ValueString(), err.Error()),
		)
		return
	}

	// Create dataset request
	datasetReq := client.DatasetRequest{
		TableName: plan.TableName.ValueString(),
		Database:  databaseID,
		Schema:    plan.Schema.ValueString(),
		SQL:       plan.SQL.ValueString(),
	}

	// Create dataset
	datasetResp, err := r.client.CreateDataset(datasetReq)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error creating dataset",
			"Could not create dataset: "+err.Error(),
		)
		return
	}

	// Extract ID from response
	var datasetID int64
	if id, ok := (*datasetResp)["id"].(float64); ok {
		datasetID = int64(id)
	} else {
		resp.Diagnostics.AddError(
			"Error creating dataset",
			"Could not extract ID from create response",
		)
		return
	}

	// Update the state
	plan.ID = types.Int64Value(datasetID)

	tflog.Debug(ctx, "Created dataset", map[string]interface{}{
		"id": datasetID,
	})

	// Set state to fully populated data
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Read refreshes the Terraform state with the latest data.
func (r *datasetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state datasetResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Get dataset from API
	dataset, err := r.client.GetDataset(state.ID.ValueInt64())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error reading dataset",
			"Could not read dataset ID "+fmt.Sprintf("%d", state.ID.ValueInt64())+": "+err.Error(),
		)
		return
	}

	// Update state from API response
	if tableName, ok := (*dataset)["table_name"].(string); ok {
		state.TableName = types.StringValue(tableName)
	}

	if schema, ok := (*dataset)["schema"].(string); ok {
		state.Schema = types.StringValue(schema)
	}

	var private privateKeyStore
	if req.Private != nil {
		private = req.Private
	}
	state.SQL = datasetSQLFromRead(state.SQL, (*dataset)["sql"], columnsRefreshPending(ctx, private))

	// Get database name by ID
	if database, ok := (*dataset)["database"].(map[string]interface{}); ok {
		if dbID, ok := database["id"].(float64); ok {
			databaseName, err := r.client.GetDatabaseNameByID(int64(dbID))
			if err != nil {
				resp.Diagnostics.AddError(
					"Error reading dataset",
					"Could not get database name: "+err.Error(),
				)
				return
			}
			state.DatabaseName = types.StringValue(databaseName)
		}
	}

	// Set refreshed state
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Update updates the resource and sets the updated Terraform state on success.
func (r *datasetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Retrieve values from plan
	var plan datasetResourceModel
	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Prior state, to tell whether the SQL changed in this plan.
	var state datasetResourceModel
	diags = req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var private privateKeyStore
	if resp.Private != nil {
		private = resp.Private
	}
	if !r.updateDataset(ctx, plan, state, private, &resp.Diagnostics) {
		return
	}

	// Set updated state
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// datasetColumnsRefreshPendingKey marks, in the resource's private state, a
// dataset whose SQL was PUT but whose column refresh then failed. Superset
// already holds the new SQL, so a plain Read would leave no diff and the
// refresh would never be retried. While it is set, Read marks the stored sql
// (datasetRefreshPendingSQLSuffix) so the next plan always diffs, and Update
// refreshes even if the SQL did not change in that plan.
const datasetColumnsRefreshPendingKey = "columns_refresh_pending"

// privateKeyStore is the subset of the framework's private-state API used
// here, so the update logic can be tested with an in-memory store.
type privateKeyStore interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

func columnsRefreshPending(ctx context.Context, private privateKeyStore) bool {
	if private == nil {
		return false
	}
	v, _ := private.GetKey(ctx, datasetColumnsRefreshPendingKey)
	return len(v) > 0
}

// updateDataset PUTs the planned dataset and, when the SQL changed (or an
// earlier column refresh is still pending), re-syncs its columns from source.
// It returns false when it added an error diagnostic.
func (r *datasetResource) updateDataset(ctx context.Context, plan, state datasetResourceModel, private privateKeyStore, diags *diag.Diagnostics) bool {
	id := plan.ID.ValueInt64()

	// Update dataset (database cannot be changed, so we don't validate it)
	err := r.client.UpdateDataset(
		id,
		plan.TableName.ValueString(),
		plan.Schema.ValueString(),
		plan.SQL.ValueString(),
	)
	if err != nil {
		diags.AddError(
			"Error updating dataset",
			"Could not update dataset ID "+fmt.Sprintf("%d", id)+": "+err.Error(),
		)
		return false
	}

	tflog.Debug(ctx, "Updated dataset", map[string]interface{}{
		"id": id,
	})

	// Superset derives a dataset's column list only on create; a column added
	// to the SQL stays unknown (and charts requesting it fail with "Columns
	// missing in dataset") until the columns are re-synced from source. Do it
	// here, only when the SQL changed or a previous refresh failed, so one
	// apply ships the new columns before dependent charts are applied.
	if plan.SQL.Equal(state.SQL) && !columnsRefreshPending(ctx, private) {
		return true
	}

	if err := r.client.RefreshDataset(id); err != nil {
		// The PUT already landed, so Superset holds the new SQL and a plain
		// Read would hide the diff. Mark the refresh pending so Read forces a
		// diff and the next apply retries; the prior state is left in place.
		if private != nil {
			diags.Append(private.SetKey(ctx, datasetColumnsRefreshPendingKey, []byte("true"))...)
		}
		diags.AddError(
			"Error refreshing dataset columns",
			"Dataset ID "+fmt.Sprintf("%d", id)+" was updated with the new SQL, but re-syncing its "+
				"columns from source failed, so columns added by the SQL change are not yet known to "+
				"Superset and charts using them will fail. The next apply retries the refresh; to fix "+
				"it sooner, use \"Sync columns from source\" on the dataset in Superset. Cause: "+err.Error(),
		)
		return false
	}

	if private != nil {
		diags.Append(private.SetKey(ctx, datasetColumnsRefreshPendingKey, nil)...)
	}
	tflog.Debug(ctx, "Refreshed dataset columns from source", map[string]interface{}{
		"id": id,
	})
	return !diags.HasError()
}

// datasetRefreshPendingSQLSuffix is appended to the sql Read stores while a
// column refresh is pending. No configuration contains it, so whatever the
// config's sql is (the SQL that was PUT, a reverted one, or none), the next
// plan shows an sql diff and the next apply runs Update, which re-PUTs the
// configured SQL and retries the refresh. The plan diff names the reason.
const datasetRefreshPendingSQLSuffix = "\n-- terraform-provider-superset: column refresh pending; the next apply re-applies this dataset and retries it"

// datasetSQLFromRead is the sql value Read stores: Superset's SQL (so
// out-of-band drift still shows), suffixed while a column refresh is pending.
func datasetSQLFromRead(prior types.String, remote interface{}, refreshPending bool) types.String {
	sql := prior
	if s, ok := remote.(string); ok {
		sql = types.StringValue(s)
	}
	if refreshPending {
		return types.StringValue(sql.ValueString() + datasetRefreshPendingSQLSuffix)
	}
	return sql
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *datasetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Retrieve values from state
	var state datasetResourceModel
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Delete existing dataset
	err := r.client.DeleteDataset(state.ID.ValueInt64())
	if err != nil {
		resp.Diagnostics.AddError(
			"Error deleting dataset",
			"Could not delete dataset, unexpected error: "+err.Error(),
		)
		return
	}

	tflog.Debug(ctx, "Deleted dataset", map[string]interface{}{
		"id": state.ID.ValueInt64(),
	})
}

// Configure adds the provider configured client to the resource.
func (r *datasetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*client.Client)

	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *client.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.client = client
}

// ImportState imports the resource state.
func (r *datasetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Retrieve import ID and save to id attribute
	id, err := strconv.ParseInt(req.ID, 10, 64)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error importing dataset",
			"Could not parse dataset ID: "+err.Error(),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
