package main

import "testing"

func TestReleaseRepositoryURLsRequireExplicitCompleteCandidateConfiguration(t *testing.T) {
	t.Setenv("GEOFLOW_UPDATER_TUF_METADATA_URL", "")
	t.Setenv("GEOFLOW_UPDATER_TUF_TARGETS_URL", "")
	t.Setenv("GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY", "")

	metadata, targets, err := releaseRepositoryURLs()
	if err != nil || metadata != defaultMetadataURL || targets != defaultTargetsURL {
		t.Fatalf("default URLs = %q, %q, %v", metadata, targets, err)
	}

	t.Setenv("GEOFLOW_UPDATER_TUF_METADATA_URL", "https://candidate.example/metadata")
	if _, _, err := releaseRepositoryURLs(); err == nil {
		t.Fatal("partial candidate repository configuration was accepted")
	}

	t.Setenv("GEOFLOW_UPDATER_TUF_TARGETS_URL", "https://candidate.example/targets")
	if _, _, err := releaseRepositoryURLs(); err == nil {
		t.Fatal("candidate repository without explicit opt-in was accepted")
	}

	t.Setenv("GEOFLOW_UPDATER_ALLOW_CANDIDATE_REPOSITORY", "1")
	metadata, targets, err = releaseRepositoryURLs()
	if err != nil || metadata != "https://candidate.example/metadata" || targets != "https://candidate.example/targets" {
		t.Fatalf("candidate URLs = %q, %q, %v", metadata, targets, err)
	}

	t.Setenv("GEOFLOW_UPDATER_TUF_TARGETS_URL", "https://other.example/targets")
	if _, _, err := releaseRepositoryURLs(); err == nil {
		t.Fatal("cross-origin candidate repository was accepted")
	}
}
