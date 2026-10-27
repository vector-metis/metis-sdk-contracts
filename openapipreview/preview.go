// Package openapipreview provides the offline, side-effect-free OpenAPI preview
// used by the developer CLI. The platform owns the complete virtual-MCP
// converter; this package deliberately returns only stable review facts.
package openapipreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

const (
	MaxDocumentSize = 8 << 20
	MaxOperations   = 512
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Options controls the non-network preview. BaseURL overrides servers[0].url;
// SelectedOperationIDs limits the preview to operation IDs in the set.
type Options struct {
	BaseURL              string
	SelectedOperationIDs map[string]bool
}

// Document is a stable preview projection. It contains no credentials or
// request/response bodies and is safe to persist in CI artifacts.
type Document struct {
	OpenAPIVersion string      `json:"openapiVersion"`
	Title          string      `json:"title,omitempty"`
	Description    string      `json:"description,omitempty"`
	BaseURL        string      `json:"baseUrl"`
	RawSHA256      string      `json:"rawSha256"`
	ToolsSHA256    string      `json:"toolsSha256"`
	Operations     []Operation `json:"operations"`
	Warnings       []string    `json:"warnings,omitempty"`
}

// Operation is the review-level mapping fact used before platform import.
type Operation struct {
	Name             string   `json:"name"`
	OperationID      string   `json:"operationId,omitempty"`
	Method           string   `json:"method"`
	Path             string   `json:"path"`
	Description      string   `json:"description,omitempty"`
	Risk             string   `json:"risk"`
	ApprovalRequired bool     `json:"approvalRequired"`
	Security         []string `json:"security,omitempty"`
	RequestBody      bool     `json:"requestBody"`
	ResponseMedia    []string `json:"responseMedia,omitempty"`
}

// Parse validates a self-contained OpenAPI 3.0/3.1 document without network
// access and returns a deterministic preview. It never follows external refs.
func Parse(data []byte, options Options) (Document, error) {
	if len(data) == 0 {
		return Document{}, errors.New("OpenAPI document is empty")
	}
	if len(data) > MaxDocumentSize {
		return Document{}, fmt.Errorf("OpenAPI document exceeds %d bytes", MaxDocumentSize)
	}
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return Document{}, fmt.Errorf("load OpenAPI document: %w", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.0.") && doc.OpenAPI != "3.0" && !strings.HasPrefix(doc.OpenAPI, "3.1.") && doc.OpenAPI != "3.1" {
		return Document{}, fmt.Errorf("unsupported OpenAPI version %q; only 3.0 and 3.1 are supported", doc.OpenAPI)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return Document{}, fmt.Errorf("validate OpenAPI document: %w", err)
	}
	baseURL, err := chooseBaseURL(doc, options.BaseURL)
	if err != nil {
		return Document{}, err
	}
	result := Document{
		OpenAPIVersion: doc.OpenAPI,
		BaseURL:        baseURL,
		RawSHA256:      digest(data),
	}
	if doc.Info != nil {
		result.Title = strings.TrimSpace(doc.Info.Title)
		result.Description = strings.TrimSpace(doc.Info.Description)
	}
	if doc.Paths == nil {
		return Document{}, errors.New("OpenAPI document must define paths")
	}
	for _, pathName := range doc.Paths.Keys() {
		pathItem := doc.Paths.Value(pathName)
		if pathItem == nil {
			continue
		}
		for _, method := range sortedMethods(pathItem.Operations()) {
			op := pathItem.Operations()[method]
			if op == nil {
				continue
			}
			operationID := strings.TrimSpace(op.OperationID)
			if len(options.SelectedOperationIDs) > 0 && (operationID == "" || !options.SelectedOperationIDs[operationID]) {
				continue
			}
			if operationID == "" {
				result.Warnings = append(result.Warnings, fmt.Sprintf("operation %s %s has no operationId and was skipped", method, pathName))
				continue
			}
			if !toolNamePattern.MatchString(operationID) {
				return Document{}, fmt.Errorf("operation %s %s has invalid tool name %q", method, pathName, operationID)
			}
			risk, approval := operationRisk(op, method)
			result.Operations = append(result.Operations, Operation{
				Name: operationID, OperationID: operationID, Method: strings.ToUpper(method), Path: pathName,
				Description: operationDescription(op), Risk: risk, ApprovalRequired: approval,
				Security: securityNames(doc, op), RequestBody: op.RequestBody != nil,
				ResponseMedia: responseMediaTypes(op),
			})
		}
	}
	if len(result.Operations) == 0 {
		return Document{}, errors.New("OpenAPI document contains no selectable operations with operationId")
	}
	if len(result.Operations) > MaxOperations {
		return Document{}, fmt.Errorf("OpenAPI document contains %d operations; maximum is %d", len(result.Operations), MaxOperations)
	}
	result.ToolsSHA256 = digestJSON(result.Operations)
	return result, nil
}

func chooseBaseURL(doc *openapi3.T, override string) (string, error) {
	value := strings.TrimSpace(override)
	if value == "" && len(doc.Servers) > 0 && doc.Servers[0] != nil {
		value = strings.TrimSpace(doc.Servers[0].URL)
	}
	if value == "" {
		return "", errors.New("OpenAPI document must define servers[0].url or an explicit base URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid OpenAPI base URL %q", value)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func sortedMethods(operations map[string]*openapi3.Operation) []string {
	methods := make([]string, 0, len(operations))
	for method := range operations {
		methods = append(methods, strings.ToUpper(method))
	}
	sort.Strings(methods)
	return methods
}

func operationDescription(operation *openapi3.Operation) string {
	if value := strings.TrimSpace(operation.Description); value != "" {
		return value
	}
	return strings.TrimSpace(operation.Summary)
}

func operationRisk(operation *openapi3.Operation, method string) (string, bool) {
	risk, approval := "write", true
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		risk, approval = "read", false
	}
	if operation.Deprecated {
		approval = true
	}
	if extension, ok := operation.Extensions["x-metis-mcp"].(map[string]any); ok {
		if value, ok := extension["risk"].(string); ok && (value == "read" || value == "write") {
			risk = value
		}
		if value, ok := extension["approval_required"].(bool); ok {
			approval = value
		}
	}
	return risk, approval
}

func securityNames(doc *openapi3.T, operation *openapi3.Operation) []string {
	security := doc.Security
	if operation.Security != nil {
		security = *operation.Security
	}
	set := make(map[string]struct{})
	for _, requirement := range security {
		for name := range requirement {
			set[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func responseMediaTypes(operation *openapi3.Operation) []string {
	set := make(map[string]struct{})
	if operation.Responses == nil {
		return nil
	}
	for _, response := range operation.Responses.Map() {
		if response == nil || response.Value == nil {
			continue
		}
		for mediaType := range response.Value.Content {
			set[mediaType] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for mediaType := range set {
		result = append(result, mediaType)
	}
	sort.Strings(result)
	return result
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return digest(encoded)
}
