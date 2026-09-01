package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/charlesnpx/witness/contract/charter"
	reviewcontract "github.com/charlesnpx/witness/contract/review"
	"github.com/charlesnpx/witness/contract/strictjson"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDefaultReviewerSchemaMatchesConformanceManifest(t *testing.T) {
	frozen := embeddedConformanceFrozenCharter(t)
	cases, err := reviewcontract.LoadConformanceManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range cases {
		t.Run(testCase.File, func(t *testing.T) {
			schema := compileDefaultReviewerSchema(t, frozen, testCase.ExpectedInputDigest)
			data, err := reviewcontract.ConformanceFS.ReadFile("testdata/conformance/" + testCase.File)
			if err != nil {
				t.Fatal(err)
			}
			_, strictErr := strictjson.DecodeBytes[reviewcontract.ReviewReportDocument](data, strictjson.DefaultMaxBytes)
			if got, want := strictErr == nil, testCase.Strict == "pass"; got != want {
				t.Fatalf("strict decode=%t, want manifest strict outcome %q: %v", got, testCase.Strict, strictErr)
			}
			instance, decodeErr := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			var schemaErr error
			if decodeErr == nil {
				schemaErr = schema.Validate(instance)
			}
			got := "fail"
			if strictErr == nil && decodeErr == nil && schemaErr == nil {
				got = "pass"
			}
			if got != testCase.Schema {
				t.Fatalf("schema outcome=%q, want manifest schema outcome %q; strict decode error: %v; instance decode error: %v; schema validation error: %v", got, testCase.Schema, strictErr, decodeErr, schemaErr)
			}
		})
	}
}

func embeddedConformanceFrozenCharter(t *testing.T) charter.FrozenCharter {
	t.Helper()
	data, err := reviewcontract.ConformanceFS.ReadFile("testdata/conformance/charter.json")
	if err != nil {
		t.Fatal(err)
	}
	var frozen charter.FrozenCharter
	if err := json.Unmarshal(data, &frozen); err != nil {
		t.Fatalf("decode embedded conformance charter: %v", err)
	}
	return frozen
}

func compileDefaultReviewerSchema(t *testing.T, frozen charter.FrozenCharter, reviewInputDigest string) *jsonschema.Schema {
	t.Helper()
	data, err := reviewcontract.DefaultReviewerSchema(frozen, reviewInputDigest)
	if err != nil {
		t.Fatalf("DefaultReviewerSchema: %v", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode generated schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("review-report-v1.json", document); err != nil {
		t.Fatalf("add generated schema: %v", err)
	}
	schema, err := compiler.Compile("review-report-v1.json")
	if err != nil {
		t.Fatalf("compile generated schema: %v", err)
	}
	return schema
}
