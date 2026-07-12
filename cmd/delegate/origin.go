package main

import (
	"os"
	"strconv"
)

const (
	parentClientTag    = "delegate.parent.client"
	parentSessionTag   = "delegate.parent.session_id"
	parentAgentTag     = "delegate.parent.agent"
	delegateDepthTag   = "delegate.depth"
	delegateOriginTag  = "delegate.origin"
	claudeParentClient = "claude-code"
)

// envelopeOrigin links a delegated job to the session that launched it.
// Individual fields are best-effort and omitted when unavailable.
type envelopeOrigin struct {
	Skill           string `json:"skill,omitempty"`
	ParentClient    string `json:"parent_client,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	ParentAgent     string `json:"parent_agent,omitempty"`
	Depth           string `json:"depth,omitempty"`
}

func (o envelopeOrigin) empty() bool {
	return o.Skill == "" && o.ParentClient == "" && o.ParentSessionID == "" && o.ParentAgent == "" && o.Depth == ""
}

func envelopeOriginPointer(origin envelopeOrigin) *envelopeOrigin {
	if origin.empty() {
		return nil
	}
	return &origin
}

func taskEnvelopeOrigin(opts taskOptions) envelopeOrigin {
	origin := opts.AuditOrigin
	if origin.Skill == "" {
		origin.Skill = opts.Origin
	}
	return origin
}

func captureTaskOrigin(skill string, parentClient, parentSession optionalStringFlag, getenv func(string) string) envelopeOrigin {
	if getenv == nil {
		getenv = os.Getenv
	}
	client := ""
	if getenv("CLAUDECODE") == "1" {
		client = claudeParentClient
	}
	if parentClient.set {
		client = parentClient.value
	}
	sessionID := getenv("CLAUDE_CODE_SESSION_ID")
	if parentSession.set {
		sessionID = parentSession.value
	}

	// No Codex-specific environment value is read here: neither this repository
	// nor its vendored adapter exposes a reliable offline parent-session signal.
	// Do not guess one; callers can use the explicit parent override flags.
	// Depth is an audit tag only; propagating it into spawned backends is out of
	// scope until agentbus provides an environment-injection mechanism.
	return envelopeOrigin{
		Skill:           skill,
		ParentClient:    client,
		ParentSessionID: sessionID,
		ParentAgent:     getenv("AI_AGENT"),
		Depth:           nextDelegateDepth(getenv("DELEGATE_DEPTH")),
	}
}

func nextDelegateDepth(parentDepth string) string {
	depth, err := strconv.Atoi(parentDepth)
	if parentDepth == "" || err != nil {
		return "1"
	}
	return strconv.Itoa(depth + 1)
}
