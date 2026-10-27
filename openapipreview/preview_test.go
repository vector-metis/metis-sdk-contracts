package openapipreview

import (
	"strings"
	"testing"
)

func TestParseBuildsStableOperationPreview(t *testing.T) {
	document := []byte(`openapi: 3.1.0
info:
  title: Pets
  description: Pet API
  version: "1.0.0"
servers:
  - url: https://api.example.test/v1
security:
  - bearerAuth: []
paths:
  /pets:
    get:
      operationId: listPets
      summary: List pets
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                type: array
    post:
      operationId: createPet
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
      responses:
        '201':
          description: created
components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
`)
	parsed, err := Parse(document, Options{})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsed.OpenAPIVersion != "3.1.0" || parsed.Title != "Pets" || parsed.BaseURL != "https://api.example.test/v1" || parsed.RawSHA256 == "" || parsed.ToolsSHA256 == "" {
		t.Fatalf("document = %#v", parsed)
	}
	if len(parsed.Operations) != 2 || parsed.Operations[0].Name != "listPets" || parsed.Operations[0].Risk != "read" || parsed.Operations[0].ApprovalRequired {
		t.Fatalf("operations = %#v", parsed.Operations)
	}
	if !parsed.Operations[1].RequestBody || parsed.Operations[1].Risk != "write" || !parsed.Operations[1].ApprovalRequired {
		t.Fatalf("write operation = %#v", parsed.Operations[1])
	}
	if len(parsed.Operations[0].Security) != 1 || parsed.Operations[0].Security[0] != "bearerAuth" {
		t.Fatalf("security = %#v", parsed.Operations[0].Security)
	}
}

func TestParseWarnsAndRejectsUnsafeDocuments(t *testing.T) {
	document := []byte(`{"openapi":"3.0.3","info":{"title":"Demo","version":"1"},"servers":[{"url":"https://api.example.test"}],"paths":{"/health":{"get":{"responses":{"200":{"description":"ok"}}}}}}`)
	_, err := Parse(document, Options{})
	if err == nil || !strings.Contains(err.Error(), "no selectable operations") {
		t.Fatalf("Parse() error = %v", err)
	}
	withOverride := []byte(`{"openapi":"3.0.3","info":{"title":"Demo","version":"1"},"servers":[{"url":"https://api.example.test"}],"paths":{"/health":{"get":{"operationId":"health","responses":{"200":{"description":"ok"}}}}}}`)
	if _, err := Parse(withOverride, Options{BaseURL: "https://api.example.test?secret=1"}); err == nil || !strings.Contains(err.Error(), "invalid OpenAPI base URL") {
		t.Fatalf("Parse() unsafe base URL error = %v", err)
	}
	external := []byte(`{"openapi":"3.0.3","info":{"title":"Demo","version":"1"},"servers":[{"url":"https://api.example.test"}],"components":{"schemas":{"Pet":{"$ref":"https://example.test/pet.json"}}},"paths":{"/pets":{"get":{"operationId":"listPets","responses":{"200":{"description":"ok"}}}}}}`)
	if _, err := Parse(external, Options{}); err == nil || !strings.Contains(err.Error(), "external reference") {
		t.Fatalf("Parse() external ref error = %v", err)
	}
}
