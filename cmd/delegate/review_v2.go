package main

import (
	"encoding/json"

	"github.com/charlesnpx/witness/contract/charter"
	reviewcontract "github.com/charlesnpx/witness/contract/review"
)

// reviewReportV2Schema returns the generic v2 report boundary with the
// request-specific bindings supplied as JSON Schema const values. Reviewer
// identifiers remain data, so adding one to a recipe does not require a code
// change here.
func reviewReportV2Schema(frozen charter.FrozenCharter, requestDigest, recipeDigest, reviewer, reviewInputDigest string, consumer reviewcontract.Identity) (json.RawMessage, error) {
	goalIDs := make([]string, len(frozen.Charter.Goals))
	for index, goal := range frozen.Charter.Goals {
		goalIDs[index] = goal.ID
	}

	properties := map[string]any{
		"schema_version":         map[string]any{"const": reviewcontract.ReviewReportV2},
		"request_digest":         map[string]any{"const": requestDigest},
		"recipe_digest":          map[string]any{"const": recipeDigest},
		"reviewer":               map[string]any{"const": reviewer},
		"charter_hash":           map[string]any{"const": frozen.CharterHash},
		"review_input_digest":    map[string]any{"const": reviewInputDigest},
		"source_identity":        reviewIdentitySchema(),
		"consumer_identity":      reviewConsumerIdentitySchema(consumer),
		"findings":               reviewV2FindingsSchema(goalIDs),
		"evaluation":             reviewEvaluationSchema(goalIDs),
		"missing_goal_questions": reviewMissingGoalQuestionsSchema(),
	}
	return json.Marshal(map[string]any{
		"type":                 "object",
		"required":             []string{"schema_version", "request_digest", "recipe_digest", "reviewer", "charter_hash", "review_input_digest", "source_identity", "consumer_identity", "findings", "evaluation"},
		"properties":           properties,
		"additionalProperties": false,
	})
}

func reviewIdentitySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "id"},
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "minLength": 1, "pattern": "\\S"},
			"id":   map[string]any{"type": "string", "minLength": 1, "pattern": "\\S"},
		},
		"additionalProperties": false,
	}
}

func reviewConsumerIdentitySchema(consumer reviewcontract.Identity) map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "id"},
		"properties": map[string]any{
			"kind": map[string]any{"const": consumer.Kind},
			"id":   map[string]any{"const": consumer.ID},
		},
		"additionalProperties": false,
	}
}

func reviewV2FindingsSchema(goalIDs []string) map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":     "object",
			"required": []string{"id", "kind", "title", "claimed_severity", "attribution", "charter_goal_ids", "witness"},
			"properties": map[string]any{
				"id":                  map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]*$"},
				"kind":                map[string]any{"enum": []string{reviewcontract.FindingKindDefect, reviewcontract.FindingKindEconomy}},
				"title":               map[string]any{"type": "string", "minLength": 1, "maxLength": 8192},
				"claimed_severity":    map[string]any{"enum": []string{reviewcontract.SeverityCritical, reviewcontract.SeverityHigh, reviewcontract.SeverityMedium, reviewcontract.SeverityLow}},
				"attribution":         map[string]any{"enum": []string{reviewcontract.FindingAttributionIntroduced, reviewcontract.FindingAttributionWorsened, reviewcontract.FindingAttributionPreExisting, reviewcontract.FindingAttributionUnattributed}},
				"charter_goal_ids":    map[string]any{"type": "array", "items": map[string]any{"enum": goalIDs}},
				"witness":             reviewV2WitnessSchema(),
				"annotation":          reviewAnnotationSchema(),
				"remedy":              reviewRemedySchema(),
				"economy_equivalence": reviewEconomyEquivalenceSchema(goalIDs),
			},
			"additionalProperties": false,
		},
	}
}

