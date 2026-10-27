## 0.3.0 (TBD)

FEATURES:
* **New Resource**: `superset_user` - Manage Superset users
* **New Data Source**: `superset_users` - Fetch users from Superset

IMPROVEMENTS:
* `superset_dataset`: when `sql` changes on update, the provider now calls `PUT /api/v1/dataset/{id}/refresh` after the update so Superset re-syncs the dataset's columns from source. A column added to a virtual dataset's SQL is usable by charts in the same apply, instead of failing with "Columns missing in dataset" until a manual "Sync columns from source". A refresh failure is reported as an error and leaves the prior state, so the next apply retries.

## 0.2.0 (2025-08-29)

FEATURES:
* **New Resource**: `superset_meta_database` - Support for Superset meta database connections for cross-database queries
* **New Resource**: `superset_dataset` - Manage individual Superset datasets
* **New Data Source**: `superset_datasets` - Fetch all datasets from Superset 
 
IMPROVEMENTS:
* Added comprehensive test coverage for meta database resource
* Added schema-level default values for boolean attributes
* Added global caching for database API calls to improve performance across multiple client instances
* Added pagination support (page_size:5000) to datasets API calls
  