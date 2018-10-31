package framework

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/helper/wrapping"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/version"
	"github.com/mitchellh/mapstructure"
)

// OpenAPI specification (OAS): https://github.com/OAI/OpenAPI-Specification/blob/master/versions/3.0.2.md
const OASVersion = "3.0.2"

// NewOASDocument returns an empty OpenAPI document.
func NewOASDocument() *OASDocument {
	return &OASDocument{
		Version: OASVersion,
		Info: oasInfo{
			Title:       "HashiCorp Vault API",
			Description: "HTTP API that gives you full access to Vault. All API routes are prefixed with `/v1/`.",
			Version:     version.GetVersion().Version,
			License: oasLicense{
				Name: "Mozilla Public License 2.0",
				URL:  "https://www.mozilla.org/en-US/MPL/2.0",
			},
		},
		Paths: make(map[string]*oasPathItem),
	}
}

type OASDocument struct {
	Version string                  `json:"openapi"`
	Info    oasInfo                 `json:"info"`
	Paths   map[string]*oasPathItem `json:"paths"`
}

type oasInfo struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Version     string     `json:"version"`
	License     oasLicense `json:"license"`
}

type oasLicense struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type oasPathItem struct {
	Description     string         `json:"description,omitempty"`
	Parameters      []oasParameter `json:"parameters,omitempty"`
	Sudo            bool           `json:"x-vault-sudo,omitempty" mapstructure:"x-vault-sudo"`
	Unauthenticated bool           `json:"x-vault-unauthenticated,omitempty" mapstructure:"x-vault-unauthenticated"`
	CreateSupported bool           `json:"x-vault-create-supported,omitempty" mapstructure:"x-vault-create-supported"`

	Get    *OASOperation `json:"get,omitempty"`
	Post   *OASOperation `json:"post,omitempty"`
	Delete *OASOperation `json:"delete,omitempty"`
}

// NewOASOperation creates an empty OpenAPI Operations object.
func NewOASOperation() *OASOperation {
	return &OASOperation{
		Responses: make(map[string]*oasResponse),
	}
}

type OASOperation struct {
	Summary     string                  `json:"summary,omitempty"`
	Description string                  `json:"description,omitempty"`
	Tags        []string                `json:"tags,omitempty"`
	Parameters  []oasParameter          `json:"parameters,omitempty"`
	RequestBody *oasRequestBody         `json:"requestBody,omitempty"`
	Responses   map[string]*oasResponse `json:"responses"`
	Deprecated  bool                    `json:"deprecated,omitempty"`
}

type oasParameter struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	In          string     `json:"in"`
	Schema      *oasSchema `json:"schema,omitempty"`
	Required    bool       `json:"required,omitempty"`
	Deprecated  bool       `json:"deprecated,omitempty"`
}

type oasRequestBody struct {
	Description string     `json:"description,omitempty"`
	Content     oasContent `json:"content,omitempty"`
}

type oasContent map[string]*oasMediaTypeObject

type oasMediaTypeObject struct {
	Schema *oasSchema `json:"schema,omitempty"`
}

type oasSchema struct {
	Type        string                `json:"type,omitempty"`
	Description string                `json:"description,omitempty"`
	Properties  map[string]*oasSchema `json:"properties,omitempty"`
	Items       *oasSchema            `json:"items,omitempty"`
	Format      string                `json:"format,omitempty"`
	Example     interface{}           `json:"example,omitempty"`
	Deprecated  bool                  `json:"deprecated,omitempty"`
}

type oasResponse struct {
	Description string     `json:"description"`
	Content     oasContent `json:"content,omitempty"`
}

var oasStdRespOK = &oasResponse{
	Description: "OK",
}

var oasStdRespNoContent = &oasResponse{
	Description: "empty body",
}

