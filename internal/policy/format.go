package policy

import (
	"strings"

	"github.com/charlesnpx/agentbus/engine"
)

// DelegateReportFormatBlock renders the worker-facing output format from the
// bundled delegate-report shape.
func DelegateReportFormatBlock() (string, error) {
	spec, err := DelegateReportSpec()
	if err != nil {
		return "", err
	}
	return ReportFormatBlock(spec)
}

// ReportFormatBlock renders the compact final-response format from a concrete
// shape contract.
func ReportFormatBlock(spec engine.ContractSpec) (string, error) {
	shape, err := parseReportShape(spec)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Your final response must follow this exact format.\n\n")
	b.WriteString("Line 1 must be exactly one of these lowercase words, with nothing else on the line:\n")
	for _, value := range shape.FirstLineEnum {
		b.WriteString(value)
		b.WriteByte('\n')
	}
	b.WriteString("\nThen include each of these headings, exactly as written, outside code fences.\n")
	b.WriteString("Use `# Heading` syntax. Do not use bold text, Setext underlines, or numbered\n")
	b.WriteString("headings as section headers.\n")
	for _, section := range shape.RequiredSections {
		b.WriteString("\n# ")
		b.WriteString(section)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// AppendReportFormatBlock appends the generated format block to delegate-report
// shape prompts so it is the last worker-visible instruction.
func AppendReportFormatBlock(prompt string, turnPolicy *engine.TurnPolicy) (string, error) {
	if turnPolicy == nil || turnPolicy.Contract == nil || turnPolicy.Contract.Shape == nil {
		return prompt, nil
	}
	block, err := ReportFormatBlock(*turnPolicy.Contract)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + strings.TrimRight(block, "\n"), nil
}
