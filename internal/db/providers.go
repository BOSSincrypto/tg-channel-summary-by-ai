package db

import (
	"database/sql"
	"fmt"

	"github.com/boss/tg-channel-summary-by-ai/internal/model"
)

// ProviderRepository provides CRUD operations for AI providers.
type ProviderRepository struct {
	db        *DB
	keyCipher *secretCipher
}

// Insert adds a new AI provider. If is_default is true, all other providers
// are cleared of their default status first.
func (r *ProviderRepository) Insert(ap *model.AIProvider) (int64, error) {
	conn := r.db.Conn()
	if ap == nil {
		return 0, fmt.Errorf("insert provider: provider is nil")
	}
	encryptedKey, err := r.keyCipher.encrypt(ap.APIKey)
	if err != nil {
		return 0, fmt.Errorf("encrypt provider API key: %w", err)
	}

	if ap.IsDefault {
		if _, err := conn.Exec(`UPDATE ai_providers SET is_default = 0`); err != nil {
			return 0, fmt.Errorf("clear existing defaults: %w", err)
		}
	}

	result, err := conn.Exec(
		`INSERT INTO ai_providers (name, base_url, api_key, default_model, is_default, version)
		 VALUES (?, ?, ?, ?, ?, 1)`,
		ap.Name, ap.BaseURL, encryptedKey, ap.DefaultModel, boolToInt(ap.IsDefault),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("insert provider %q: %w", ap.Name, ErrDuplicate)
		}
		return 0, fmt.Errorf("insert provider: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// GetByID returns a provider by its ID.
func (r *ProviderRepository) GetByID(id int64) (*model.AIProvider, error) {
	ap := &model.AIProvider{}
	var isDefault int
	err := r.db.Conn().QueryRow(
		`SELECT id, version, name, base_url, api_key, default_model, is_default, created_at
		 FROM ai_providers WHERE id = ?`, id,
	).Scan(&ap.ID, &ap.Version, &ap.Name, &ap.BaseURL, &ap.APIKey, &ap.DefaultModel, &isDefault, &ap.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider by id: %w", err)
	}
	if ap.APIKey, err = r.keyCipher.decrypt(ap.APIKey); err != nil {
		return nil, fmt.Errorf("decrypt provider API key: %w", err)
	}
	ap.IsDefault = intToBool(isDefault)
	return ap, nil
}

// GetByName returns a provider by its name.
func (r *ProviderRepository) GetByName(name string) (*model.AIProvider, error) {
	ap := &model.AIProvider{}
	var isDefault int
	err := r.db.Conn().QueryRow(
		`SELECT id, version, name, base_url, api_key, default_model, is_default, created_at
		 FROM ai_providers WHERE name = ? COLLATE NOCASE`, name,
	).Scan(&ap.ID, &ap.Version, &ap.Name, &ap.BaseURL, &ap.APIKey, &ap.DefaultModel, &isDefault, &ap.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider by name: %w", err)
	}
	if ap.APIKey, err = r.keyCipher.decrypt(ap.APIKey); err != nil {
		return nil, fmt.Errorf("decrypt provider API key: %w", err)
	}
	ap.IsDefault = intToBool(isDefault)
	return ap, nil
}

// GetDefault returns the default AI provider, or ErrNotFound if none is set.
func (r *ProviderRepository) GetDefault() (*model.AIProvider, error) {
	ap := &model.AIProvider{}
	var isDefault int
	err := r.db.Conn().QueryRow(
		`SELECT id, version, name, base_url, api_key, default_model, is_default, created_at
		 FROM ai_providers WHERE is_default = 1 LIMIT 1`,
	).Scan(&ap.ID, &ap.Version, &ap.Name, &ap.BaseURL, &ap.APIKey, &ap.DefaultModel, &isDefault, &ap.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get default provider: %w", err)
	}
	if ap.APIKey, err = r.keyCipher.decrypt(ap.APIKey); err != nil {
		return nil, fmt.Errorf("decrypt provider API key: %w", err)
	}
	ap.IsDefault = intToBool(isDefault)
	return ap, nil
}

// List returns all AI providers.
func (r *ProviderRepository) List() ([]model.AIProvider, error) {
	rows, err := r.db.Conn().Query(
		`SELECT id, version, name, base_url, api_key, default_model, is_default, created_at
		 FROM ai_providers ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()

	var providers []model.AIProvider
	for rows.Next() {
		var ap model.AIProvider
		var isDefault int
		if err := rows.Scan(&ap.ID, &ap.Version, &ap.Name, &ap.BaseURL, &ap.APIKey, &ap.DefaultModel, &isDefault, &ap.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		ap.APIKey, err = r.keyCipher.decrypt(ap.APIKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider API key: %w", err)
		}
		ap.IsDefault = intToBool(isDefault)
		providers = append(providers, ap)
	}
	return providers, rows.Err()
}

// Update modifies an existing provider. If is_default is true, clears others.
func (r *ProviderRepository) Update(ap *model.AIProvider) error {
	conn := r.db.Conn()
	if ap == nil {
		return fmt.Errorf("update provider: provider is nil")
	}
	encryptedKey, err := r.keyCipher.encrypt(ap.APIKey)
	if err != nil {
		return fmt.Errorf("encrypt provider API key: %w", err)
	}

	if ap.IsDefault {
		if _, err := conn.Exec(`UPDATE ai_providers SET is_default = 0 WHERE id != ?`, ap.ID); err != nil {
			return fmt.Errorf("clear existing defaults: %w", err)
		}
	}

	_, err = conn.Exec(
		`UPDATE ai_providers SET name = ?, base_url = ?, api_key = ?, default_model = ?, is_default = ?, version = version + 1
		 WHERE id = ?`,
		ap.Name, ap.BaseURL, encryptedKey, ap.DefaultModel, boolToInt(ap.IsDefault), ap.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("update provider %q: %w", ap.Name, ErrDuplicate)
		}
		return fmt.Errorf("update provider: %w", err)
	}
	return nil
}

// UpdateOptimistic updates a provider only when the supplied version is still
// current. It preserves the repository's encrypted-key and default semantics.
func (r *ProviderRepository) UpdateOptimistic(ap *model.AIProvider, version int64) error {
	if ap == nil {
		return fmt.Errorf("update provider: provider is nil")
	}
	if version <= 0 {
		return ErrConflict
	}
	encryptedKey, err := r.keyCipher.encrypt(ap.APIKey)
	if err != nil {
		return fmt.Errorf("encrypt provider API key: %w", err)
	}
	tx, err := r.db.Conn().Begin()
	if err != nil {
		return fmt.Errorf("begin optimistic provider update: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE ai_providers SET name = ?, base_url = ?, api_key = ?, default_model = ?, is_default = ?, version = version + 1
		 WHERE id = ? AND version = ?`,
		ap.Name, ap.BaseURL, encryptedKey, ap.DefaultModel, boolToInt(ap.IsDefault), ap.ID, version,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("update provider %q: %w", ap.Name, ErrDuplicate)
		}
		return fmt.Errorf("update provider: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update provider rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	if ap.IsDefault {
		// Clearing the previous default is part of the same optimistic-lock
		// transaction. Its version must advance as well, so clients holding
		// the old default snapshot cannot overwrite the transition.
		if _, err := tx.Exec(`UPDATE ai_providers
			SET is_default = 0, version = version + 1
			WHERE id != ? AND is_default = 1`, ap.ID); err != nil {
			return fmt.Errorf("clear existing defaults: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit optimistic provider update: %w", err)
	}
	return nil
}

// DeleteOptimistic removes a provider only when the supplied positive version
// is still current.
func (r *ProviderRepository) DeleteOptimistic(id, version int64) error {
	if version <= 0 {
		return ErrConflict
	}
	result, err := r.db.Conn().Exec(
		`DELETE FROM ai_providers WHERE id = ? AND version = ?`,
		id, version,
	)
	if err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete provider rows affected: %w", err)
	}
	if affected == 0 {
		return ErrConflict
	}
	return nil
}

// Delete removes a provider by ID. Referenced group_settings.provider_id
// will be set to NULL via ON DELETE SET NULL.
func (r *ProviderRepository) Delete(id int64) error {
	_, err := r.db.Conn().Exec(`DELETE FROM ai_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete provider: %w", err)
	}
	return nil
}