// Regex for handling optional and named parameters in paths, and string cleanup.
// Predefined here to avoid substantial recompilation.
var reqdRe = regexp.MustCompile(`\(?\?P<(\w+)>[^)]*\)?`) // Capture required parameters, e.g. "(?P<name>regex)"
var optRe = regexp.MustCompile(`(?U)\(.*\)\?`)           // Capture optional path elements in ungreedy (?U) fashion, e.g. "(leases/)?renew"
var altRe = regexp.MustCompile(`\((.*)\|(.*)\)`)         // Capture alternation elements, e.g. "(raw/?$|raw/(?P<path>.+))"
var pathFieldsRe = regexp.MustCompile(`{(\w+)}`)         // Capture OpenAPI-style named parameters, e.g. "lookup/{urltoken}",
var cleanCharsRe = regexp.MustCompile("[()^$?]")         // Set of regex characters that will be stripped during cleaning
var cleanSuffixRe = regexp.MustCompile(`/\?\$?$`)        // Path suffix patterns that will be stripped during cleaning
var wsRe = regexp.MustCompile(`\s+`)                     // Match whitespace, to be compressed during cleaning

// documentPaths parses all paths in a framework.Backend into OpenAPI paths.
func documentPaths(backend *Backend, doc *OASDocument) error {
	for _, p := range backend.Paths {
		if err := documentPath(p, backend.SpecialPaths(), backend.BackendType, doc); err != nil {
			return err
		}
	}

	return nil
}

// documentPath parses a framework.Path into one or more OpenAPI paths.
func documentPath(p *Path, specialPaths *logical.Paths, backendType logical.BackendType, doc *OASDocument) error {
	var sudoPaths []string
	var unauthPaths []string

	if specialPaths != nil {
		sudoPaths = specialPaths.Root
		unauthPaths = specialPaths.Unauthenticated
	}

	// Convert optional parameters into distinct patterns to be process independently.
	paths := expandPattern(p.Pattern)

	for _, path := range paths {
		// Construct a top level PathItem which will be populated as the path is processed.
		pi := oasPathItem{
			Description: cleanString(p.HelpSynopsis),
		}

		pi.Sudo = specialPathMatch(path, sudoPaths)
		pi.Unauthenticated = specialPathMatch(path, unauthPaths)

		// If the newer style Operations map isn't defined, create one from the legacy fields.
		operations := p.Operations
		if operations == nil {
			operations = make(map[logical.Operation]OperationHandler)

			for opType, cb := range p.Callbacks {
				operations[opType] = &PathOperation{
					Callback: cb,
					Summary:  p.HelpSynopsis,
				}
			}
		}

		// Process path and header parameters, which are common to all operations.
		// Body fields will be added to individual operations.
		pathFields, bodyFields := splitFields(p.Fields, path)

		for name, field := range pathFields {
			location := "path"
			required := true

			// Header parameters are part of the Parameters group but with
			// a dedicated "header" location, a header parameter is not required.
			if field.Type == TypeHeader {
				location = "header"
				required = false
			}

			t := convertType(field.Type)
			p := oasParameter{
				Name:        name,
				Description: cleanString(field.Description),
				In:          location,
				Schema:      &oasSchema{Type: t.baseType},
				Required:    required,
				Deprecated:  field.Deprecated,
			}
			pi.Parameters = append(pi.Parameters, p)
		}

		// Sort parameters for a stable output
		sort.Slice(pi.Parameters, func(i, j int) bool {
			return strings.ToLower(pi.Parameters[i].Name) < strings.ToLower(pi.Parameters[j].Name)
		})

		// Process each supported operation by building up an Operation object
		// with descriptions, properties and examples from the framework.Path data.
		for opType, opHandler := range operations {
			props := opHandler.Properties()
			if props.Unpublished {
				continue
			}

			if opType == logical.CreateOperation {
				pi.CreateSupported = true

				// If both Create and Update are defined, only process Update.
				if operations[logical.UpdateOperation] != nil {
					continue
				}
			}

			// If both List and Read are defined, only process Read.
			if opType == logical.ListOperation && operations[logical.ReadOperation] != nil {
				continue
			}

			op := NewOASOperation()

			op.Summary = props.Summary
			op.Description = props.Description
			op.Deprecated = props.Deprecated

			// Add any fields not present in the path as body parameters for POST.
			if opType == logical.CreateOperation || opType == logical.UpdateOperation {
				s := &oasSchema{
					Type:       "object",
					Properties: make(map[string]*oasSchema),
				}

				for name, field := range bodyFields {
					openapiField := convertType(field.Type)
					p := oasSchema{
						Type:        openapiField.baseType,
						Description: cleanString(field.Description),
						Format:      openapiField.format,
						Deprecated:  field.Deprecated,
					}
					if openapiField.baseType == "array" {
						p.Items = &oasSchema{
							Type: openapiField.items,
						}
					}
					s.Properties[name] = &p
				}

				// If examples were given, use the first one as the sample
				// of this schema.
				if len(props.Examples) > 0 {
					s.Example = props.Examples[0].Data
				}

				// Set the final request body. Only JSON request data is supported.
				if len(s.Properties) > 0 || s.Example != nil {
					op.RequestBody = &oasRequestBody{
						Content: oasContent{
							"application/json": &oasMediaTypeObject{
								Schema: s,
							},
						},
					}
				}
			}

			// LIST is represented as GET with a `list` query parameter
			if opType == logical.ListOperation || (opType == logical.ReadOperation && operations[logical.ListOperation] != nil) {
				op.Parameters = append(op.Parameters, oasParameter{
					Name:        "list",
					Description: "Return a list if `true`",
					In:          "query",
					Schema:      &oasSchema{Type: "string"},
				})
			}

			// Add tags based on backend type
			var tags []string
			switch backendType {
			case logical.TypeLogical:
				tags = []string{"secrets"}
			case logical.TypeCredential:
				tags = []string{"auth"}
			}

			op.Tags = append(op.Tags, tags...)

			// Set default responses.
			if len(props.Responses) == 0 {
				if opType == logical.DeleteOperation {
					op.Responses["204"] = oasStdRespNoContent
				} else {
					op.Responses["200"] = oasStdRespOK
				}
			}

			// Add any defined response details.
			for code, responses := range props.Responses {
				var description string
				content := make(oasContent)

				for i, resp := range responses {
					if i == 0 {
						description = resp.Description
					}
					if resp.Example != nil {
						mediaType := resp.MediaType
						if mediaType == "" {
							mediaType = "application/json"
						}

						// create a version of the response that will not emit null items
						cr, err := cleanResponse(resp.Example)
						if err != nil {
							return err
						}

						// Only one example per media type is allowed, so first one wins
						if _, ok := content[mediaType]; !ok {
							content[mediaType] = &oasMediaTypeObject{
								Schema: &oasSchema{
									Example: cr,
								},
							}
						}
					}
				}

				op.Responses[code] = &oasResponse{
					Description: description,
					Content:     content,
				}
			}

			switch opType {
			case logical.CreateOperation, logical.UpdateOperation:
				pi.Post = op
			case logical.ReadOperation, logical.ListOperation:
				pi.Get = op
			case logical.DeleteOperation:
				pi.Delete = op
			}
		}

		doc.Paths["/"+path] = &pi
	}

	return nil
}

