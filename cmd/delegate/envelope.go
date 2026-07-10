package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/charlesnpx/agentbus/engine"
)

const (
	envelopeSchema = 1
	taskKind       = "task"

	contractKindShape      = "shape"
	contractKindJSONSchema = "jsonSchema"
	contractKindNone       = "none"
)

// LaunchEnvelope is the schema returned when delegate has launched a job.
type LaunchEnvelope struct {
	Schema       int     `json:"schema"`
	JobID        string  `json:"job_id"`
	Status       string  `json:"status"`
	ResultSHA256 *string `json:"result_sha256"`
	SHA256       string  `json:"sha256"`
}

// TerminalEnvelope is the schema returned by delegate result and task --wait.
type TerminalEnvelope struct {
	Schema       int                  `json:"schema"`
	JobID        string               `json:"job_id"`
	Status       engine.JobState      `json:"status"`
	Kind         string               `json:"kind"`
	ContractKind string               `json:"contractKind"`
	Contract     engine.ContractStamp `json:"contract"`
	ResultSHA256 string               `json:"result_sha256"`
	SHA256       string               `json:"sha256"`
}

func (e TerminalEnvelope) MarshalJSON() ([]byte, error) {
	type terminalEnvelopeJSON struct {
		Schema       int             `json:"schema"`
		JobID        string          `json:"job_id"`
		Status       engine.JobState `json:"status"`
		Kind         string          `json:"kind"`
		ContractKind string          `json:"contractKind"`
		Contract     map[string]any  `json:"contract"`
		ResultSHA256 string          `json:"result_sha256"`
		SHA256       string          `json:"sha256"`
	}
	return json.Marshal(terminalEnvelopeJSON{
		Schema:       e.Schema,
		JobID:        e.JobID,
		Status:       e.Status,
		Kind:         e.Kind,
		ContractKind: e.ContractKind,
		Contract:     contractStampEnvelopeValue(e.Contract),
		ResultSHA256: e.ResultSHA256,
		SHA256:       e.SHA256,
	})
}

func contractStampEnvelopeValue(stamp engine.ContractStamp) map[string]any {
	stamp = normalizeContractStamp(stamp)
	out := map[string]any{
		"status":    stamp.Status,
		"missing":   stamp.Missing,
		"reason":    stamp.Reason,
		"attempts":  stamp.Attempts,
		"retryUsed": stamp.RetryUsed,
	}
	if stamp.ContractName != "" {
		out["contractName"] = stamp.ContractName
	}
	if stamp.ContractSHA256 != "" {
		out["contractSha256"] = stamp.ContractSHA256
	}
	if !stamp.ValidatedAt.IsZero() {
		out["validatedAt"] = stamp.ValidatedAt
	}
	return out
}

func newLaunchEnvelope(jobID string, state engine.JobState) (LaunchEnvelope, error) {
	env := LaunchEnvelope{
		Schema: envelopeSchema,
		JobID:  jobID,
		Status: launchStatus(state),
	}
	sum, err := envelopeSHA256(env)
	if err != nil {
		return LaunchEnvelope{}, err
	}
	env.SHA256 = sum
	return env, nil
}

func newTerminalEnvelope(jobID string, state engine.JobState, kind, contractKind string, stamp engine.ContractStamp, resultSHA256 string) (TerminalEnvelope, error) {
	if resultSHA256 == "" {
		return TerminalEnvelope{}, fmt.Errorf("terminal result for %s is missing result sha256", jobID)
	}
	stamp = normalizeContractStamp(stamp)
	env := TerminalEnvelope{
		Schema:       envelopeSchema,
		JobID:        jobID,
		Status:       state,
		Kind:         kind,
		ContractKind: contractKind,
		Contract:     stamp,
		ResultSHA256: resultSHA256,
	}
	sum, err := envelopeSHA256(env)
	if err != nil {
		return TerminalEnvelope{}, err
	}
	env.SHA256 = sum
	return env, nil
}

func launchStatus(state engine.JobState) string {
	if state == engine.StateQueued {
		return string(engine.StateQueued)
	}
	return string(engine.StateRunning)
}

func normalizeContractStamp(stamp engine.ContractStamp) engine.ContractStamp {
	if stamp.Missing == nil {
		stamp.Missing = []string{}
	}
	return stamp
}

func envelopeSHA256(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return "", err
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		return "", fmt.Errorf("envelope root must be an object")
	}
	delete(obj, "sha256")
	canonical, err := canonicalJSON(obj)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonicalJSON(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonicalJSON(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(raw)
	case json.Number:
		if _, err := x.Int64(); err != nil {
			if _, floatErr := x.Float64(); floatErr != nil {
				return fmt.Errorf("invalid JSON number %q", x)
			}
		}
		buf.WriteString(x.String())
	case float64:
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(raw)
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalJSON(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			rawKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			buf.Write(rawKey)
			buf.WriteByte(':')
			if err := writeCanonicalJSON(buf, x[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return err
		}
		var decoded any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		return writeCanonicalJSON(buf, decoded)
	}
	return nil
}

func writeJSONLine(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = w.Write(raw)
	return err
}
