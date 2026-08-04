// Package redaction removes credentials from values that may be persisted or logged.
package redaction

import "regexp"

var secretPattern = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{12,}|gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,}|(?:akia|asia)[0-9a-z]{16}|\d{6,}:[a-z0-9_-]{20,}|[a-z0-9_-]{20,}\.[a-z0-9_-]{20,}\.[a-z0-9_-]{20,})\b|(?i:aws_(?:secret_access_key|session_token)\s*[:=]\s*["']?)[a-z0-9/+=]{16,}`)

// Secrets replaces credential-like values with a safe placeholder.
func Secrets(value string) string { return secretPattern.ReplaceAllString(value, "[redacted]") }
