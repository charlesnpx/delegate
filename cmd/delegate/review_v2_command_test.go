package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesnpx/agentbus/client"
	"github.com/charlesnpx/agentbus/engine"
	"github.com/charlesnpx/witness/contract/charter"
	reviewcontract "github.com/charlesnpx/witness/contract/review"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestContractReviewV2BindsSubmittedSchemaAndPrompt(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_contract_review_v2",
			State:   publicStateQueued,
			Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewV2Fixture(t)
	var stdout, stderr bytes.Buffer
	args := append(contractReviewV2Args(fixture, t.TempDir()), "--reviewer", fixture.reviewers[0])
	if code := run(args, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("contract v2 review code=%d stderr=%q", code, stderr.String())
	}
	assertTaskReceiptShape(t, stdout.Bytes(), "job_contract_review_v2")
	if len(fake.submits) != 1 {
		t.Fatalf("submits=%d, want 1", len(fake.submits))
	}
	spec := fake.submits[0].TaskSpec
	var schema map[string]any
	if err := json.Unmarshal(spec.OutputSchema, &schema); err != nil {
		t.Fatalf("v2 schema is invalid JSON: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(spec.OutputSchema))
	if err != nil {
		t.Fatalf("decode v2 schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("review-report-v2.json", document); err != nil {
		t.Fatalf("add v2 schema: %v", err)
	}
	if _, err := compiler.Compile("review-report-v2.json"); err != nil {
		t.Fatalf("compile v2 schema: %v", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("v2 schema properties=%#v", schema["properties"])
	}
	for field, want := range map[string]string{
		"schema_version":      reviewcontract.ReviewReportV2,
		"request_digest":      fixture.requestDigest,
		"recipe_digest":       fixture.recipeDigest,
		"reviewer":            fixture.reviewers[0],
		"charter_hash":        fixture.frozen.CharterHash,
		"review_input_digest": fixture.reviewInputDigest,
	} {
		property, ok := properties[field].(map[string]any)
		if !ok || property["const"] != want {
			t.Fatalf("schema %s=%#v, want const %q", field, properties[field], want)
		}
	}
	for _, want := range []string{
		fixture.recipe.Instructions,
		"allow_unbound_findings",
		fixture.requestDigest,
		fixture.recipeDigest,
		fixture.reviewers[0],
		"EXACTLY ONE review-report-v2 JSON object",
	} {
		if !strings.Contains(spec.Prompt, want) {
			t.Fatalf("v2 prompt missing %q:\n%s", want, spec.Prompt)
		}
	}
	if strings.Contains(spec.Prompt, "review-report-v1") {
		t.Fatalf("v2 prompt contains v1 report wording: %q", spec.Prompt)
	}
}

func TestContractReviewV2RejectsUndeclaredReviewer(t *testing.T) {
	fake := &fakeAgentbusClient{hello: helloWithBackends()}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewV2Fixture(t)
	var stdout, stderr bytes.Buffer
	args := append(contractReviewV2Args(fixture, t.TempDir()), "--reviewer", "reviewer-not-declared")
	if code := run(args, nil, &stdout, &stderr); code == 0 {
		t.Fatal("undeclared v2 reviewer unexpectedly succeeded")
	}
	for _, want := range []string{"reviewer-not-declared", fixture.reviewers[0], fixture.reviewers[1]} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr=%q, want %q", stderr.String(), want)
		}
	}
	if len(fake.submits) != 0 {
		t.Fatalf("submits=%d, want 0", len(fake.submits))
	}
}

func TestContractReviewV2ReviewersHaveDistinctReplayIdentities(t *testing.T) {
	fake := &fakeAgentbusClient{
		hello: helloWithBackends(),
		submitResult: client.JobSubmitResult{
			JobID:   "job_contract_review_v2_a",
			State:   publicStateQueued,
			Timeout: &engine.TimeoutResolution{Effective: 1800000, Source: engine.TimeoutSourceDaemonDefault},
		},
	}
	restore := stubAgentbusGlobals(t, fake)
	defer restore()

	fixture := newContractReviewV2Fixture(t)
	cwd := t.TempDir()
	for index, reviewer := range fixture.reviewers {
		if index == 1 {
			fake.submitResult.JobID = "job_contract_review_v2_b"
		}
		var stdout, stderr bytes.Buffer
		args := append(contractReviewV2Args(fixture, cwd), "--reviewer", reviewer)
		if code := run(args, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("reviewer %q code=%d stderr=%q", reviewer, code, stderr.String())
		}
		assertTaskReceiptShape(t, stdout.Bytes(), fake.submitResult.JobID)
	}
	if len(fake.submits) != len(fixture.reviewers) {
		t.Fatalf("submits=%d, want %d", len(fake.submits), len(fixture.reviewers))
	}
	for index, reviewer := range fixture.reviewers {
		identity := sha256.Sum256([]byte(fixture.requestDigest + "\x00" + reviewer))
		want := "delegate-review-" + hex.EncodeToString(identity[:])[:32]
		if got := fake.submits[index].RequestID; got != want {
			t.Fatalf("reviewer %q request ID=%q, want %q", reviewer, got, want)
		}
	}
	if fake.submits[0].RequestID == fake.submits[1].RequestID {
		t.Fatalf("reviewers collided on request ID %q", fake.submits[0].RequestID)
	}
}

type contractReviewV2Fixture struct {
	requestPath       string
	artifactPath      string
	charterPath       string
	artifact          []byte
	frozen            charter.FrozenCharter
	requestDigest     string
	reviewInputDigest string
	recipe            reviewcontract.ReviewRecipe
	recipeDigest      string
	reviewers         []string
}

func newContractReviewV2Fixture(t *testing.T) contractReviewV2Fixture {
	t.Helper()
	inputCharter, ok := charter.InitTemplate(charter.TemplateDeltaReview, "delegate-test", "event-test", "Delegate v2 contract test fixture.")
	if !ok {
		t.Fatal("delta-review charter template is unavailable")
	}
	frozen, err := charter.Freeze(inputCharter, nil)
	if err != nil {
		t.Fatalf("freeze fixture charter: %v", err)
	}
	charterData, err := json.Marshal(inputCharter)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("diff --git a/subject.go b/subject.go\n+CALLER_FROZEN_V2_INPUT=delegate-must-carry\n")
	artifactSum := sha256.Sum256(artifact)
	reviewInputDigest := "sha256:" + hex.EncodeToString(artifactSum[:])
	recipe := reviewcontract.ReviewRecipe{
		RecipeID:        "delegate-review-v2",
		Instructions:    "Inspect the supplied change for defects and economy, then report only evidence supported by the frozen input.",
		RequiredOutputs: []string{"reviewer-alpha", "reviewer-beta"},
		Policy: map[string]any{
			"allow_unbound_findings": true,
			"severity_cap":           "evidence",
		},
	}
	recipeData, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	recipeDigest, err := reviewcontract.ReviewRecipeDigest(recipeData)
	if err != nil {
		t.Fatalf("digest fixture recipe: %v", err)
	}
	request := reviewcontract.ReviewRequestV2Document{
		SchemaVersion: reviewcontract.ReviewRequestV2,
		ConsumerIdentity: reviewcontract.Identity{
			Kind: "delegate",
			ID:   "delegate-contract-v2-test",
		},
		Subject:           reviewcontract.RequestSubject{Head: "frozen-v2-subject-head"},
		CharterHash:       frozen.CharterHash,
		ReviewInputDigest: reviewInputDigest,
		FrozenRecipe:      recipeData,
		RecipeDigest:      recipeDigest,
		Adapter:           "delegate-test-adapter",
		RequiredOutputs:   append([]string(nil), recipe.RequiredOutputs...),
	}
	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest, err := reviewcontract.ReviewRequestV2Digest(request)
	if err != nil {
		t.Fatalf("digest fixture request: %v", err)
	}
	dir := t.TempDir()
	fixture := contractReviewV2Fixture{
		requestPath:       filepath.Join(dir, "request.json"),
		artifactPath:      filepath.Join(dir, "artifact.patch"),
		charterPath:       filepath.Join(dir, "charter.json"),
		artifact:          artifact,
		frozen:            frozen,
		requestDigest:     requestDigest,
		reviewInputDigest: reviewInputDigest,
		recipe:            recipe,
		recipeDigest:      recipeDigest,
		reviewers:         append([]string(nil), recipe.RequiredOutputs...),
	}
	for _, file := range []struct {
		path string
		data []byte
	}{
		{path: fixture.requestPath, data: requestData},
		{path: fixture.artifactPath, data: artifact},
		{path: fixture.charterPath, data: charterData},
	} {
		if err := os.WriteFile(file.path, file.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func contractReviewV2Args(fixture contractReviewV2Fixture, cwd string) []string {
	return []string{
		"review", "--backend", "codex", "--cwd", cwd,
		"--request-file", fixture.requestPath,
		"--artifact-file", fixture.artifactPath,
		"--charter-file", fixture.charterPath,
		"--model", "review-model", "--effort", "review-effort",
	}
}