func specialPathMatch(path string, specialPaths []string) bool {
	// Test for exact or prefix match of special paths.
	for _, sp := range specialPaths {
		if sp == path ||
			(strings.HasSuffix(sp, "*") && strings.HasPrefix(path, sp[0:len(sp)-1])) {
			return true
		}
	}
	return false
}

// expandPattern expands a regex pattern by generating permutations of any optional parameters
// and changing named parameters into their {openapi} equivalents.
func expandPattern(pattern string) []string {
	var paths []string

	// GenericNameRegex adds a regex that complicates our parsing. It is much easier to
	// detect and remove it now than to compensate for in the other regexes.
	//
	// example: (?P<foo>\\w(([\\w-.]+)?\\w)?) -> (?P<foo>)
	base := GenericNameRegex("")
	start := strings.Index(base, ">")
	end := strings.LastIndex(base, ")")
	regexToRemove := ""
	if start != -1 && end != -1 && end > start {
		regexToRemove = base[start+1 : end]
	}

	pattern = strings.Replace(pattern, regexToRemove, "", -1)

	// Initialize paths with the original pattern or the halves of an
	// alternation, which is also present in some patterns.
	matches := altRe.FindAllStringSubmatch(pattern, -1)
	if len(matches) > 0 {
		paths = []string{matches[0][1], matches[0][2]}
	} else {
		paths = []string{pattern}
	}

	// Expand all optional regex elements into two paths. This approach is really only useful up to 2 optional
	// groups, but we probably don't want to deal with the exponential increase beyond that anyway.
	for i := 0; i < len(paths); i++ {
		p := paths[i]

		// match is a 2-element slice that will have a start and end index
		// for the left-most match of a regex of form: (lease/)?
		match := optRe.FindStringIndex(p)

		if match != nil {
			// create a path that includes the optional element but without
			// parenthesis or the '?' character.
			paths[i] = p[:match[0]] + p[match[0]+1:match[1]-2] + p[match[1]:]

			// create a path that excludes the optional element.
			paths = append(paths, p[:match[0]]+p[match[1]:])
			i--
		}
	}

	// Replace named parameters (?P<foo>) with {foo}
	var replacedPaths []string

	for _, path := range paths {
		result := reqdRe.FindAllStringSubmatch(path, -1)
		if result != nil {
			for _, p := range result {
				par := p[1]
				path = strings.Replace(path, p[0], fmt.Sprintf("{%s}", par), 1)
			}
		}
		// Final cleanup
		path = cleanSuffixRe.ReplaceAllString(path, "")
		path = cleanCharsRe.ReplaceAllString(path, "")
		replacedPaths = append(replacedPaths, path)
	}

	return replacedPaths
}

