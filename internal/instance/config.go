package instance

import "time"

type Config struct {
	SchemaVersion   int       `yaml:"schema_version" json:"schema_version"`
	ID              string    `yaml:"id" json:"id"`
	Root            string    `yaml:"root" json:"root"`
	ComposeFile     string    `yaml:"compose_file" json:"compose_file"`
	EnvironmentFile string    `yaml:"environment_file" json:"environment_file"`
	ControlToken    string    `yaml:"control_token_file" json:"control_token_file"`
	ReleaseSequence uint64    `yaml:"release_sequence" json:"release_sequence"`
	Version         string    `yaml:"version" json:"version"`
	PostgresMajor   string    `yaml:"postgres_major" json:"postgres_major"`
	PostgresDataDir string    `yaml:"postgres_data_dir" json:"postgres_data_dir"`
	PostgresMount   string    `yaml:"postgres_container_data_dir" json:"postgres_container_data_dir"`
	RedisMajor      string    `yaml:"redis_major" json:"redis_major"`
	EnrolledAt      time.Time `yaml:"enrolled_at" json:"enrolled_at"`
}