func reviewV2WitnessSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "strength", "content"},
		"properties": map[string]any{
			"kind":     map[string]any{"enum": []string{reviewcontract.WitnessKindDefect, reviewcontract.WitnessKindEquivalence}},
			"strength": map[string]any{"enum": []string{reviewcontract.WitnessStrengthExecutable, reviewcontract.WitnessStrengthConstructed, reviewcontract.WitnessStrengthArgued}},
			"content":  map[string]any{"type": "string", "minLength": 1},
			"executable": map[string]any{
				"type":     "object",
				"required": []string{"argv", "cwd", "expected_observation"},
				"properties": map[string]any{
					"argv":                 map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}},
					"cwd":                  map[string]any{"type": "string", "minLength": 1},
					"expected_observation": map[string]any{"type": "string", "minLength": 1},
					"transformation_ref":   reviewArtifactRefSchema(),
				},
				"additionalProperties": false,
			},
		},
		"additionalProperties": false,
	}
}

func reviewArtifactRefSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"kind", "id", "digest"},
		"properties": map[string]any{
			"kind":           map[string]any{"type": "string", "minLength": 1},
			"id":             map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]*$"},
			"digest":         map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"},
			"digest_profile": map[string]any{"const": "relay-root-digests-v1"},
			"media_type":     map[string]any{"type": "string"},
		},
		"additionalProperties": false,
	}
}

func reviewAnnotationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":     map[string]any{"type": "string", "minLength": 1},
			"line":     map[string]any{"type": "integer"},
			"category": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]*$"},
		},
		"additionalProperties": false,
	}
}

func reviewRemedySchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"direction", "summary", "minimality_argument"},
		"properties": map[string]any{
			"direction":           map[string]any{"enum": []string{reviewcontract.RemedyDirectionAdd, reviewcontract.RemedyDirectionChange, reviewcontract.RemedyDirectionRemove}},
			"summary":             map[string]any{"type": "string", "minLength": 1},
			"minimality_argument": map[string]any{"type": "string", "minLength": 1},
		},
		"additionalProperties": false,
	}
}

func reviewEconomyEquivalenceSchema(goalIDs []string) map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"preserved_behavior", "charter_goal_ids"},
		"properties": map[string]any{
			"preserved_behavior": map[string]any{"type": "string", "minLength": 1},
			"charter_goal_ids":   map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"enum": goalIDs}},
		},
		"additionalProperties": false,
	}
}

func reviewEvaluationSchema(goalIDs []string) map[string]any {
	goalIDsSchema := map[string]any{"type": "array", "items": map[string]any{"enum": goalIDs}}
	if len(goalIDs) > 0 {
		goalIDsSchema["minItems"] = 1
	}
	return map[string]any{
		"type":     "object",
		"required": []string{"evaluated_paths", "evaluated_goal_ids"},
		"properties": map[string]any{
			"evaluated_paths":    map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}},
			"evaluated_goal_ids": goalIDsSchema,
		},
		"additionalProperties": false,
	}
}

func reviewMissingGoalQuestionsSchema() map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":     "object",
			"required": []string{"id", "finding_id", "dimension", "anchor_index", "property", "affected_decision", "statement"},
			"properties": map[string]any{
				"id":                map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]*$"},
				"finding_id":        map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]*$"},
				"dimension":         map[string]any{"enum": []string{charter.DimensionEntryPoints, charter.DimensionInputSurface, charter.DimensionValidStates, charter.DimensionEnvironments, charter.DimensionScaleBounds, charter.DimensionCompatibilityPromises, charter.DimensionThreatModel}},
				"anchor_index":      map[string]any{"type": "integer"},
				"property":          map[string]any{"type": "string", "minLength": 1},
				"value":             map[string]any{"type": "string"},
				"affected_decision": map[string]any{"type": "string", "minLength": 1},
				"statement":         map[string]any{"type": "string", "minLength": 1},
			},
			"additionalProperties": false,
		},
	}
}
