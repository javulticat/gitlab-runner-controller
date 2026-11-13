package provider

import "strings"

const MaxDiagnosticTokenBytes = 64

const minDiagnosticEnvValueBytes = 9

// Diagnostic contains public remote-call metadata, never raw errors or responses.
type Diagnostic struct {
	Operation  string `json:"operation"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Code       string `json:"code,omitempty"`
}

func (d *Diagnostic) Valid() bool {
	return d != nil && diagnosticToken(d.Operation) &&
		(d.Code == "" || diagnosticToken(d.Code)) &&
		(d.HTTPStatus == 0 || (d.HTTPStatus >= 100 && d.HTTPStatus <= 599))
}

func diagnosticToken(value string) bool {
	if len(value) == 0 || len(value) > MaxDiagnosticTokenBytes {
		return false
	}
	for i, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || (i > 0 && (c == '.' || c == '_' || c == '-')) {
			continue
		}
		return false
	}
	return true
}

// SanitizeDiagnostic omits invalid metadata and matching runner tokens or long environment values.
// Adapters must still construct diagnostics from public, fixed call names and codes.
func SanitizeDiagnostic(d *Diagnostic, environment []string, runnerToken string) *Diagnostic {
	if !d.Valid() {
		return nil
	}
	contains := func(value string) bool {
		return value != "" && (strings.Contains(d.Operation, value) || strings.Contains(d.Code, value))
	}
	if contains(runnerToken) {
		return nil
	}
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		// Short values often name modes or flags rather than credentials.
		if ok && key != "PATH" && key != "LANG" && len(value) >= minDiagnosticEnvValueBytes && contains(value) {
			return nil
		}
	}
	copy := *d
	return &copy
}
