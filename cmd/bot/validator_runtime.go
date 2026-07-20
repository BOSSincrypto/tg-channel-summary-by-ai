package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	validatorRunDBPrefix = "tg-channel-summary-by-ai-validator-"
	validatorOwnerEnv    = "VALIDATOR_OWNER_FILE"
	validatorTokenEnv    = "VALIDATOR_RUN_TOKEN"
)

type validatorRunDatabase struct {
	path    string
	cleanup func() error
}

// newValidatorRunDatabase creates a fresh SQLite file for one validator
// process. The configured path is used only to select the temporary
// directory, never as a reusable database location.
func newValidatorRunDatabase(configuredPath string) (*validatorRunDatabase, error) {
	if strings.TrimSpace(configuredPath) == "" {
		return nil, errors.New("validator database path is empty")
	}
	directory := filepath.Dir(configuredPath)
	file, err := os.CreateTemp(directory, validatorRunDBPrefix+"*.sqlite")
	if err != nil {
		return nil, fmt.Errorf("create validator database: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = removeValidatorDatabaseFiles(path)
		return nil, fmt.Errorf("close validator database placeholder: %w", err)
	}

	return &validatorRunDatabase{
		path: path,
		cleanup: func() error {
			return removeValidatorDatabaseFiles(path)
		},
	}, nil
}

func removeValidatorDatabaseFiles(path string) error {
	var errs []error
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", candidate, err))
		}
	}
	return errors.Join(errs...)
}

type validatorListenerOwner struct {
	path   string
	token  string
	dbPath string
	held   bool
}

type validatorListenerOwnerRecord struct {
	Mode      string `json:"mode"`
	PID       int    `json:"pid"`
	Token     string `json:"token"`
	DBPath    string `json:"db_path"`
	StartedAt string `json:"started_at"`
}

func newValidatorListenerOwner(dbPath string) (*validatorListenerOwner, error) {
	return newValidatorListenerOwnerForRun(dbPath, dbPath)
}

func newValidatorListenerOwnerForRun(ownerKey, dbPath string) (*validatorListenerOwner, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, errors.New("validator listener owner requires a database path")
	}
	if strings.TrimSpace(ownerKey) == "" {
		return nil, errors.New("validator listener owner requires an owner key")
	}
	ownerPath := strings.TrimSpace(os.Getenv(validatorOwnerEnv))
	if ownerPath == "" {
		ownerPath = ownerKey + ".owner.json"
	}
	ownerPath, err := filepath.Abs(filepath.Clean(ownerPath))
	if err != nil {
		return nil, fmt.Errorf("resolve validator listener owner file: %w", err)
	}
	tempDir, err := filepath.Abs(filepath.Clean(os.TempDir()))
	if err != nil {
		return nil, fmt.Errorf("resolve validator temporary directory: %w", err)
	}
	relative, err := filepath.Rel(tempDir, ownerPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("validator listener owner file must be inside %s", tempDir)
	}
	token := strings.TrimSpace(os.Getenv(validatorTokenEnv))
	if token == "" {
		token = filepath.Base(dbPath)
	}
	return &validatorListenerOwner{
		path:   ownerPath,
		token:  token,
		dbPath: dbPath,
	}, nil
}

// Claim records ownership only after the HTTP listener has successfully
// bound its port. O_EXCL prevents a new run from overwriting live ownership
// evidence; records from prior runs are replaced only after validation.
func (o *validatorListenerOwner) Claim() error {
	if o == nil {
		return errors.New("validator listener owner is nil")
	}
	record, err := o.record()
	if err != nil {
		return fmt.Errorf("encode validator listener ownership: %w", err)
	}
	file, err := o.createRecordFile()
	if os.IsExist(err) {
		existing, readErr := readValidatorListenerOwner(o.path)
		if readErr != nil {
			return fmt.Errorf("inspect existing validator listener ownership: %w", readErr)
		}
		if existing.PID == os.Getpid() {
			return fmt.Errorf("validator listener ownership at %s is already held by this process", o.path)
		}
		if existing.Mode != "validator_http_only" || !isValidatorRunDatabasePath(existing.DBPath) {
			return fmt.Errorf("validator listener ownership at %s is not a stale validator record", o.path)
		}
		if validatorProcessAlive(existing.PID) {
			return fmt.Errorf("validator listener ownership at %s is held by active process %d", o.path, existing.PID)
		}
		if removeErr := os.Remove(o.path); removeErr != nil {
			return fmt.Errorf("remove stale validator listener ownership: %w", removeErr)
		}
		file, err = o.createRecordFile()
	}
	if err != nil {
		return fmt.Errorf("claim validator listener ownership at %s: %w", o.path, err)
	}
	if _, err := file.Write(record); err != nil {
		_ = file.Close()
		_ = os.Remove(o.path)
		return fmt.Errorf("write validator listener ownership: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(o.path)
		return fmt.Errorf("sync validator listener ownership: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(o.path)
		return fmt.Errorf("close validator listener ownership: %w", err)
	}
	o.held = true
	return nil
}

func (o *validatorListenerOwner) Release() error {
	if o == nil || !o.held {
		return nil
	}
	record, err := readValidatorListenerOwner(o.path)
	if errors.Is(err, os.ErrNotExist) {
		o.held = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect validator listener ownership before release: %w", err)
	}
	if record.PID != os.Getpid() || record.Token != o.token || record.DBPath != o.dbPath {
		o.held = false
		return nil
	}
	if err := os.Remove(o.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("release validator listener ownership at %s: %w", o.path, err)
	}
	o.held = false
	return nil
}

func (o *validatorListenerOwner) record() ([]byte, error) {
	return json.Marshal(validatorListenerOwnerRecord{
		Mode:      "validator_http_only",
		PID:       os.Getpid(),
		Token:     o.token,
		DBPath:    o.dbPath,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (o *validatorListenerOwner) createRecordFile() (*os.File, error) {
	file, err := os.OpenFile(o.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func readValidatorListenerOwner(path string) (validatorListenerOwnerRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return validatorListenerOwnerRecord{}, err
	}
	var record validatorListenerOwnerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return validatorListenerOwnerRecord{}, fmt.Errorf("decode owner record: %w", err)
	}
	return record, nil
}

func isValidatorRunDatabasePath(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	tempDir, err := filepath.Abs(filepath.Clean(os.TempDir()))
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(tempDir, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return strings.HasPrefix(filepath.Base(absolute), validatorRunDBPrefix)
}
