package service

import "context"

// KiroModelCatalogRepository is the catalog-specific conditional write contract.
type KiroModelCatalogRepository interface {
	UpdateKiroModelCatalogIfCurrent(ctx context.Context, id int64, catalog map[string]any, expectedWriteVersion int64, expectedCredentialGeneration int64) (bool, error)
}