// schemaType is a subset of the JSON Schema elements used as a target
// for conversions from Vault's standard FieldTypes.
type schemaType struct {
	baseType string
	items    string
	format   string
}

// convertType translates a FieldType into an OpenAPI type.
// In the case of arrays, a subtype is returned as well.
func convertType(t FieldType) schemaType {
	ret := schemaType{}

	switch t {
	case TypeString, TypeNameString, TypeHeader:
		ret.baseType = "string"
	case TypeLowerCaseString:
		ret.baseType = "string"
		ret.format = "lowercase"
	case TypeInt:
		ret.baseType = "number"
	case TypeDurationSecond:
		ret.baseType = "number"
		ret.format = "seconds"
	case TypeBool:
		ret.baseType = "boolean"
	case TypeMap:
		ret.baseType = "object"
		ret.format = "map"
	case TypeKVPairs:
		ret.baseType = "object"
		ret.format = "kvpairs"
	case TypeSlice:
		ret.baseType = "array"
		ret.items = "object"
	case TypeStringSlice, TypeCommaStringSlice:
		ret.baseType = "array"
		ret.items = "string"
	case TypeCommaIntSlice:
		ret.baseType = "array"
		ret.items = "number"
	default:
		log.L().Warn("error parsing field type", "type", t)
		ret.format = "unknown"
	}

	return ret
}

// cleanString prepares s for inclusion in the output
func cleanString(s string) string {
	// clean leading/trailing whitespace, and replace whitespace runs into a single space
	s = strings.TrimSpace(s)
	s = wsRe.ReplaceAllString(s, " ")
	return s
}

// splitFields partitions fields into path and body groups
// The input pattern is expected to have been run through expandPattern,
// with paths parameters denotes in {braces}.
func splitFields(allFields map[string]*FieldSchema, pattern string) (pathFields, bodyFields map[string]*FieldSchema) {
	pathFields = make(map[string]*FieldSchema)
	bodyFields = make(map[string]*FieldSchema)

	for _, match := range pathFieldsRe.FindAllStringSubmatch(pattern, -1) {
		name := match[1]
		pathFields[name] = allFields[name]
	}

	for name, field := range allFields {
		if _, ok := pathFields[name]; !ok {
			// Header fields are in "parameters" with other path fields
			if field.Type == TypeHeader {
				pathFields[name] = field
			} else {
				bodyFields[name] = field
			}
		}
	}

	return pathFields, bodyFields
}

// cleanedResponse is identical to logical.Response but with nulls
// removed from from JSON encoding
type cleanedResponse struct {
	Secret   *logical.Secret            `json:"secret,omitempty"`
	Auth     *logical.Auth              `json:"auth,omitempty"`
	Data     map[string]interface{}     `json:"data,omitempty"`
	Redirect string                     `json:"redirect,omitempty"`
	Warnings []string                   `json:"warnings,omitempty"`
	WrapInfo *wrapping.ResponseWrapInfo `json:"wrap_info,omitempty"`
}

func cleanResponse(resp *logical.Response) (*cleanedResponse, error) {
	var r cleanedResponse

	if err := mapstructure.Decode(resp, &r); err != nil {
		return nil, err
	}

	return &r, nil
}
