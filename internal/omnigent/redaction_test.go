package omnigent

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactingWriterHandlesSplitSecretsAndPartialFinalLine(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-VALUE"
	var output bytes.Buffer
	writer := newRedactingWriter(&output)
	for _, fragment := range []string{
		"starting token=SENTINEL-", "SECRET-VALUE\nbackend https://user:", sentinel,
		"@model.example/v1\nfinal bearer ", sentinel,
	} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), sentinel) || !strings.Contains(output.String(), "[redacted]") {
		t.Fatalf("redacted output = %q", output.String())
	}
}
