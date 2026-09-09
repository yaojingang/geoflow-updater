package instance

import (
	"path/filepath"
	"time"
)

type Config struct {
	Layout                string    `yaml:"layout,omitempty" json:"layout,omitempty"`
	ActiveSlot            string    `yaml:"active_slot,omitempty" json:"active_slot,omitempty"`
	InfraComposeFile      string    `yaml:"infra_compose_file,omitempty" json:"infra_compose_file,omitempty"`
	InfraEnvironmentFile  string    `yaml:"infra_environment_file,omitempty" json:"infra_environment_file,omitempty"`
	SchemaVersion         int       `yaml:"schema_version" json:"schema_version"`
	ID                    string    `yaml:"id" json:"id"`
	Root                  string    `yaml:"root" json:"root"`
	ComposeFile           string    `yaml:"compose_file" json:"compose_file"`
	EnvironmentFile       string    `yaml:"environment_file" json:"environment_file"`
	ControlToken          string    `yaml:"control_token_file" json:"control_token_file"`
	ReleaseSequence       uint64    `yaml:"release_sequence" json:"release_sequence"`
	EnrolledReleaseSHA256 string    `yaml:"enrolled_release_sha256,omitempty" json:"enrolled_release_sha256,omitempty"`
	Version               string    `yaml:"version" json:"version"`
	PostgresMajor         string    `yaml:"postgres_major" json:"postgres_major"`
	PostgresDataDir       string    `yaml:"postgres_data_dir" json:"postgres_data_dir"`
	PostgresMount         string    `yaml:"postgres_container_data_dir" json:"postgres_container_data_dir"`
	RedisMajor            string    `yaml:"redis_major" json:"redis_major"`
	EnrolledAt            time.Time `yaml:"enrolled_at" json:"enrolled_at"`
}

// StateDirectory remains stable when the active Compose file belongs to a slot.
func (config Config) StateDirectory() string {
	if config.ControlToken != "" {
		return filepath.Dir(config.ControlToken)
	}
	return filepath.Dir(config.ComposeFile)
}
