package redaction

import (
	"strings"
	"testing"
)

func TestSecretsRedactsAWSStaticCredentials(t *testing.T) {
	value := "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	got := Secrets(value)
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") || strings.Contains(got, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") {
		t.Fatalf("AWS credentials were not redacted: %q", got)
	}
}
