package wire

import "github.com/meschoening/silkpool/internal/diagnostics"

// AskDiagnostics is the Hub's ask for a connected node's diagnostics
// report, which the agent answers with a DiagnosticsReport of the same
// ID. The report is fixed, so the ask names nothing else: no file, path
// or part of it.
type AskDiagnostics struct{ ID string }

// DiagnosticsReport is an agent's answer to the AskDiagnostics with ID:
// its diagnostics report.
type DiagnosticsReport struct {
	ID     string
	Report diagnostics.Report
}

// The diagnostics messages' types.
const (
	// The Hub's.
	typeAskDiagnostics = "ask-diagnostics"
	// The agent's.
	typeDiagnosticsReport = "diagnostics-report"
)

type askDiagnosticsBody struct {
	ID string `json:"id"`
}

type diagnosticsReportBody struct {
	ID     string             `json:"id"`
	Report diagnostics.Report `json:"report"`
}
