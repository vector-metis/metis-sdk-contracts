package contract_test

import (
	"bytes"
	"testing"

	contract "github.com/vector-metis/metis-sdk-contracts"
)

// TestValidateMPKRejectsComposeEnvironment 固化运行时环境必须通过 manifest 声明。
func TestValidateMPKRejectsComposeEnvironment(t *testing.T) {
	data := buildMPK(t, func(files map[string][]byte) {
		files["compose.amd64.yaml"] = []byte(`services:
  web:
    image: demo-a7x2m/web:1.0.0
    restart: unless-stopped
    environment:
      APP: ${METIS_APP_ID}
`)
	})
	if _, err := contract.ValidateMPK(bytes.NewReader(data), contract.ValidateOptions{ExpectedAppID: "demo-a7x2m"}); err == nil {
		t.Fatal("ValidateMPK() = nil, want compose environment rejection")
	}
}
