package policy

import (
	"errors"
	"regexp"
	"strings"

	"github.com/charlesnpx/agentbus/engine"
)

// ShapeValidation is delegate's minimal markdown-shape verdict.
type ShapeValidation struct {
	Compliant  bool
	Violations []string
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// ValidateDelegateReportShape validates text against the bundled delegate-report
// markdown shape. It intentionally checks only the first line and required
// section headers; richer agentbus-only shape heuristics are not part of this
// client-side validator.
func ValidateDelegateReportShape(text string) (ShapeValidation, error) {
	spec, err := DelegateReportSpec()
	if err != nil {
		return ShapeValidation{}, err
	}
	return ValidateShape(text, spec)
}

// ValidateShape validates the minimal strict report shape delegate owns.
func ValidateShape(text string, spec engine.ContractSpec) (ShapeValidation, error) {
	if spec.Shape == nil {
		return ShapeValidation{}, errors.New("shape validation requires a shape contract")
	}
	shape := spec.Shape
	raw := ansiPattern.ReplaceAllString(text, "")
	var violations []string
	if len(shape.FirstLineEnum) > 0 && !allowedFirstLine(firstLine(raw), shape.FirstLineEnum) {
		violations = append(violations, "firstLineEnum")
	}
	for _, section := range shape.RequiredSections {
		if !hasSection(raw, section) {
			violations = append(violations, "section:"+section)
		}
	}
	return ShapeValidation{Compliant: len(violations) == 0, Violations: violations}, nil
}

// DelegateReportCorrectionPrompt builds the one client-side report-only
// correction prompt from a noncompliant prior report.
func DelegateReportCorrectionPrompt(priorBody string, violations []string) (string, error) {
	spec, err := DelegateReportSpec()
	if err != nil {
		return "", err
	}
	if spec.Shape == nil {
		return "", errors.New("delegate-report correction requires a shape contract")
	}
	var b strings.Builder
	b.WriteString("This is a report-format correction for a delegate report. The previous report body did not satisfy the required report shape.\n\n")
	b.WriteString("Violations:\n")
	for _, violation := range violations {
		b.WriteString("- ")
		b.WriteString(correctionInstruction(violation, *spec.Shape))
		b.WriteByte('\n')
	}
	b.WriteString("\nPrior report body begins after this marker and ends before the closing marker.\n")
	b.WriteString("----- BEGIN PRIOR REPORT BODY -----\n")
	b.WriteString(priorBody)
	if !strings.HasSuffix(priorBody, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("----- END PRIOR REPORT BODY -----\n\n")
	b.WriteString("Emit ONLY the corrected report. Make NO other changes. Stay read-only.")
	return b.String(), nil
}

func correctionInstruction(violation string, shape engine.ShapeSpec) string {
	if violation == "firstLineEnum" {
		return "line 1 must be exactly one of: " + strings.Join(shape.FirstLineEnum, ", ")
	}
	const sectionPrefix = "section:"
	if strings.HasPrefix(violation, sectionPrefix) {
		section := strings.TrimPrefix(violation, sectionPrefix)
		return "add a section header `# " + section + "` or `" + section + ":`"
	}
	return "fix report-shape violation `" + violation + "`"
}

func allowedFirstLine(line string, allowed []string) bool {
	for _, value := range allowed {
		if line == value {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSuffix(s[:i], "\r")
	}
	return strings.TrimSuffix(s, "\r")
}

func hasSection(text, section string) bool {
	want := strings.ToLower(strings.TrimSpace(section))
	for _, line := range splitLines(text) {
		if name, ok := headingName(line); ok && strings.ToLower(strings.TrimSpace(name)) == want {
			return true
		}
		if name, ok := labelName(line); ok && strings.ToLower(strings.TrimSpace(name)) == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func headingName(line string) (string, bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n == 0 || n > 4 || len(line) == n || (line[n] != ' ' && line[n] != '\t') {
		return "", false
	}
	name := strings.TrimSpace(line[n:])
	name = strings.TrimSpace(strings.TrimRight(name, "#"))
	return name, name != ""
}

func labelName(line string) (string, bool) {
	if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
		return "", false
	}
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", false
	}
	name := strings.TrimSpace(line[:i])
	return name, name != ""
}
