package managed_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yaojingang/geoflow-updater/internal/managed"
)

const reviewedPlan = `{"schema_version":1,"strategy":"maintenance","allowed_sources":[],"migrations":[{"name":"2026_09_08_000001_example","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","online":false}],"compatibility":{"schema":false,"queue":false,"cache":false,"storage":false},"steps":[{"id":"migrate","kind":"migrate","phase":"apply","timeout_seconds":600,"online":false}]}`

func TestUpgradePlanRequiresTheExactApplicationJSONContract(t *testing.T) {
	t.Parallel()
	for _, online := range []bool{false, true} {
		contents := reviewedPlan
		if online {
			contents = strings.Replace(contents, `"maintenance"`, `"online"`, 1)
			contents = strings.Replace(contents, `"allowed_sources":[]`, `"allowed_sources":[17]`, 1)
			contents = strings.ReplaceAll(contents, "false", "true")
		}
		if _, err := managed.DecodeUpgradePlan([]byte(contents)); err != nil {
			t.Fatalf("valid plan online=%t rejected: %v", online, err)
		}
	}
	for _, scope := range []string{"plan", "compatibility", "migration", "step"} {
		var original map[string]any
		if err := json.Unmarshal([]byte(reviewedPlan), &original); err != nil {
			t.Fatal(err)
		}
		object := planObject(original, scope)
		for key := range object {
			for _, change := range []string{"missing", "null", "case-alias"} {
				t.Run(scope+"/"+key+"/"+change, func(t *testing.T) {
					var plan map[string]any
					if err := json.Unmarshal([]byte(reviewedPlan), &plan); err != nil {
						t.Fatal(err)
					}
					fields := planObject(plan, scope)
					switch change {
					case "missing":
						delete(fields, key)
					case "null":
						fields[key] = nil
					case "case-alias":
						fields[strings.ToUpper(key)] = fields[key]
						delete(fields, key)
					}
					contents, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := managed.DecodeUpgradePlan(contents); err == nil {
						t.Fatal("invalid wire contract accepted")
					}
				})
			}
		}
	}
	for name, contents := range map[string]string{
		"duplicate-top-field":        strings.Replace(reviewedPlan, `"strategy":"maintenance"`, `"strategy":"online","strategy":"maintenance"`, 1),
		"duplicate-compatibility":    strings.Replace(reviewedPlan, `"schema":false`, `"schema":true,"schema":false`, 1),
		"duplicate-migration-field":  strings.Replace(reviewedPlan, `"online":false`, `"online":true,"online":false`, 1),
		"duplicate-step-field":       strings.Replace(reviewedPlan, `"timeout_seconds":600`, `"timeout_seconds":10,"timeout_seconds":600`, 1),
		"compatibility-as-array":     strings.Replace(reviewedPlan, `{"schema":false,"queue":false,"cache":false,"storage":false}`, `[]`, 1),
		"boolean-as-string":          strings.ReplaceAll(reviewedPlan, "false", `"false"`),
		"boolean-as-number":          strings.ReplaceAll(reviewedPlan, "false", `0`),
		"fractional-timeout":         strings.Replace(reviewedPlan, `"timeout_seconds":600`, `"timeout_seconds":600.0`, 1),
		"null-source":                strings.Replace(reviewedPlan, `"allowed_sources":[]`, `"allowed_sources":[null]`, 1),
		"source-exceeds-php-integer": strings.Replace(reviewedPlan, `"allowed_sources":[]`, `"allowed_sources":[9223372036854775808]`, 1),
		"null-migration":             strings.Replace(reviewedPlan, `"migrations":[`, `"migrations":[null,`, 1),
		"null-step":                  strings.Replace(reviewedPlan, `"steps":[`, `"steps":[null,`, 1),
		"trailing-object":            reviewedPlan + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := managed.DecodeUpgradePlan([]byte(contents)); err == nil {
				t.Fatal("invalid JSON contract accepted")
			}
		})
	}
}

func planObject(plan map[string]any, scope string) map[string]any {
	switch scope {
	case "compatibility":
		return plan["compatibility"].(map[string]any)
	case "migration":
		return plan["migrations"].([]any)[0].(map[string]any)
	case "step":
		return plan["steps"].([]any)[0].(map[string]any)
	default:
		return plan
	}
}
